package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Clock is the loop's time seam. Tests share one clock between the loop and
// the account governor so task deadlines and trace durations stay
// deterministic.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// AttemptRunner is the only provider access the loop holds. It is the
// governor-owned attempt executor boundary: every model turn passes through
// account admission, and provider.Client is never visible to the loop.
type AttemptRunner interface {
	Execute(context.Context, governor.AttemptRequest) governor.ExecutionResult
	// AccountPressure reports whether the account lane was busy, cooling down,
	// circuit-blocked or budget-exhausted at the given time. The loop uses it
	// only to distinguish account delay from plain time-budget exhaustion when
	// a task deadline fires while admission is pending.
	AccountPressure(now time.Time) bool
}

// Limits bound one loop run. Defaults are conservative; the account governor
// enforces its own rolling, task and retry budgets below this layer.
type Limits struct {
	MaxSteps           int           // total model turns
	MaxCorrections     int           // protocol correction attempts
	MaxRepeatedActions int           // repeated-action corrections before stopping
	TimeBudget         time.Duration // elapsed task time
	ProviderBudget     int           // governed provider attempts per task
}

func DefaultLimits() Limits {
	return Limits{
		MaxSteps:           24,
		MaxCorrections:     2,
		MaxRepeatedActions: 2,
		TimeBudget:         10 * time.Minute,
		ProviderBudget:     80,
	}
}

// Task is one loop run: a prompt answered against a read-only workspace.
type Task struct {
	ID     string
	Prompt string
}

// Loop is the bounded read-only agent loop. It owns no provider client, no
// writes, no shell and no persistence; it only coordinates the existing
// governor executor, protocol parser, repeat guard and tool registry.
type Loop struct {
	runner   AttemptRunner
	registry *tools.Registry
	contract string
	limits   Limits
	clock    Clock
	trace    TraceSink
	model    string
}

// Config wires one loop instance at the composition root.
type Config struct {
	Runner   AttemptRunner
	Registry *tools.Registry
	Limits   Limits
	Clock    Clock
	Trace    TraceSink
	Model    string
}

func NewLoop(config Config) (*Loop, error) {
	if config.Runner == nil {
		return nil, fmt.Errorf("agent loop requires the governor attempt runner")
	}
	if config.Registry == nil {
		return nil, fmt.Errorf("agent loop requires the read-only tool registry")
	}
	contract, err := BuildSystemContract(config.Registry)
	if err != nil {
		return nil, err
	}
	limits := config.Limits
	if limits.MaxSteps <= 0 {
		limits.MaxSteps = DefaultLimits().MaxSteps
	}
	if limits.MaxCorrections <= 0 {
		limits.MaxCorrections = DefaultLimits().MaxCorrections
	}
	if limits.MaxRepeatedActions <= 0 {
		limits.MaxRepeatedActions = DefaultLimits().MaxRepeatedActions
	}
	if limits.TimeBudget <= 0 {
		limits.TimeBudget = DefaultLimits().TimeBudget
	}
	if limits.ProviderBudget <= 0 {
		limits.ProviderBudget = DefaultLimits().ProviderBudget
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	traceSink := config.Trace
	if traceSink == nil {
		traceSink = nopTrace
	}
	return &Loop{
		runner:   config.Runner,
		registry: config.Registry,
		contract: contract,
		limits:   limits,
		clock:    clock,
		trace:    traceSink,
		model:    strings.TrimSpace(config.Model),
	}, nil
}

// Run executes the bounded loop until a terminal outcome. The context is the
// one-shot cancellation signal propagated through governor admission, provider
// I/O and tool execution; it is never reset or reused. When a time budget is
// configured the deadline is attached to the propagated context so the
// governor can enforce it during admission.
func (l *Loop) Run(ctx context.Context, task Task) Result {
	startedAt := l.clock.Now()
	deadline := time.Time{}
	if l.limits.TimeBudget > 0 {
		deadline = startedAt.Add(l.limits.TimeBudget)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	transcript := newTranscript(l.contract, task.Prompt)
	evidence := NewEvidenceSet()
	guard := newRepeatGuard()

	state := runState{}
	emit := func(line TraceLine) {
		state.sequence++
		line.Sequence = state.sequence
		l.trace(line)
	}
	stop := func(outcome Outcome, reason string, extra func(*Result)) Result {
		result := Result{
			Outcome:      outcome,
			StopReason:   reason,
			Turns:        state.turns,
			Attempts:     state.attempts,
			Observations: evidence.Count(),
			Corrections:  state.corrections,
			Repeated:     state.repeated,
			MixedProse:   state.mixedProse,
		}
		if extra != nil {
			extra(&result)
		}
		emit(TraceLine{Kind: TraceStop, Status: string(outcome), StopReason: reason})
		return result
	}

	for {
		if err := ctx.Err(); err != nil {
			if err == context.Canceled {
				return stop(OutcomeCanceled, outcomeReasonCanceled(err), nil)
			}
			return stop(OutcomeTimeBudgetExhausted, OutcomeTimeBudgetExhausted.StopReason(), nil)
		}
		if deadlineReached(l.clock.Now(), deadline) {
			return stop(OutcomeTimeBudgetExhausted, OutcomeTimeBudgetExhausted.StopReason(), nil)
		}
		if state.turns >= l.limits.MaxSteps {
			return stop(OutcomeStepsExhausted, OutcomeStepsExhausted.StopReason(), nil)
		}
		if state.attempts >= l.limits.ProviderBudget {
			return stop(OutcomeProviderBudgetExhausted, OutcomeProviderBudgetExhausted.StopReason(), nil)
		}

		if turn, terminal := l.modelTurn(ctx, task, transcript, evidence, guard, deadline, &state, emit, stop); terminal {
			return turn
		}
	}
}

type runState struct {
	sequence    int
	turns       int
	attempts    int
	corrections int
	repeated    int
	mixedProse  int
}

func outcomeReasonCanceled(err error) string {
	if err == nil {
		return OutcomeCanceled.StopReason()
	}
	return fmt.Sprintf("%s: %v", OutcomeCanceled.StopReason(), err)
}

func deadlineReached(now time.Time, deadline time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}

// modelTurn runs one governed model attempt and reports whether the run must
// stop. Every attempt is counted against the task budgets before admission, so
// no correction or retry can escape the governor.
func (l *Loop) modelTurn(
	ctx context.Context,
	task Task,
	transcript *transcript,
	evidence *EvidenceSet,
	guard *repeatGuard,
	deadline time.Time,
	state *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	attemptStart := l.clock.Now()
	state.turns++
	state.attempts++
	requestID := fmt.Sprintf("%s-%04d", task.ID, state.turns)
	// Account pressure is sampled before admission: when the task deadline
	// fires while the governor is delaying the request, the delay was caused
	// by the account lane (pacing, cooldown, circuit, busy lane or budgets).
	pressure := l.runner.AccountPressure(l.clock.Now())

	execution := l.runner.Execute(ctx, governor.AttemptRequest{
		TaskID:          task.ID,
		ClientRequestID: requestID,
		ProviderRequest: provider.Request{
			Protocol: protocol.Current,
			Prompt:   transcript.render(),
			Model:    l.model,
		},
	})
	attemptDuration := l.clock.Now().Sub(attemptStart)

	admissionStatus := "admitted"
	if !execution.Admission.Admitted() {
		admissionStatus = string(execution.Admission.Code)
	}
	emit(TraceLine{Kind: TraceAttempt, Status: admissionStatus, Duration: attemptDuration, Classification: string(execution.Completion.Outcome)})

	if !execution.Admission.Admitted() {
		return l.classifyAdmission(execution.Admission, pressure, emit, stop), true
	}
	if execution.Err != nil {
		return stopOutcomeForContext(ctx, l.clock.Now(), deadline,
			func(result *Result) {
				result.Classification = string(execution.Completion.Outcome)
				result.StopReason = providerFailureReason(execution.Completion.Outcome, execution.Err)
				result.Err = execution.Err
			}, stop), true
	}
	if strings.TrimSpace(execution.Response.Text) == "" {
		return stop(OutcomeProviderFailure, "provider failure: empty_response", func(result *Result) {
			result.Classification = "empty_response"
		}), true
	}

	text := execution.Response.Text
	transcript.assistant(text)
	parse := protocol.Parse(text, l.registry)
	if parse.MixedProse {
		state.mixedProse++
		emit(TraceLine{Kind: TraceDeviation, Status: "mixed_prose"})
	}
	if parse.Failure != nil {
		return l.handleParseFailure(parse.Failure, transcript, state, emit, stop)
	}

	switch parse.Kind {
	case protocol.KindAction:
		return l.handleAction(ctx, parse.Action, transcript, evidence, guard, state, emit, stop)
	case protocol.KindFinal:
		return l.handleFinal(parse.Final, evidence, state, emit, stop)
	default:
		return stop(OutcomeProviderFailure, "provider failure: unrecognized envelope kind", nil), true
	}
}

func providerFailureReason(classification governor.OutcomeClass, err error) string {
	if classification != "" {
		return fmt.Sprintf("provider failure: %s", classification)
	}
	return fmt.Sprintf("provider failure: %v", err)
}

func stopOutcomeForContext(
	ctx context.Context,
	now time.Time,
	deadline time.Time,
	extra func(*Result),
	stop func(Outcome, string, func(*Result)) Result,
) Result {
	if ctx.Err() == context.Canceled {
		return stop(OutcomeCanceled, outcomeReasonCanceled(ctx.Err()), extra)
	}
	if deadlineReached(now, deadline) {
		return stop(OutcomeTimeBudgetExhausted, OutcomeTimeBudgetExhausted.StopReason(), extra)
	}
	return stop(OutcomeProviderFailure, OutcomeProviderFailure.StopReason(), extra)
}

func (l *Loop) classifyAdmission(
	admission governor.AdmissionResult,
	pressure bool,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) Result {
	switch admission.Code {
	case governor.AdmissionContextCancelled:
		return stop(OutcomeCanceled, outcomeReasonCanceled(admission.Err), nil)
	case governor.AdmissionTaskDeadlineExceeded:
		if pressure {
			return stop(OutcomeAccountDelayTimeout, OutcomeAccountDelayTimeout.StopReason(), nil)
		}
		return stop(OutcomeTimeBudgetExhausted, OutcomeTimeBudgetExhausted.StopReason(), nil)
	case governor.AdmissionTaskBudgetExhausted,
		governor.AdmissionRollingBudgetExhausted,
		governor.AdmissionUpstreamAllowanceExhausted,
		governor.AdmissionRetryBudgetExhausted:
		return stop(OutcomeProviderBudgetExhausted, OutcomeProviderBudgetExhausted.StopReason(), nil)
	case governor.AdmissionCircuitOpen,
		governor.AdmissionHumanAcknowledgementRequired,
		governor.AdmissionAuthenticationRefreshRequired:
		return stop(OutcomeAccountCircuitOpen, OutcomeAccountCircuitOpen.StopReason(), nil)
	case governor.AdmissionDelayed, governor.AdmissionCooldownActive:
		return stop(OutcomeAccountDelayTimeout, OutcomeAccountDelayTimeout.StopReason(), nil)
	default:
		return stop(OutcomeProviderFailure, fmt.Sprintf("provider failure: %s", admission.Code), func(result *Result) {
			result.Classification = string(admission.Code)
		})
	}
}

func (l *Loop) handleParseFailure(
	failure *protocol.ParseFailure,
	transcript *transcript,
	state *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	if !failure.CorrectionReasonable || state.corrections >= l.limits.MaxCorrections {
		return stop(OutcomeCorrectionsExhausted, fmt.Sprintf("protocol correction exhausted: %s", failure.Code), nil), true
	}
	state.corrections++
	retriesRemaining := l.limits.MaxCorrections - state.corrections
	message, err := protocol.GenerateCorrectionMessage(failure.Code, retriesRemaining)
	if err != nil {
		return stop(OutcomeProviderFailure, "provider failure: correction message generation failed", nil), true
	}
	emit(TraceLine{Kind: TraceCorrection, Status: string(failure.Code), Code: string(failure.Code), RetriesRemaining: retriesRemaining})
	transcript.correction(message)
	return Result{}, false
}

func (l *Loop) handleAction(
	ctx context.Context,
	action *protocol.Action,
	transcript *transcript,
	evidence *EvidenceSet,
	guard *repeatGuard,
	state *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	probe := func() string {
		signature, err := workspaceSignature(ctx, l.registry.Workspace())
		if err != nil {
			return ""
		}
		return signature
	}
	signature := ""
	signatureComputed := false
	signatureFor := func() string {
		if signatureComputed {
			return signature
		}
		signatureComputed = true
		signature = probe()
		return signature
	}

	if guard.repeat(*action, signatureFor()) {
		state.repeated++
		if state.repeated > l.limits.MaxRepeatedActions {
			return stop(OutcomeRepeatedAction, OutcomeRepeatedAction.StopReason(), nil), true
		}
		retriesRemaining := l.limits.MaxRepeatedActions - state.repeated
		message, err := protocol.GenerateCorrectionMessage(protocol.FailureRepeatedAction, retriesRemaining)
		if err != nil {
			return stop(OutcomeProviderFailure, "provider failure: correction message generation failed", nil), true
		}
		emit(TraceLine{Kind: TraceCorrection, Status: string(protocol.FailureRepeatedAction), Code: string(protocol.FailureRepeatedAction), RetriesRemaining: retriesRemaining})
		transcript.correction(message)
		return Result{}, false
	}

	executionStart := l.clock.Now()
	observation := l.registry.Execute(ctx, *action)
	executionDuration := l.clock.Now().Sub(executionStart)
	status := "executed"
	if !observation.Success {
		status = "failed"
		if observation.Failure != nil {
			status = string(observation.Failure.Code)
		}
	}
	emit(TraceLine{Kind: TraceAction, Status: status, Tool: action.Tool, Duration: executionDuration, EvidenceID: observation.ID})
	observationStatus := "success"
	if !observation.Success {
		observationStatus = "failed"
	}
	emit(TraceLine{Kind: TraceObservation, Status: observationStatus, EvidenceID: observation.ID, Duration: executionDuration})
	guard.record(*action, signatureFor())
	evidence.Add(observation)
	transcript.observation(observation)
	return Result{}, false
}

func (l *Loop) handleFinal(
	final *protocol.FinalResponse,
	evidence *EvidenceSet,
	state *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	grounded, missing := evidence.Ground(*final)
	if !grounded {
		return stop(OutcomeFinalNotGrounded, fmt.Sprintf("final evidence not grounded: missing %s", strings.Join(missing, ",")), func(result *Result) {
			result.Evidence = append([]string(nil), missing...)
		}), true
	}
	if final.Status == protocol.StatusComplete {
		return stop(OutcomeCompleted, OutcomeCompleted.StopReason(), func(result *Result) {
			result.Summary = final.Summary
			result.Evidence = append([]string(nil), final.Evidence...)
		}), true
	}
	return stop(OutcomeFinalIncomplete, OutcomeFinalIncomplete.StopReason(), func(result *Result) {
		result.Summary = final.Summary
		result.Evidence = append([]string(nil), final.Evidence...)
	}), true
}
