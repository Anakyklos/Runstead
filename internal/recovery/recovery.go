// Package recovery implements the resume/recovery pipeline (issue #9):
//
//	load task
//	→ load authoritative persisted history
//	→ classify existing attempts
//	→ reconcile interrupted/uncertain attempts
//	→ decide whether automatic continuation is safe
//	→ reconstruct bounded model context
//	→ establish recovery boundary
//	→ continue through the normal governed agent loop
//
// Recovery means persisted state + environment reconciliation + bounded
// context reconstruction + new governed execution attempts. It never replays
// historical provider calls, never re-executes completed effects, never
// fabricates historical execution, and never resets governor/account
// protection (the CLI restores the persisted governor projection).
package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Decision is the typed outcome of the recovery pipeline before the loop runs.
type Decision string

const (
	// DecisionContinue means automatic continuation is safe: the interrupted
	// attempts were reconciled and the loop may run with the reconstructed
	// seed.
	DecisionContinue Decision = "continue"
	// DecisionHumanReview means an unreconcilable effect requires a human
	// decision before any automatic continuation. The task was persisted in
	// the human_review_required state with structured reasons.
	DecisionHumanReview Decision = "human_review_required"
	// DecisionBlocked means automatic continuation is currently blocked by
	// restored account-protection policy (circuit, cooldown, rolling or task
	// budget). The recovery decision is journaled and the task remains
	// pending; a later resume may proceed once the block clears.
	DecisionBlocked Decision = "governor_blocked"
)

// Plan is the result of the recovery pipeline. When Decision is
// DecisionContinue, Seed carries the reconstructed state for agent.Loop.
type Plan struct {
	Decision Decision
	// Reason explains the decision for the operator. It is also persisted in
	// the journal (task_human_review_required event) where applicable.
	Reason string
	// ReconciledToolAttempts and ReconciledProviderAttempts count the
	// interrupted attempts that were reconciled during this recovery.
	ReconciledToolAttempts     int
	ReconciledProviderAttempts int
	// UncertainProviderAttempts counts provider requests that may have reached
	// upstream and stay conservatively accounted.
	UncertainProviderAttempts int
	// Seed is non-nil when Decision == DecisionContinue.
	Seed *agent.RecoverySeed
	// Task describes the resumed task root for the caller (composition root):
	// the objective, workspace, model and persisted configuration snapshot.
	Task TaskInfo
	// NextEvidenceSequence is the highest persisted evidence sequence for this
	// task, so the resumed registry continues the task-scoped evidence ID space
	// instead of colliding with persisted evidence.
	NextEvidenceSequence int
}

// TaskInfo is the durable task root a resume continues.
type TaskInfo struct {
	ID         string
	Objective  string
	Workspace  string
	Model      string
	ConfigJSON string
}

// Options configures one recovery run.
type Options struct {
	TaskID string
	// Trace receives the sanitized live recovery trace lines. Nil disables
	// them.
	Trace agent.TraceSink
	// Budget bounds the reconstructed model context. Zero uses DefaultBudget.
	Budget Budget
	// Blocked is an optional callback over the restored account governor
	// reporting whether automatic continuation is currently blocked by
	// account-protection policy. When it reports blocked, the pipeline
	// journals recovery_blocked and returns DecisionBlocked instead of
	// starting the loop. A nil callback never blocks.
	Blocked func() (bool, string)
}

// errTrace is a helper type so Options.Trace can stay an agent.TraceSink.
type traceLines struct {
	sink     agent.TraceSink
	sequence int
}

func (t *traceLines) emit(kind, status string) {
	if t == nil || t.sink == nil {
		return
	}
	t.sequence++
	t.sink(agent.TraceLine{Sequence: t.sequence, Kind: kind, Status: status})
}

// Resume loads the authoritative persisted history of one interrupted task,
// classifies and reconciles its attempts, and decides whether automatic
// continuation through the normal agent loop is safe. All reconciliation
// transitions and their journal events are persisted atomically; no SQLite
// transaction is ever held across external work. The caller then runs the
// normal agent loop with the returned seed (or reports the typed decision).
func Resume(ctx context.Context, store *state.Store, options Options) (*Plan, error) {
	if options.TaskID == "" {
		return nil, fmt.Errorf("recovery requires a task id")
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, options.TaskID)
	if err != nil {
		return nil, err
	}
	if isTerminalTaskStatus(snapshot.Task.Status) {
		return nil, fmt.Errorf("%w: task %q is %s", state.ErrNotResumable, options.TaskID, snapshot.Task.Status)
	}
	trace := &traceLines{sink: options.Trace}
	budget := options.Budget
	if budget.MaxContextBytes <= 0 {
		budget = DefaultBudget()
	}

	// The recovery boundary starts here: the persisted recovery_started event
	// (with the resume-count projection) precedes every reconciliation event.
	if err := store.MarkRecoveryStarted(ctx, options.TaskID, "task interrupted; recovery started"); err != nil {
		return nil, fmt.Errorf("mark recovery start: %w", err)
	}
	trace.emit(agent.TraceRecoveryStart, "task interrupted; recovery started")

	plan := &Plan{
		Decision: DecisionContinue,
		Task: TaskInfo{
			ID:         snapshot.Task.TaskID,
			Objective:  snapshot.Task.Objective,
			Workspace:  snapshot.Task.Workspace,
			Model:      snapshot.Task.Model,
			ConfigJSON: snapshot.Task.ConfigJSON,
		},
	}
	var humanReview []string

	// The evidence sequence continues from the highest persisted evidence id
	// so reconciled write evidence allocated during this recovery never
	// collides with the resumed registry's observation ids.
	nextEvidence := maxEvidenceSequence(snapshot.Evidence)

	// Classify and reconcile interrupted tool attempts. Completed attempts are
	// verified progress (eligible for context reconstruction); failed attempts
	// are deterministic failures; class-1 prepared attempts reconcile as
	// replay-safe; class-2 write attempts are reconciled from observable
	// filesystem state; any other non-terminal attempt requires human review.
	for _, attempt := range snapshot.ToolAttempts {
		switch classifyToolAttempt(attempt) {
		case toolVerified:
			// Terminal verified progress; no mutation.
		case toolFailed:
			// Terminal deterministic failure; no mutation.
		case toolNoop:
			// Already terminal (reconciled/canceled); no mutation.
		case toolReplaySafe:
			if err := store.ReconcileToolAttempt(ctx, state.ReconcileToolAttempt{
				TaskID:      options.TaskID,
				ExecutionID: attempt.ExecutionID,
				Status:      "reconciled",
				Reason:      "replay_safe_observation",
			}); err != nil {
				return nil, fmt.Errorf("reconcile tool attempt %s: %w", attempt.ExecutionID, err)
			}
			plan.ReconciledToolAttempts++
			trace.emit(agent.TraceRecoveryReconcile,
				fmt.Sprintf("tool attempt %s reconciled as replay_safe_observation", attempt.ExecutionID))
		case toolWriteReconcile:
			review, err := reconcileWriteAttempt(ctx, store, options.TaskID, snapshot.Task.Workspace, attempt, &nextEvidence)
			if err != nil {
				return nil, fmt.Errorf("reconcile write attempt %s: %w", attempt.ExecutionID, err)
			}
			plan.ReconciledToolAttempts++
			if review {
				humanReview = append(humanReview, attempt.ExecutionID)
				trace.emit(agent.TraceRecoveryUncertain,
					fmt.Sprintf("write attempt %s requires human review: the filesystem state matches neither the recorded precondition nor the expected after-state", attempt.ExecutionID))
			} else {
				trace.emit(agent.TraceRecoveryReconcile,
					fmt.Sprintf("write attempt %s reconciled from observable filesystem state", attempt.ExecutionID))
			}
		case toolHumanReview:
			if err := store.ReconcileToolAttempt(ctx, state.ReconcileToolAttempt{
				TaskID:      options.TaskID,
				ExecutionID: attempt.ExecutionID,
				Status:      "human_review_required",
				Reason:      "unreconcilable_effect",
			}); err != nil {
				return nil, fmt.Errorf("mark tool attempt %s human review: %w", attempt.ExecutionID, err)
			}
			humanReview = append(humanReview, attempt.ExecutionID)
			trace.emit(agent.TraceRecoveryUncertain,
				fmt.Sprintf("tool attempt %s requires human review: effect may have occurred and cannot be reconciled safely", attempt.ExecutionID))
		}
	}

	// Classify and reconcile interrupted provider attempts. A request that may
	// have reached upstream keeps its conservative debit and is never
	// re-issued; the recovery decision is journaled and the attempt becomes
	// reconciled. An already-persisted human-review requirement stops the task.
	for _, attempt := range snapshot.ProviderAttempts {
		switch classifyProviderAttempt(attempt) {
		case providerDone:
			// Terminal classified outcome; no mutation.
		case providerHumanReview:
			humanReview = append(humanReview, attempt.ExecutionID)
			trace.emit(agent.TraceRecoveryUncertain,
				fmt.Sprintf("provider attempt %s already requires human review", attempt.ExecutionID))
		case providerConservative:
			// A receipt-aware attempt interrupted before TX 2 was never debited
			// (StartReceiptAware defers all debits to the receipt finish path),
			// so recovery must apply the #29 conservative debit to the persisted
			// governor projection atomically. A persisted 'uncertain' status
			// means TX 2 already debited; a plain attempt was debited at TX 1.
			applyDebit := attempt.ReceiptAware && attempt.Status != "uncertain"
			if err := store.ReconcileProviderAttempt(ctx, state.ReconcileProviderAttempt{
				TaskID:                 options.TaskID,
				ExecutionID:            attempt.ExecutionID,
				ClientRequestID:        attempt.ClientRequestID,
				Status:                 "reconciled",
				Reason:                 "upstream_may_have_been_reached",
				Uncertain:              true,
				AttemptDebited:         1,
				DebitAt:                attempt.PreparedAt,
				ApplyConservativeDebit: applyDebit,
			}); err != nil {
				return nil, fmt.Errorf("reconcile provider attempt %s: %w", attempt.ExecutionID, err)
			}
			plan.ReconciledProviderAttempts++
			plan.UncertainProviderAttempts++
			trace.emit(agent.TraceRecoveryUncertain,
				fmt.Sprintf("provider attempt %s may have reached upstream; conservative debit preserved; not retried", attempt.ExecutionID))
		}
	}

	// Fail closed: an unreconcilable effect stops automatic continuation with
	// a typed human-review outcome and persisted structured evidence.
	if len(humanReview) > 0 {
		reason := fmt.Sprintf("automatic continuation stopped: %d attempt(s) require human review before any new execution", len(humanReview))
		if err := store.MarkHumanReviewRequired(ctx, options.TaskID, reason, humanReview); err != nil {
			return nil, fmt.Errorf("mark task human review: %w", err)
		}
		plan.Decision = DecisionHumanReview
		plan.Reason = reason
		trace.emit(agent.TraceRecoveryUncertain, reason)
		return plan, nil
	}

	// Reconciliation may have produced new citable evidence (verified write
	// completions) and completed their actions. Reload the authoritative
	// snapshot so the reconstructed context, evidence set and repeat guard
	// include the reconciled state instead of the pre-reconciliation view.
	snapshot, err = store.LoadRecoverySnapshot(ctx, options.TaskID)
	if err != nil {
		return nil, fmt.Errorf("reload recovery snapshot after reconciliation: %w", err)
	}

	// Account-protection state is authoritative across restart: when the
	// restored governor reports that continuation is currently blocked
	// (circuit, cooldown or budget), the task remains pending with a journaled
	// recovery_blocked decision instead of starting a new execution.
	if options.Blocked != nil {
		if blocked, reason := options.Blocked(); blocked {
			if err := store.AppendRecoveryEvent(ctx, options.TaskID, "recovery_blocked", map[string]any{
				"reason": state.Redact(reason),
			}); err != nil {
				return nil, fmt.Errorf("persist recovery blocked event: %w", err)
			}
			plan.Decision = DecisionBlocked
			plan.Reason = reason
			trace.emit(agent.TraceRecoveryBlocked, reason)
			return plan, nil
		}
	}

	// Reconstruct the bounded model context from verified progress, unresolved
	// failures, uncertain attempts and the persisted objective/constraints.
	context := BuildContext(snapshot, budget)
	if err := store.AppendRecoveryEvent(ctx, options.TaskID, "recovery_context_reconstructed", map[string]any{
		"evidence_ids":                 context.EvidenceIDs,
		"reconciled_tool_attempts":     plan.ReconciledToolAttempts,
		"reconciled_provider_attempts": plan.ReconciledProviderAttempts,
		"uncertain_provider_attempts":  plan.UncertainProviderAttempts,
		"context_chars":                context.Chars,
	}); err != nil {
		return nil, fmt.Errorf("persist recovery context event: %w", err)
	}
	trace.emit(agent.TraceRecoveryContext, fmt.Sprintf("reconstructed %d-byte context with %d evidence ids",
		context.Chars, len(context.EvidenceIDs)))

	seed := buildSeed(snapshot, context, trace.sequence)
	plan.NextEvidenceSequence = nextEvidence
	if err := store.AppendRecoveryEvent(ctx, options.TaskID, "recovery_continued", map[string]any{
		"turns":         seed.Turns,
		"attempts":      seed.Attempts,
		"evidence":      len(seed.Evidence),
		"context_chars": context.Chars,
	}); err != nil {
		return nil, fmt.Errorf("persist recovery boundary event: %w", err)
	}
	plan.Seed = seed
	return plan, nil
}

// maxEvidenceSequence returns the highest numeric evidence sequence among the
// persisted evidence IDs (obs-000001 -> 1). Zero means no persisted evidence.
func maxEvidenceSequence(evidence []state.RecoveryEvidence) int {
	max := 0
	for _, item := range evidence {
		sequence := evidenceSequence(item.EvidenceID)
		if sequence > max {
			max = sequence
		}
	}
	return max
}

func evidenceSequence(evidenceID string) int {
	const prefix = "obs-"
	if !strings.HasPrefix(evidenceID, prefix) {
		return 0
	}
	sequence := 0
	for _, digit := range strings.TrimPrefix(evidenceID, prefix) {
		if digit < '0' || digit > '9' {
			return 0
		}
		sequence = sequence*10 + int(digit-'0')
	}
	return sequence
}

// isTerminalTaskStatus reports whether the task status projection is terminal.
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "planned", "running":
		return false
	default:
		return true
	}
}

// GovernorBlocks reports whether the restored account governor currently
// blocks automatic continuation of the given task, and why. It mirrors the
// account-protection projection only: circuit, cooldown, rolling budgets and
// the governor task budget. The agent loop remains the authoritative admission
// path; this check exists so resume can leave the task pending with a journaled
// recovery_blocked decision instead of finalizing it on account-protection
// grounds.
func GovernorBlocks(g *governor.Governor, taskID string) (bool, string) {
	if g == nil {
		return false, ""
	}
	snapshot := g.Snapshot()
	switch snapshot.Circuit.State {
	case governor.CircuitHumanReviewRequired:
		return true, "account circuit requires human acknowledgement"
	case governor.CircuitOpenUntil:
		reason := "account circuit is open"
		if !snapshot.Circuit.OpenUntil.IsZero() {
			reason += fmt.Sprintf(" until %s", snapshot.Circuit.OpenUntil.UTC().Format(time.RFC3339))
		}
		return true, reason
	}
	if snapshot.Circuit.RefreshRequired {
		return true, "account circuit requires credential refresh"
	}
	now := time.Now()
	if !snapshot.CooldownUntil.IsZero() && snapshot.CooldownUntil.After(now) {
		return true, fmt.Sprintf("account cooldown active until %s", snapshot.CooldownUntil.UTC().Format(time.RFC3339))
	}
	if snapshot.Telemetry.RateLimited || snapshot.Telemetry.CapacityExhausted || snapshot.Telemetry.UpstreamCircuit == governor.UpstreamCircuitOpen {
		return true, "upstream rate or capacity state blocks admission"
	}
	if snapshot.Telemetry.Unsafe {
		return true, "conservative accounting is unsafe; human review required before continuation"
	}
	budgets := snapshot.Budgets
	if budgets.Rolling3hCeiling > 0 && budgets.Rolling3hUsed >= budgets.Rolling3hCeiling {
		return true, "rolling 3h provider budget exhausted"
	}
	if budgets.Rolling1hCeiling > 0 && budgets.Rolling1hUsed >= budgets.Rolling1hCeiling {
		return true, "rolling 1h provider budget exhausted"
	}
	if budgets.Rolling10mCeiling > 0 && budgets.Rolling10mUsed >= budgets.Rolling10mCeiling {
		return true, "rolling 10m provider budget exhausted"
	}
	if taskUsage, ok := snapshot.Tasks[taskID]; ok && budgets.TaskCeiling > 0 && taskUsage.Attempts >= budgets.TaskCeiling {
		return true, "task provider budget exhausted"
	}
	return false, ""
}

type toolClass int

const (
	toolVerified toolClass = iota
	toolFailed
	toolNoop
	toolReplaySafe
	toolWriteReconcile
	toolHumanReview
)

// classifyToolAttempt maps a persisted tool attempt to its recovery behavior.
// Class-1 attempts (read-only observations) reconcile as replay-safe; class-2
// attempts (write tools) reconcile from observable filesystem state; any
// other non-terminal attempt requires human review.
func classifyToolAttempt(attempt state.RecoveryToolAttempt) toolClass {
	switch attempt.Status {
	case "completed":
		return toolVerified
	case "failed":
		return toolFailed
	case "reconciled", "canceled":
		return toolNoop
	case "human_review_required":
		return toolHumanReview
	default:
		// planned/prepared/running/observed/verified/uncertain/verification_failed
		switch {
		case attempt.RecoveryClass == 1:
			return toolReplaySafe
		case attempt.RecoveryClass == 2 && isWriteTool(attempt.Tool):
			return toolWriteReconcile
		default:
			return toolHumanReview
		}
	}
}

// isWriteTool reports whether the tool is a policy-gated write tool.
func isWriteTool(tool string) bool {
	return tool == tools.ToolWriteFile || tool == tools.ToolApplyPatch
}

// reconcileWriteAttempt reconciles one interrupted write attempt (ADR
// recovery class 2) against the current filesystem state. It never repeats
// the effect. It returns true when the attempt escalated to
// human_review_required (so the caller stops automatic continuation).
func reconcileWriteAttempt(ctx context.Context, store *state.Store, taskID, workspaceRoot string, attempt state.RecoveryToolAttempt, nextEvidence *int) (bool, error) {
	result := tools.ReconcileWrite(ctx, workspaceRoot, tools.WriteIntent{
		Tool:              attempt.Tool,
		Arguments:         []byte(attempt.ArgumentsJSON),
		ExpectedAfterHash: attempt.EffectAfterHash,
	})
	switch result.Status {
	case tools.ReconcileNotStarted:
		return false, store.ReconcileToolAttempt(ctx, state.ReconcileToolAttempt{
			TaskID:      taskID,
			ExecutionID: attempt.ExecutionID,
			Status:      "reconciled",
			Reason:      "write_effect_not_started",
		})
	case tools.ReconcileCompleted:
		*nextEvidence++
		evidenceID := fmt.Sprintf("obs-%06d", *nextEvidence)
		return false, store.ReconcileWriteAttempt(ctx, state.ReconcileWriteAttempt{
			TaskID:      taskID,
			ExecutionID: attempt.ExecutionID,
			Status:      "reconciled",
			Reason:      "write_effect_completed",
			EvidenceID:  evidenceID,
			Evidence:    result.Evidence,
		})
	default:
		return true, store.ReconcileToolAttempt(ctx, state.ReconcileToolAttempt{
			TaskID:      taskID,
			ExecutionID: attempt.ExecutionID,
			Status:      "human_review_required",
			Reason:      "write_effect_unreconcilable",
		})
	}
}

type providerClass int

const (
	providerDone providerClass = iota
	providerConservative
	providerHumanReview
)

// classifyProviderAttempt maps a persisted provider attempt to its recovery
// behavior. Any request that may have reached upstream is reconciled
// conservatively: the debit stands, the request is never re-issued.
func classifyProviderAttempt(attempt state.RecoveryProviderAttempt) providerClass {
	switch attempt.Status {
	case "completed", "failed", "reconciled", "canceled":
		return providerDone
	case "human_review_required":
		return providerHumanReview
	default:
		// planned/prepared/running/uncertain
		return providerConservative
	}
}

// buildSeed reconstructs the agent loop seed from the persisted snapshot:
// continued run counters, persisted evidence, the repeat guard seeded with the
// workspace signatures recorded when historical actions were accepted, and the
// bounded recovery context.
func buildSeed(snapshot *state.RecoverySnapshot, context Context, traceSequence int) *agent.RecoverySeed {
	seed := &agent.RecoverySeed{
		Turns:         len(snapshot.ProviderAttempts),
		Attempts:      len(snapshot.ProviderAttempts),
		Repeated:      rejectedCount(snapshot),
		Evidence:      reconstructedObservations(snapshot),
		Guard:         reconstructedGuard(snapshot),
		Context:       context.Text,
		TraceSequence: traceSequence,
	}
	return seed
}

// reconstructedObservations rebuilds citable observations from the persisted
// tool_results rows. Only successful completed attempts with stored evidence
// become citable; the tool name and arguments come from the tool attempt row.
func reconstructedObservations(snapshot *state.RecoverySnapshot) []tools.Observation {
	byEvidence := make(map[string]state.RecoveryEvidence, len(snapshot.Evidence))
	for _, item := range snapshot.Evidence {
		byEvidence[item.EvidenceID] = item
	}
	attemptByExecution := make(map[string]state.RecoveryToolAttempt, len(snapshot.ToolAttempts))
	for _, attempt := range snapshot.ToolAttempts {
		attemptByExecution[attempt.ExecutionID] = attempt
	}
	seen := make(map[string]bool)
	var observations []tools.Observation
	for _, attempt := range snapshot.ToolAttempts {
		// Completed attempts and write attempts reconciled as verified
		// completed carry citable evidence; every other attempt does not.
		if attempt.Status != "completed" && attempt.Status != "reconciled" {
			continue
		}
		if attempt.EvidenceID == "" || seen[attempt.EvidenceID] {
			continue
		}
		evidence, ok := byEvidence[attempt.EvidenceID]
		if !ok {
			continue
		}
		seen[attempt.EvidenceID] = true
		observation := tools.Observation{
			ID:      evidence.EvidenceID,
			Tool:    evidence.Tool,
			Success: true,
		}
		decodeAny(evidence.DataJSON, &observation.Data)
		decodeAny(evidence.MetadataJSON, &observation.Metadata)
		// Fall back to the attempt row when the join produced empty tool data.
		if observation.Tool == "" {
			if attempt, ok := attemptByExecution[evidence.ExecutionID]; ok {
				observation.Tool = attempt.Tool
			}
		}
		observations = append(observations, observation)
	}
	return observations
}

// reconstructedGuard seeds the repeat guard from the workspace signatures
// recorded when historical actions were accepted. Only actions with a real
// tool attempt (executed) are seeded; prepared/reconciled attempts are not, so
// a re-proposal after an interruption executes as a new attempt.
func reconstructedGuard(snapshot *state.RecoverySnapshot) map[string]string {
	actionByID := make(map[string]state.RecoveryAction, len(snapshot.Actions))
	for _, action := range snapshot.Actions {
		actionByID[action.ActionID] = action
	}
	guard := make(map[string]string)
	for _, attempt := range snapshot.ToolAttempts {
		switch attempt.Status {
		case "completed", "failed":
			// An executed attempt (completed or deterministically failed)
			// seeds the guard: an identical proposal is rejected while the
			// workspace signature is unchanged.
		case "reconciled":
			// Only a write attempt reconciled as verified-completed was
			// executed; a replay-safe read reconciliation was not.
			if attempt.RecoveryReason != "write_effect_completed" {
				continue
			}
		default:
			continue
		}
		action, ok := actionByID[attempt.ActionID]
		if !ok || action.Fingerprint == "" {
			continue
		}
		if _, exists := guard[action.Fingerprint]; !exists {
			guard[action.Fingerprint] = action.WorkspaceSignature
		}
	}
	return guard
}

// decodeAny unmarshals a stored JSON document into value without failing the
// pipeline: an undecodable document leaves the target at its zero value.
func decodeAny(raw string, value any) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	_ = jsonUnmarshal(raw, value)
}

// jsonUnmarshal is a tiny indirection so decodeAny stays testable.
func jsonUnmarshal(raw string, value any) error {
	return json.Unmarshal([]byte(raw), value)
}
