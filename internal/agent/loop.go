package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
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
// MaxCorrections and MaxRepeatedActions treat zero as a valid explicit value
// that disables the corresponding allowance: the loop stops with
// corrections_exhausted or repeated_action without granting a single
// correction or repeat. Only negative values for those two fields fall back to
// the defaults. The remaining fields treat zero or negative as "use the
// default", since zero steps, zero elapsed time or zero provider attempts are
// meaningless budgets.
type Limits struct {
	MaxSteps           int           // total model turns
	MaxCorrections     int           // protocol correction attempts; 0 disables
	MaxRepeatedActions int           // repeated-action corrections; 0 disables
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
// writes, no shell and no raw SQL; it coordinates the governor executor,
// protocol parser, repeat guard, tool registry and the semantic persistence
// boundary (issue #8).
type Loop struct {
	runner       AttemptRunner
	registry     *tools.Registry
	contract     string
	limits       Limits
	clock        Clock
	trace        TraceSink
	model        string
	state        state.Persistence
	policy       policy.Policy
	writePolicy  string
	recipePolicy string
	// recipeCatalogDigest is the persisted digest of the effective recipe
	// catalog, used to reject catalog drift on resume (issue #26 review).
	recipeCatalogDigest string
	recovery            *RecoverySeed
}

// Config wires one loop instance at the composition root.
type Config struct {
	Runner   AttemptRunner
	Registry *tools.Registry
	Limits   Limits
	Clock    Clock
	Trace    TraceSink
	Model    string
	// State is the optional semantic persistence boundary. A nil value
	// disables persistence (the M1 in-memory behavior).
	State state.Persistence
	// Policy is the optional control-plane decision seam for write actions and
	// process recipes (issues #10/#26). A nil policy fails closed: no
	// policy-gated effect executes, and every proposal is denied with the
	// typed reason "no_write_policy".
	Policy policy.Policy
	// WritePolicy is the canonical tool=mode specification of the effective
	// write policy (policy.Config.Spec). It is persisted with the task
	// configuration so resume continues under the SAME policy the task
	// started with; a divergent override is rejected by the composition root.
	WritePolicy string
	// RecipePolicy is the canonical recipe=mode specification of the effective
	// recipe policy (policy.Config.RecipeSpec over the configured catalog). It
	// is persisted with the task configuration so resume continues under the
	// SAME recipe policy; a divergent override is rejected by the composition
	// root.
	RecipePolicy string
	// RecipeCatalogDigest is the stable digest of the effective recipe catalog
	// (issue #26 review). It is persisted with the task configuration so resume
	// can reject catalog drift fail-closed: a re-supplied catalog whose
	// effective definitions changed can never silently continue the task. Empty
	// when no catalog is configured.
	RecipeCatalogDigest string
	// Recovery is the optional reconstructed state of an interrupted task
	// (issue #9). A nil value starts a fresh run; a non-nil value resumes the
	// same durable task from persisted state without replaying historical
	// model calls or re-executing completed effects.
	Recovery *RecoverySeed
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
	if limits.MaxCorrections < 0 {
		limits.MaxCorrections = DefaultLimits().MaxCorrections
	}
	if limits.MaxRepeatedActions < 0 {
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
		runner:              config.Runner,
		registry:            config.Registry,
		contract:            contract,
		limits:              limits,
		clock:               clock,
		trace:               traceSink,
		model:               strings.TrimSpace(config.Model),
		state:               config.State,
		policy:              config.Policy,
		writePolicy:         strings.TrimSpace(config.WritePolicy),
		recipePolicy:        strings.TrimSpace(config.RecipePolicy),
		recipeCatalogDigest: strings.TrimSpace(config.RecipeCatalogDigest),
		recovery:            config.Recovery,
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

	run := runState{}
	emit := func(line TraceLine) {
		run.sequence++
		line.Sequence = run.sequence
		l.trace(line)
	}

	// Persist the durable task root before the first provider attempt: the
	// task row must exist before any attempt row can reference it, and a
	// crash before the first TX 1 leaves a reconstructable task with no
	// attempts. The bootstrap must not depend on the run deadline: a task
	// with an already-elapsed budget still gets a durable terminal outcome.
	// A resumed run skips this: the task row already exists and the recovery
	// pipeline reconciled its interrupted attempts.
	if l.state != nil && l.recovery == nil {
		if err := l.state.CreateTask(context.Background(), state.TaskRecord{
			TaskID:     task.ID,
			Objective:  task.Prompt,
			Workspace:  l.registry.Workspace(),
			Model:      l.model,
			ConfigJSON: l.configSnapshot(),
		}); err != nil {
			return persistenceFailure(err)
		}
		if err := l.state.StartTask(context.Background(), task.ID); err != nil {
			return persistenceFailure(err)
		}
	}

	if l.recovery != nil {
		// A resumed run continues from the reconstructed durable state: the
		// recovery context is appended to the transcript, the grounding set is
		// seeded with persisted evidence, the repeat guard is seeded with the
		// workspace signatures recorded when historical actions were accepted,
		// and the run counters continue so the loop budgets do not reset.
		if l.recovery.Context != "" {
			transcript.recovery(l.recovery.Context)
		}
		for _, observation := range l.recovery.Evidence {
			evidence.Add(observation)
		}
		for fingerprint, signature := range l.recovery.Guard {
			guard.seed(fingerprint, signature)
		}
		run.turns = maxInt(run.turns, l.recovery.Turns)
		run.attempts = maxInt(run.attempts, l.recovery.Attempts)
		run.repeated = maxInt(run.repeated, l.recovery.Repeated)
		run.sequence = maxInt(run.sequence, l.recovery.TraceSequence)
		// The recovery boundary marks where reconciliation ends and new
		// governed execution begins in the resumed trace.
		emit(TraceLine{Kind: TraceRecoveryBoundary, Status: "new execution begins"})
	}
	stop := func(outcome Outcome, reason string, extra func(*Result)) Result {
		result := Result{
			Outcome:      outcome,
			StopReason:   reason,
			Turns:        run.turns,
			Attempts:     run.attempts,
			Observations: evidence.Count(),
			Corrections:  run.corrections,
			Repeated:     run.repeated,
			MixedProse:   run.mixedProse,
		}
		if extra != nil {
			extra(&result)
		}
		if l.state != nil {
			if outcome == OutcomeApprovalRequired {
				// A write requires operator approval: the task is NOT
				// finalized. It stays durably resumable (status running) with
				// the pending action recorded, so `runstead decide` +
				// `runstead resume` continues the same task. No correction
				// budget is consumed and no further provider attempt is made
				// to wait for the operator.
				if err := l.state.MarkTaskApprovalRequired(context.Background(), task.ID, result.PendingActionID, result.StopReason); err != nil {
					result.Outcome = OutcomePersistenceFailure
					result.StopReason = fmt.Sprintf("durable state could not be persisted: %v", err)
				}
			} else if err := l.state.FinalizeTask(context.Background(), state.TaskFinalize{
				TaskID:         task.ID,
				Outcome:        string(result.Outcome),
				StopReason:     result.StopReason,
				Summary:        result.Summary,
				Classification: result.Classification,
				Evidence:       result.Evidence,
				Turns:          result.Turns,
				Attempts:       result.Attempts,
				Observations:   result.Observations,
				Corrections:    result.Corrections,
				Repeated:       result.Repeated,
				MixedProse:     result.MixedProse,
			}); err != nil {
				result.Outcome = OutcomePersistenceFailure
				result.StopReason = fmt.Sprintf("durable state could not be persisted: %v", err)
			}
		}
		emit(TraceLine{Kind: TraceStop, Status: string(result.Outcome), StopReason: result.StopReason})
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
		if run.turns >= l.limits.MaxSteps {
			return stop(OutcomeStepsExhausted, OutcomeStepsExhausted.StopReason(), nil)
		}
		if run.attempts >= l.limits.ProviderBudget {
			return stop(OutcomeProviderBudgetExhausted, OutcomeProviderBudgetExhausted.StopReason(), nil)
		}

		if turn, terminal := l.modelTurn(ctx, task, transcript, evidence, guard, deadline, &run, emit, stop); terminal {
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

// configSnapshot renders the meaningful execution configuration as a
// sanitized JSON snapshot for the durable task row. It contains no secrets:
// workspace, model, the loop limits, the effective write policy, the effective
// recipe policy and the digest of the effective recipe catalog. Both policies
// are part of the authoritative task configuration: resume continues under the
// persisted policies and rejects a divergent override (issues #10/#26). The
// catalog digest lets resume reject catalog drift fail-closed (issue #26
// review).
func (l *Loop) configSnapshot() []byte {
	encoded, err := json.Marshal(map[string]any{
		"workspace":             l.registry.Workspace(),
		"model":                 l.model,
		"write_policy":          l.writePolicy,
		"recipe_policy":         l.recipePolicy,
		"recipe_catalog_digest": l.recipeCatalogDigest,
		"max_steps":             l.limits.MaxSteps,
		"max_corrections":       l.limits.MaxCorrections,
		"max_repeated_actions":  l.limits.MaxRepeatedActions,
		"time_budget_ns":        int64(l.limits.TimeBudget),
		"provider_budget":       l.limits.ProviderBudget,
	})
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// persistenceFailure builds the terminal result for a failed persistence
// operation. The stop reason carries the error so the failure is
// diagnosable; the outcome is typed so exit codes stay stable.
func persistenceFailure(err error) Result {
	return Result{
		Outcome:    OutcomePersistenceFailure,
		StopReason: fmt.Sprintf("durable state could not be persisted: %v", err),
	}
}

func outcomeReasonCanceled(err error) string {
	if err == nil {
		return OutcomeCanceled.StopReason()
	}
	return fmt.Sprintf("%s: %v", OutcomeCanceled.StopReason(), err)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	run *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	attemptStart := l.clock.Now()
	run.turns++
	run.attempts++
	requestID := fmt.Sprintf("%s-%04d", task.ID, run.turns)
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
		run.mixedProse++
		emit(TraceLine{Kind: TraceDeviation, Status: "mixed_prose"})
	}
	if parse.Failure != nil {
		return l.handleParseFailure(parse.Failure, transcript, run, emit, stop)
	}

	switch parse.Kind {
	case protocol.KindAction:
		return l.handleAction(ctx, task, parse.Action, transcript, evidence, guard, run, emit, stop)
	case protocol.KindFinal:
		return l.handleFinal(ctx, task, parse.Final, evidence, run, emit, stop)
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
	run *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	if !failure.CorrectionReasonable || run.corrections >= l.limits.MaxCorrections {
		return stop(OutcomeCorrectionsExhausted, fmt.Sprintf("protocol correction exhausted: %s", failure.Code), nil), true
	}
	run.corrections++
	retriesRemaining := l.limits.MaxCorrections - run.corrections
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
	task Task,
	action *protocol.Action,
	transcript *transcript,
	evidence *EvidenceSet,
	guard *repeatGuard,
	run *runState,
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

	// Every accepted envelope becomes a distinct logical action BEFORE the
	// repeat guard decision, so a proposal the guard rejects is still
	// represented as a rejected logical action. The workspace signature is
	// captured at acceptance time and persisted as repeat/loop evidence so a
	// resumed run can seed its repeat guard (issue #9).
	actionID := ""
	if l.state != nil {
		arguments, marshalErr := json.Marshal(action.Arguments)
		if marshalErr != nil {
			arguments = []byte("{}")
		}
		var err error
		actionID, err = l.state.RecordAction(ctx, state.ActionRecord{
			TaskID:             task.ID,
			Tool:               action.Tool,
			Arguments:          arguments,
			Fingerprint:        protocol.ActionFingerprint(*action),
			RecipeFingerprint:  l.recipeApprovalFingerprint(action),
			WorkspaceSignature: signatureFor(),
		})
		if err != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
		}
	}

	// Policy-gated effects (writes and process recipes) are decided by the
	// control-plane policy BEFORE any execution decision. The decision is
	// persisted (allowed, denied, approval_required) with its typed reason,
	// and model prose can never influence it. A denied proposal is rejected
	// as a logical action and the model receives a correction; an
	// approval-required proposal stays pending operator approval.
	var policyOutcome policy.Outcome
	if l.registry.IsPolicyGated(action.Tool) {
		var err error
		policyOutcome, err = l.evaluatePolicy(ctx, task, actionID, action)
		if err != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
		}
		switch policyOutcome.Decision {
		case policy.Denied:
			if l.state != nil {
				if err := l.state.RejectAction(ctx, task.ID, actionID, policyOutcome.Reason); err != nil {
					return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
				}
			}
			emit(TraceLine{Kind: TraceAction, Status: string(policyOutcome.Decision), Tool: action.Tool, Classification: policyOutcome.Reason})
			return l.policyCorrection(transcript, action.Tool, policyOutcome, run, emit, stop)
		case policy.ApprovalRequired:
			// Control-plane dependency: the effect must not execute and the run
			// pauses until the operator records a decision. This is NOT a
			// protocol correction: no correction budget is consumed, no
			// further provider attempt is made to wait for the operator, and
			// the task is not finalized. The action stays pending and the
			// CLI reports the task/action for `runstead decide`.
			emit(TraceLine{Kind: TraceAction, Status: string(policyOutcome.Decision), Tool: action.Tool, Classification: policyOutcome.Reason})
			return stop(OutcomeApprovalRequired, OutcomeApprovalRequired.StopReason(), func(result *Result) {
				result.PendingActionID = actionID
			}), true
		case policy.Allowed:
			// Fall through to the repeat guard and execution.
		}
	}

	if guard.repeat(*action, signatureFor()) {
		run.repeated++
		// The proposal was rejected by the repeat guard, so its durable
		// projection must be 'rejected' in every branch, including the
		// terminal one: the loop may stop right after this on the repeated
		// action limit, and the action must never remain 'planned'.
		if l.state != nil {
			if err := l.state.RejectAction(ctx, task.ID, actionID, "repeated_action"); err != nil {
				return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
			}
		}
		if run.repeated > l.limits.MaxRepeatedActions {
			return stop(OutcomeRepeatedAction, OutcomeRepeatedAction.StopReason(), nil), true
		}
		retriesRemaining := l.limits.MaxRepeatedActions - run.repeated
		message, err := protocol.GenerateCorrectionMessage(protocol.FailureRepeatedAction, retriesRemaining)
		if err != nil {
			return stop(OutcomeProviderFailure, "provider failure: correction message generation failed", nil), true
		}
		emit(TraceLine{Kind: TraceCorrection, Status: string(protocol.FailureRepeatedAction), Code: string(protocol.FailureRepeatedAction), RetriesRemaining: retriesRemaining})
		transcript.correction(message)
		return Result{}, false
	}

	// TX 1: persist the concrete tool execution intent before the effect. For
	// write tools the deterministic expected after-state hash and the bounded
	// planned diff are computed from the real arguments and persisted with the
	// intent so recovery can reconcile an interruption from observable
	// filesystem state and reconstruct bounded diff evidence. For process
	// recipes the resolved recipe intent (recipe, argv, capabilities, policy
	// decision) is persisted as intent evidence; a prepared process attempt
	// left by a crash is recovery class 4 and is never blindly re-run.
	executionID := ""
	if l.state != nil {
		arguments, marshalErr := json.Marshal(action.Arguments)
		if marshalErr != nil {
			arguments = []byte("{}")
		}
		plan := l.registry.PlanWrite(*action)
		processIntent, intentErr := l.buildProcessIntent(action)
		if intentErr != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), intentErr), nil), true
		}
		var err error
		executionID, err = l.state.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
			TaskID:          task.ID,
			ActionID:        actionID,
			Tool:            action.Tool,
			Arguments:       arguments,
			RecoveryClass:   recoveryClassFor(action.Tool),
			EffectAfterHash: plan.AfterHash,
			PlannedEffect:   plan.Effect,
			ProcessIntent:   processIntent,
		})
		if err != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
		}
	}

	executionStart := l.clock.Now()
	observation := l.registry.Execute(ctx, *action)
	executionDuration := l.clock.Now().Sub(executionStart)
	// Attach the execution identities to successful write evidence before TX 2
	// so the persisted evidence carries action/execution ids.
	if l.registry.IsWriteTool(action.Tool) {
		l.registry.AnnotateWriteEvidence(&observation, actionID, executionID)
	}
	// For recipes the persisted evidence must carry the REAL control-plane
	// policy decision (for example allowed/approved_by_operator), never the
	// hardcoded placeholder the execute path used to build the observation.
	if l.registry.IsRecipeTool(action.Tool) {
		l.registry.AnnotateRecipeEvidence(&observation, actionID, executionID, string(policyOutcome.Decision), policyOutcome.Reason)
	}
	status := "executed"
	if !observation.Success {
		status = "failed"
		if observation.Failure != nil {
			status = string(observation.Failure.Code)
		}
	}
	actionLine := TraceLine{Kind: TraceAction, Status: status, Tool: action.Tool, Duration: executionDuration, EvidenceID: observation.ID}
	if l.registry.IsRecipeTool(action.Tool) {
		if evidence, ok := observation.Data.(recipe.Evidence); ok {
			actionLine.ExitCode = evidence.ExitCode
		}
	}
	emit(actionLine)
	observationStatus := "success"
	if !observation.Success {
		observationStatus = "failed"
	}
	emit(TraceLine{Kind: TraceObservation, Status: observationStatus, EvidenceID: observation.ID, Duration: executionDuration})

	// TX 2: persist the tool result and evidence after the effect returned.
	if l.state != nil {
		attemptStatus := "completed"
		classification := ""
		if !observation.Success {
			attemptStatus = "failed"
			if observation.Failure != nil {
				classification = string(observation.Failure.Code)
			}
		}
		evidenceID := ""
		if observation.Success {
			evidenceID = observation.ID
		}
		if err := l.state.CompleteToolAttempt(ctx, state.ToolAttemptCompleted{
			TaskID:         task.ID,
			ExecutionID:    executionID,
			Status:         attemptStatus,
			Classification: classification,
			EvidenceID:     evidenceID,
			DurationNanos:  int64(executionDuration),
			Observation:    observation,
		}); err != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
		}
	}

	guard.record(*action, signatureFor())
	evidence.Add(observation)
	transcript.observation(observation)
	return Result{}, false
}

func (l *Loop) handleFinal(
	ctx context.Context,
	task Task,
	final *protocol.FinalResponse,
	evidence *EvidenceSet,
	run *runState,
	emit func(TraceLine),
	stop func(Outcome, string, func(*Result)) Result,
) (Result, bool) {
	grounded, missing := evidence.Ground(*final)
	if !grounded {
		return stop(OutcomeFinalNotGrounded, fmt.Sprintf("final evidence not grounded: missing %s", strings.Join(missing, ",")), func(result *Result) {
			result.Evidence = append([]string(nil), missing...)
		}), true
	}
	// No task may finalize (as completed OR failed) while a mandatory write is
	// still awaiting an operator approval: pause instead, keeping the task
	// durably resumable so the operator can still decide and resume.
	if l.state != nil {
		pending, err := l.state.PendingApprovals(ctx, task.ID)
		if err != nil {
			return stop(OutcomePersistenceFailure, fmt.Sprintf("%s: %v", OutcomePersistenceFailure.StopReason(), err), nil), true
		}
		if len(pending) > 0 {
			emit(TraceLine{Kind: TraceAction, Status: string(policy.ApprovalRequired), Tool: pending[0].Tool, Classification: "approval_required"})
			return stop(OutcomeApprovalRequired, OutcomeApprovalRequired.StopReason(), func(result *Result) {
				result.PendingActionID = pending[0].ActionID
			}), true
		}
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

// evaluatePolicy computes and persists the control-plane decision for one
// write or process-recipe proposal. The decision is persisted with its typed
// reason (allowed, denied, approval_required) before any execution decision;
// model prose is never an input to the policy.
func (l *Loop) evaluatePolicy(ctx context.Context, task Task, actionID string, action *protocol.Action) (policy.Outcome, error) {
	outcome := policy.Outcome{Decision: policy.Denied, Reason: "no_write_policy"}
	shortCircuit := false
	request := policy.Request{
		TaskID:      task.ID,
		ActionID:    actionID,
		Tool:        action.Tool,
		Fingerprint: protocol.ActionFingerprint(*action),
		Workspace:   l.registry.Workspace(),
	}
	if l.registry.IsRecipeTool(action.Tool) {
		if recipeID, failure := recipeArgument(action); failure == nil {
			if selected, ok := l.registry.Recipe(recipeID); !ok {
				// A recipe outside the configured catalog is meaningless: it
				// is denied here (and would fail unknown_recipe at execution,
				// which never happens because policy gates first).
				outcome = policy.Outcome{Decision: policy.Denied, Reason: "unknown_recipe"}
				shortCircuit = true
			} else {
				request.Recipe = recipeID
				// Approval identity is bound to the EFFECTIVE recipe
				// definition digest: an approval for one definition can never
				// authorize a different definition of the same id (issue #26
				// review).
				request.Fingerprint = recipe.ApprovalFingerprint(recipeID, selected.Digest())
			}
		}
	}
	if !shortCircuit && l.policy != nil {
		outcome = l.policy.Evaluate(ctx, request)
	}
	if l.state != nil {
		if request.Recipe != "" {
			if err := l.state.RecordRecipePolicyDecision(ctx, state.RecipePolicyDecision{
				TaskID: task.ID, ActionID: actionID, Recipe: request.Recipe,
				Decision: string(outcome.Decision), Reason: outcome.Reason,
			}); err != nil {
				return policy.Outcome{}, err
			}
		} else if err := l.state.RecordWritePolicyDecision(ctx, state.WritePolicyDecision{
			TaskID:   task.ID,
			ActionID: actionID,
			Tool:     action.Tool,
			Decision: string(outcome.Decision),
			Reason:   outcome.Reason,
		}); err != nil {
			return policy.Outcome{}, err
		}
	}
	return outcome, nil
}

// recipeArgument reads the recipe id from a run_recipe action's arguments.
func recipeArgument(action *protocol.Action) (string, *tools.Failure) {
	raw, ok := action.Arguments["recipe"]
	if !ok {
		return "", tools.InvalidArgumentFailure()
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", tools.InvalidArgumentFailure()
	}
	return value, nil
}

// recipeApprovalFingerprint returns the digest-bound approval identity of a
// run_recipe proposal, or the empty string when the action is not a known
// recipe (unknown recipes are denied by the policy gate and never approved).
// The identity binds the recipe id to its effective definition digest, so an
// operator approval for one definition can never authorize a different
// definition of the same id (issue #26 review).
func (l *Loop) recipeApprovalFingerprint(action *protocol.Action) string {
	if !l.registry.IsRecipeTool(action.Tool) {
		return ""
	}
	recipeID, failure := recipeArgument(action)
	if failure != nil {
		return ""
	}
	selected, ok := l.registry.Recipe(recipeID)
	if !ok {
		return ""
	}
	return recipe.ApprovalFingerprint(recipeID, selected.Digest())
}

// policyCorrection reports a non-executed policy-gated effect to the model as
// a correction and applies the correction budget. Denial is a model error
// (it proposed a denied effect); approval-required never reaches this path.
func (l *Loop) policyCorrection(transcript *transcript, tool string, outcome policy.Outcome, run *runState, emit func(TraceLine), stop func(Outcome, string, func(*Result)) Result) (Result, bool) {
	if run.corrections >= l.limits.MaxCorrections {
		return stop(OutcomeCorrectionsExhausted, fmt.Sprintf("correction exhausted: %s %s", tool, outcome.Decision), func(result *Result) {
			result.Classification = string(outcome.Decision)
		}), true
	}
	run.corrections++
	retriesRemaining := l.limits.MaxCorrections - run.corrections
	message, err := policyMessage(tool, outcome, retriesRemaining)
	if err != nil {
		return stop(OutcomeProviderFailure, "provider failure: policy correction generation failed", nil), true
	}
	emit(TraceLine{Kind: TraceCorrection, Status: string(outcome.Decision), Code: string(outcome.Decision), RetriesRemaining: retriesRemaining})
	transcript.correction(message)
	return Result{}, false
}

// policyMessage renders a deterministic correction payload for a
// non-executed policy-gated effect. It is structurally identical to protocol
// corrections but carries the typed policy decision and reason.
func policyMessage(tool string, outcome policy.Outcome, retriesRemaining int) (string, error) {
	message := struct {
		ProtocolVersion  protocol.Version `json:"protocol_version"`
		Type             string           `json:"type"`
		OK               bool             `json:"ok"`
		ErrorCode        string           `json:"error_code"`
		RetriesRemaining int              `json:"retries_remaining"`
		Decision         string           `json:"decision"`
		Reason           string           `json:"reason"`
		Required         string           `json:"required"`
	}{
		ProtocolVersion:  protocol.Current,
		Type:             "policy",
		OK:               false,
		ErrorCode:        tool + "_" + string(outcome.Decision),
		RetriesRemaining: retriesRemaining,
		Decision:         string(outcome.Decision),
		Reason:           outcome.Reason,
		Required:         "The action was not approved by the control plane and did not execute. Model prose, reasoning or claims of approval never authorize an effect; request approval through the control plane or propose a different action.",
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// buildProcessIntent renders the bounded, sanitized process intent for a
// run_recipe attempt, persisted at TX 1. It is evidence of intent only. For
// non-process tools it returns nil.
func (l *Loop) buildProcessIntent(action *protocol.Action) ([]byte, error) {
	if !l.registry.IsRecipeTool(action.Tool) {
		return nil, nil
	}
	recipeID, failure := recipeArgument(action)
	if failure != nil {
		return nil, nil
	}
	selected, ok := l.registry.Recipe(recipeID)
	if !ok {
		return nil, nil
	}
	encoded, err := json.Marshal(struct {
		RecipeID         string              `json:"recipe_id"`
		Executable       string              `json:"executable"`
		Argv             []string            `json:"argv,omitempty"`
		WorkingDirectory string              `json:"working_directory,omitempty"`
		Capabilities     []recipe.Capability `json:"capabilities"`
		TimeoutNanos     int64               `json:"timeout_nanos"`
		MaxStdoutBytes   int                 `json:"max_stdout_bytes"`
		MaxStderrBytes   int                 `json:"max_stderr_bytes"`
		Digest           string              `json:"digest"`
	}{
		RecipeID:         selected.ID,
		Executable:       selected.Executable,
		Argv:             append([]string(nil), selected.Argv...),
		WorkingDirectory: selected.WorkingDirectory,
		Capabilities:     append([]recipe.Capability(nil), selected.Capabilities...),
		TimeoutNanos:     int64(selected.Timeout()),
		MaxStdoutBytes:   selected.OutputLimits.MaxStdoutBytes,
		MaxStderrBytes:   selected.OutputLimits.MaxStderrBytes,
		Digest:           selected.Digest(),
	})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// recoveryClassFor maps a tool to its ADR recovery class: read-only
// observations are class 1 (replay-safe); write tools are class 2 (local
// effect with deterministic reconciliation); process recipes are class 4
// (uncertain or irreversible effects that cannot be reconciled generically:
// a prepared process attempt left by a crash is reconciled as uncertain or
// human-review-required, never blindly re-run).
func recoveryClassFor(tool string) int {
	if tool == tools.ToolWriteFile || tool == tools.ToolApplyPatch {
		return 2
	}
	if tool == tools.ToolRunRecipe {
		return 4
	}
	return 1
}
