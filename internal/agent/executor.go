package agent

// Issue #92: bounded, governor-owned retry orchestration. Retry scheduling
// lives ABOVE the adapters, inside this agent-facing frontier: every
// retried physical attempt re-enters Governor.Execute, gets its own
// admission, its own debit/accounting and its own durable evidence. One
// provider.Client.Complete call still represents at most one physical
// upstream request; adapters never retry. The governor remains the ONLY
// authority for admission, budgets, cooldown, circuits and retry
// eligibility (FinishResult.RetryEligible).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

var ErrExecutorUnavailable = errors.New("agent executor is unavailable")

// AttemptRunner is the loop-facing provider boundary implemented by Executor.
// The compile-time assertion below guarantees the executor satisfies it; the
// provider.Client is retained privately so the loop can never bypass account
// admission.
var _ AttemptRunner = (*Executor)(nil)

// ExecutorOptions configures the process-local retry orchestration. The
// zero value keeps the historical behavior: exactly one governed attempt per
// Execute, no retry (EnableRetry defaults to false, so existing workloads
// never gain implicit retries).
type ExecutorOptions struct {
	// EnableRetry turns on the bounded governor-owned retry orchestration.
	// It is an explicit operator choice; retries remain bounded by the
	// governor's retry/task/elapsed budgets and circuit/cooldown safety.
	EnableRetry bool
	// Clock drives the retry backoff wait. It MUST be the same Clock the
	// governor uses so fake-clock tests control both admission pacing and
	// the retry wait deterministically. Nil uses a real clock.
	Clock governor.Clock
	// RetryProfileCooldown, when non-nil, supplies the effective
	// cooldown_millis input of the durable profile record (#91) for the
	// identity being executed (0 when the profile has no applicable value).
	// The profile is an INPUT only; it never executes retries and never
	// changes governor authority.
	RetryProfileCooldown func() time.Duration
}

// Executor is the only agent-facing provider execution seam. The provider is
// retained privately and every attempt is routed through the governor. It
// owns one explicit event-drain step after each execution; no background
// dispatcher is required to deliver the governor's mandatory events.
type Executor struct {
	governor   *governor.Governor
	provider   provider.Client
	classifier governor.OutcomeClassifier
	clock      governor.Clock
	profile    func() time.Duration
	retry      bool
}

// NewExecutor builds the agent-facing provider frontier. classifier may be
// nil (the governor's conservative default classification applies). options
// may be omitted entirely; the zero value disables retry orchestration so
// existing workloads never gain implicit retries.
func NewExecutor(accountGovernor *governor.Governor, client provider.Client, classifier governor.OutcomeClassifier, options ...ExecutorOptions) (*Executor, error) {
	if accountGovernor == nil || client == nil {
		return nil, ErrExecutorUnavailable
	}
	var opts ExecutorOptions
	if len(options) > 0 {
		opts = options[0]
	}
	clock := opts.Clock
	if clock == nil {
		clock = executorRealClock{}
	}
	return &Executor{governor: accountGovernor, provider: client, classifier: classifier, clock: clock, profile: opts.RetryProfileCooldown, retry: opts.EnableRetry}, nil
}

// Execute runs one logical governed attempt. When the governor's
// FinishResult marks the completed attempt retry-eligible (rate/capacity or
// other delivery-safe recoverable classes, retry budget remaining, circuit
// closed), the executor performs a bounded, cancellable backoff wait and
// then issues a NEW Governor.Execute: a new admission, a new debit and a new
// durable physical attempt. Retry stops when eligibility, budgets, circuit,
// delivery safety or the context says so.
func (e *Executor) Execute(ctx context.Context, request governor.AttemptRequest) governor.ExecutionResult {
	if e == nil || e.governor == nil || e.provider == nil {
		return governor.ExecutionResult{Err: ErrExecutorUnavailable}
	}
	attempt := 0
	for {
		attempt++
		attemptRequest := request
		if attempt > 1 {
			// Each retry is a distinct governed attempt with its own client
			// request id (duplicate detection and per-attempt evidence).
			attemptRequest.ClientRequestID = fmt.Sprintf("%s-r%d", request.ClientRequestID, attempt-1)
			attemptRequest.Retry = true
		}
		result := e.governor.Execute(ctx, attemptRequest, e.provider, e.classifier)
		e.governor.DrainEvents()
		if !e.retry || !e.retryEligible(ctx, result) {
			return result
		}
		// Bounded wait BEFORE the next admission: no attempt budget is
		// reserved and no physical request is issued by waiting. A cancel or
		// deadline during the wait returns the ORIGINAL attempt result; the
		// next attempt never starts and is never debited.
		if err := e.retryBackoff(ctx, result.Completion); err != nil {
			return result
		}
		if ctx.Err() != nil {
			return result
		}
	}
}

// retryEligible is the executor-side gate that keeps retries bounded and
// safe: the governor already computed eligibility (recoverable class +
// delivery evidence + retry budget + circuit closed); the executor adds the
// context gate and refuses to retry failed/persistently-errored completions.
//
// A failed durable finish (TX 2: RecordProviderFinished) after an otherwise
// retryable outcome leaves the physical attempt only 'prepared'/ambiguous in
// the store: the effect was observed but its classified outcome was never
// durably recorded. Automatic retry must not issue another physical attempt
// while the previous effect lacks a completed durable outcome, so the
// persistence failure turns the loop off even though the computed completion
// was still marked retry-eligible. Typed adapter failures (429/503) also
// ride on ExecutionResult.Err and must remain retryable, so only the
// persistence sentinel is excluded.
func (e *Executor) retryEligible(ctx context.Context, result governor.ExecutionResult) bool {
	if result.Completion.Err != nil {
		return false
	}
	if errors.Is(result.Err, governor.ErrProviderOutcomePersist) {
		return false
	}
	if !result.Completion.RetryEligible {
		return false
	}
	return ctx.Err() == nil
}

// retryBackoff waits for the bounded backoff before the next admission:
//
//  1. the governor-selected backoff (authoritative Retry-After / reset when
//     observed, jittered circuit baseline otherwise);
//  2. the effective cooldown of the durable profile record (#91) when it
//     is larger (input only);
//  3. the governor's own cooldown window remaining (snapshot) when larger.
//
// The wait is cancellable, never reserves a budget and never leaks timers
// (the timer is always stopped).
func (e *Executor) retryBackoff(ctx context.Context, completion governor.FinishResult) error {
	base := completion.SelectedBackoff
	if base < 0 {
		base = 0
	}
	if e.profile != nil {
		if profile := e.profile(); profile > base {
			base = profile
		}
	}
	if cooldown := e.governor.Snapshot().CooldownUntil; !cooldown.IsZero() {
		if remaining := cooldown.Sub(e.clock.Now()); remaining > base {
			base = remaining
		}
	}
	if base <= 0 {
		return nil
	}
	timer := e.clock.NewTimer(base)
	defer timer.Stop()
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AccountPressure reports whether the account lane was delaying or blocking
// admission at the given time: an occupied lane, queue, cooldown, open circuit,
// upstream rate/capacity state or exhausted rolling/task budgets.
func (e *Executor) AccountPressure(now time.Time) bool {
	if e == nil || e.governor == nil {
		return false
	}
	snapshot := e.governor.Snapshot()
	if snapshot.QueueLength > 0 || snapshot.InFlight {
		return true
	}
	if !snapshot.CooldownUntil.IsZero() && snapshot.CooldownUntil.After(now) {
		return true
	}
	if snapshot.Circuit.State != governor.CircuitClosed {
		return true
	}
	if snapshot.Telemetry.RateLimited || snapshot.Telemetry.CapacityExhausted {
		return true
	}
	config := e.governor.Config()
	if !snapshot.LastStart.IsZero() && snapshot.LastStart.Add(config.MinimumStartInterval).After(now) {
		return true
	}
	budgets := snapshot.Budgets
	if budgets.TaskCeiling > 0 && budgets.TaskUsed >= budgets.TaskCeiling {
		return true
	}
	if budgets.Rolling3hCeiling > 0 && budgets.Rolling3hUsed >= budgets.Rolling3hCeiling {
		return true
	}
	if budgets.Rolling1hCeiling > 0 && budgets.Rolling1hUsed >= budgets.Rolling1hCeiling {
		return true
	}
	if budgets.Rolling10mCeiling > 0 && budgets.Rolling10mUsed >= budgets.Rolling10mCeiling {
		return true
	}
	return false
}

// executorRealClock is the real-time default for the retry backoff when no
// Clock is injected.
type executorRealClock struct{}

func (executorRealClock) Now() time.Time { return time.Now() }

type executorRealTimer struct{ timer *time.Timer }

func (t executorRealTimer) C() <-chan time.Time { return t.timer.C }
func (t executorRealTimer) Stop() bool          { return t.timer.Stop() }

func (executorRealClock) NewTimer(delay time.Duration) governor.Timer {
	return executorRealTimer{timer: time.NewTimer(delay)}
}
