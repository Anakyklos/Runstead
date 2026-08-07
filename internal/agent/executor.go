package agent

import (
	"context"
	"errors"
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

// Executor is the only agent-facing provider execution seam. The provider is
// retained privately and every attempt is routed through the governor. It
// owns one explicit event-drain step after each execution; no background
// dispatcher is required to deliver the governor's mandatory events.
type Executor struct {
	governor   *governor.Governor
	provider   provider.Client
	classifier governor.OutcomeClassifier
}

func NewExecutor(accountGovernor *governor.Governor, client provider.Client, classifier governor.OutcomeClassifier) (*Executor, error) {
	if accountGovernor == nil || client == nil {
		return nil, ErrExecutorUnavailable
	}
	return &Executor{governor: accountGovernor, provider: client, classifier: classifier}, nil
}

func (e *Executor) Execute(ctx context.Context, request governor.AttemptRequest) governor.ExecutionResult {
	if e == nil || e.governor == nil || e.provider == nil {
		return governor.ExecutionResult{Err: ErrExecutorUnavailable}
	}
	result := e.governor.Execute(ctx, request, e.provider, e.classifier)
	e.governor.DrainEvents()
	return result
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
