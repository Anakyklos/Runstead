package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

// ErrTaskNotFound is returned by RenderInspect when the task row is absent.
var ErrTaskNotFound = errors.New("task not found")

// ErrFinalProjectionUnavailable is returned when a task does not have a
// durable completed state backed by a passed verifier attempt. The CLI must
// never render a verified completion projection for any other state.
var ErrFinalProjectionUnavailable = errors.New("verified final projection unavailable")

type inspectTask struct {
	TaskID      string
	Objective   string
	Status      string
	Outcome     string
	StopReason  string
	Workspace   string
	Model       string
	ConfigJSON  string
	Summary     string
	ResumeCount int
	CreatedAt   string
	StartedAt   string
	FinishedAt  string
}

type inspectAction struct {
	ActionID           string
	Tool               string
	ArgumentsJSON      string
	Fingerprint        string
	Status             string
	WorkspaceSignature string
	CreatedAt          string
}

type inspectToolAttempt struct {
	ExecutionID     string
	ActionID        string
	Tool            string
	Status          string
	Classification  string
	RecoveryClass   int
	EvidenceID      string
	RecoveryReason  string
	EffectAfterHash string
	DurationNanos   int64
	CreatedAt       string
	PreparedAt      string
	CompletedAt     string
}

type inspectProviderAttempt struct {
	ExecutionID     string
	ClientRequestID string
	Provider        string
	Model           string
	Status          string
	Outcome         string
	UpstreamReached bool
	Uncertain       bool
	AttemptDebited  int
	SelectedBackoff int64
	ErrorClass      string
	RecoveryReason  string
	ReceiptCount    int
	CreatedAt       string
	PreparedAt      string
	CompletedAt     string
}

type inspectReceipt struct {
	ReceiptAttemptID string
	ExecutionID      string
	Sequence         int
	Provider         string
	Model            string
	StartedAt        string
	CompletedAt      string
	Outcome          string
	Trigger          string
	UpstreamReached  bool
}

// inspectProjection is the shared durable view used by both the historical
// inspect renderer and the completion-only final renderer. Every field is
// loaded from the same persisted projections and reports; no renderer derives
// authority from model output.
type inspectProjection struct {
	task                 inspectTask
	events               []eventRow
	actions              []inspectAction
	toolAttempts         []inspectToolAttempt
	providerAttempts     []inspectProviderAttempt
	receipts             []inspectReceipt
	writeDecisions       []inspectWritePolicyDecision
	approvals            []inspectApproval
	pending              []PendingApproval
	processEvidence      []inspectProcessEvidence
	verificationAttempts []VerificationAttemptRow
	governorState        *inspectGovernor
}

// RenderInspect writes a stable, human-readable reconstruction of one task.
// It never dumps raw SQLite rows: the output is fixed-section, ordered and
// sanitized, and every identifier shown comes from the persisted projection.
func (s *Store) RenderInspect(ctx context.Context, out io.Writer, taskID string) error {
	projection, err := s.loadInspectProjection(ctx, taskID)
	if err != nil {
		return err
	}
	task := projection.task
	events := projection.events
	actions := projection.actions
	toolAttempts := projection.toolAttempts
	providerAttempts := projection.providerAttempts
	receipts := projection.receipts
	writeDecisions := projection.writeDecisions
	approvals := projection.approvals
	pending := projection.pending
	processEvidence := projection.processEvidence
	verificationAttempts := projection.verificationAttempts
	governorState := projection.governorState

	var builder strings.Builder
	fmt.Fprintf(&builder, "Task: %s\n", task.TaskID)
	fmt.Fprintf(&builder, "Objective: %s\n", task.Objective)
	fmt.Fprintf(&builder, "Status: %s\n", task.Status)
	if task.ResumeCount > 0 {
		fmt.Fprintf(&builder, "Resumes: %d\n", task.ResumeCount)
	}
	if task.Outcome != "" {
		fmt.Fprintf(&builder, "Outcome: %s\n", task.Outcome)
	}
	if task.StopReason != "" {
		fmt.Fprintf(&builder, "Stop reason: %s\n", task.StopReason)
	}
	fmt.Fprintf(&builder, "Workspace: %s\n", task.Workspace)
	if task.Model != "" {
		fmt.Fprintf(&builder, "Model: %s\n", task.Model)
	}
	fmt.Fprintf(&builder, "Created: %s\n", task.CreatedAt)
	if task.StartedAt != "" {
		fmt.Fprintf(&builder, "Started: %s\n", task.StartedAt)
	}
	if task.FinishedAt != "" {
		fmt.Fprintf(&builder, "Finished: %s\n", task.FinishedAt)
	}
	if task.Summary != "" {
		fmt.Fprintf(&builder, "Summary: %s\n", task.Summary)
	}

	builder.WriteString("\nConfiguration:\n")
	if config, ok := renderConfig(task.ConfigJSON); ok {
		for _, line := range config {
			fmt.Fprintf(&builder, "  %s\n", line)
		}
	} else {
		builder.WriteString("  (none recorded)\n")
	}

	builder.WriteString("\nEvents:\n")
	if len(events) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, event := range events {
		fmt.Fprintf(&builder, "  %d. %s at %s\n", event.Sequence, event.Kind, event.CreatedAt)
		if payload := compactEventPayload(event.Payload); payload != "" {
			fmt.Fprintf(&builder, "     %s\n", payload)
		}
	}

	builder.WriteString("\nActions:\n")
	if len(actions) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, action := range actions {
		fmt.Fprintf(&builder, "  %s %s status=%s\n", action.ActionID, action.Tool, action.Status)
		if fingerprint := action.Fingerprint; fingerprint != "" {
			fmt.Fprintf(&builder, "    fingerprint=%s\n", shortID(fingerprint))
		}
	}

	builder.WriteString("\nTool attempts:\n")
	if len(toolAttempts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, attempt := range toolAttempts {
		fmt.Fprintf(&builder, "  %s tool=%s action=%s status=%s\n", attempt.ExecutionID, attempt.Tool, attempt.ActionID, attempt.Status)
		if attempt.Classification != "" {
			fmt.Fprintf(&builder, "    classification=%s\n", attempt.Classification)
		}
		if attempt.EvidenceID != "" {
			fmt.Fprintf(&builder, "    evidence=%s\n", attempt.EvidenceID)
		}
		if attempt.EffectAfterHash != "" {
			fmt.Fprintf(&builder, "    effect_after_hash=%s\n", shortID(attempt.EffectAfterHash))
		}
		if attempt.Status == "prepared" {
			fmt.Fprintf(&builder, "    uncertain=prepared: the effect may have started; reconcile before re-execution\n")
		}
		if attempt.RecoveryReason != "" {
			fmt.Fprintf(&builder, "    recovery=%s\n", attempt.RecoveryReason)
		}
		if attempt.DurationNanos > 0 {
			fmt.Fprintf(&builder, "    duration=%dns\n", attempt.DurationNanos)
		}
	}

	builder.WriteString("\nProvider attempts:\n")
	if len(providerAttempts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, attempt := range providerAttempts {
		fmt.Fprintf(&builder, "  %s request=%s provider=%s model=%s status=%s\n", attempt.ExecutionID, attempt.ClientRequestID, attempt.Provider, attempt.Model, attempt.Status)
		if attempt.Outcome != "" {
			fmt.Fprintf(&builder, "    outcome=%s upstream_reached=%t\n", attempt.Outcome, attempt.UpstreamReached)
		}
		if attempt.Uncertain || attempt.Status == "uncertain" {
			fmt.Fprintf(&builder, "    uncertain=yes: the upstream may have been reached; never auto-retry\n")
		}
		if attempt.AttemptDebited > 0 {
			fmt.Fprintf(&builder, "    debited=%d\n", attempt.AttemptDebited)
		}
		if attempt.ErrorClass != "" {
			fmt.Fprintf(&builder, "    receipt_error=%s\n", attempt.ErrorClass)
		}
		if attempt.Status == "prepared" {
			fmt.Fprintf(&builder, "    uncertain=prepared: the upstream may have been reached; reconcile before re-execution\n")
		}
		if attempt.RecoveryReason != "" {
			fmt.Fprintf(&builder, "    recovery=%s\n", attempt.RecoveryReason)
		}
	}

	builder.WriteString("\nProvider attempt receipts:\n")
	if len(receipts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, receipt := range receipts {
		fmt.Fprintf(&builder, "  receipt=%s execution=%s sequence=%d outcome=%s trigger=%s upstream=%t\n",
			receipt.ReceiptAttemptID, receipt.ExecutionID, receipt.Sequence, receipt.Outcome, receipt.Trigger, receipt.UpstreamReached)
	}

	builder.WriteString("\nPolicy decisions:\n")
	if len(writeDecisions) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, decision := range writeDecisions {
		fmt.Fprintf(&builder, "  %s %s tool=%s decision=%s reason=%s\n", decision.CreatedAt, decision.ActionID, decision.Tool, decision.Decision, decision.Reason)
	}

	builder.WriteString("\nApprovals:\n")
	if len(approvals) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, approval := range approvals {
		fmt.Fprintf(&builder, "  %s action=%s decision=%s actor=%s reason=%s\n", approval.CreatedAt, approval.ActionID, approval.Decision, approval.Actor, approval.Reason)
	}

	builder.WriteString("\nPending approvals:\n")
	if len(pending) == 0 {
		builder.WriteString("  (none)\n")
	} else {
		for _, item := range pending {
			fmt.Fprintf(&builder, "  action=%s tool=%s awaiting operator decision\n", item.ActionID, item.Tool)
		}
		builder.WriteString("  decide with: runstead decide <task-id> <action-id> approved|rejected\n")
	}

	builder.WriteString("\nProcess attempts:\n")
	renderProcessEvidence(&builder, processEvidence)

	builder.WriteString("\nVerification:\n")
	if len(verificationAttempts) == 0 {
		builder.WriteString("  (none)\n")
	}
	for _, attempt := range verificationAttempts {
		fmt.Fprintf(&builder, "  %s decision=%s summary=%s\n", attempt.AttemptID, attempt.Decision, attempt.Summary)
		for _, check := range attempt.Checks {
			fmt.Fprintf(&builder, "    check=%s type=%s status=%s\n", check.CheckID, check.Type, check.Status)
			if check.Expected != "" {
				fmt.Fprintf(&builder, "      expected: %s\n", boundedRender(check.Expected))
			}
			if check.Observed != "" {
				fmt.Fprintf(&builder, "      observed: %s\n", boundedRender(check.Observed))
			}
			if len(check.Evidence) > 0 {
				fmt.Fprintf(&builder, "      evidence: %s\n", strings.Join(check.Evidence, ","))
			}
			if check.Reason != "" {
				fmt.Fprintf(&builder, "      reason: %s\n", boundedRender(check.Reason))
			}
		}
	}

	builder.WriteString("\nGit observation:\n")
	renderGitObservation(&builder, verificationAttempts)

	builder.WriteString("\nGovernor state:\n")
	if governorState == nil {
		builder.WriteString("  (none recorded)\n")
	} else {
		fmt.Fprintf(&builder, "  account=%s provider=%s model_pool=%s\n", governorState.AccountPolicyID, governorState.ProviderID, governorState.ModelPool)
		fmt.Fprintf(&builder, "  upstream allowance: %s profile=%s\n", governorState.AllowanceKind, governorState.AllowanceProfile)
		switch governorState.AllowanceKind {
		case governor.AllowanceKindUnlimitedText:
			builder.WriteString("  no published numeric rolling quota (explicitly configured unlimited text)\n")
		case governor.AllowanceKindUnknown:
			builder.WriteString("  no published numeric rolling quota (no evidence; explicit local conservative ceilings still enforced)\n")
			fmt.Fprintf(&builder, "  rolling usage: 10m=%d/%d 1h=%d/%d 3h=%d/%d\n",
				governorState.Rolling10m, governorState.Rolling10mCeiling, governorState.Rolling1h, governorState.Rolling1hCeiling, governorState.Rolling3h, governorState.Rolling3hCeiling)
			fmt.Fprintf(&builder, "  task attempts: %d/%d\n", governorState.TaskUsed, governorState.TaskCeiling)
			if governorState.ManualReserve > 0 {
				fmt.Fprintf(&builder, "  manual reserve: %d (%d remaining)\n", governorState.ManualReserve, governorState.ManualReserveRemaining)
			}
		default:
			fmt.Fprintf(&builder, "  rolling usage: 10m=%d/%d 1h=%d/%d 3h=%d/%d\n",
				governorState.Rolling10m, governorState.Rolling10mCeiling, governorState.Rolling1h, governorState.Rolling1hCeiling, governorState.Rolling3h, governorState.Rolling3hCeiling)
			fmt.Fprintf(&builder, "  task attempts: %d/%d\n", governorState.TaskUsed, governorState.TaskCeiling)
			if governorState.ManualReserve > 0 {
				fmt.Fprintf(&builder, "  manual reserve: %d (%d remaining)\n", governorState.ManualReserve, governorState.ManualReserveRemaining)
			}
		}
		fmt.Fprintf(&builder, "  local workload ceilings: task=%d retry=%d\n", governorState.TaskCeiling, governorState.RetryCeiling)
		builder.WriteString("  serialized lane, start-to-start pacing, queue/fairness, cooldown and circuit breakers remain active for every allowance kind\n")
		if governorState.CooldownUntil != "" {
			fmt.Fprintf(&builder, "  cooldown until: %s\n", governorState.CooldownUntil)
		} else {
			builder.WriteString("  cooldown until: none\n")
		}
		fmt.Fprintf(&builder, "  circuit: %s\n", governorState.CircuitState)
		if governorState.CircuitReason != "" {
			fmt.Fprintf(&builder, "  circuit reason: %s\n", governorState.CircuitReason)
		}
		if governorState.TelemetryUnsafe {
			builder.WriteString("  telemetry: unsafe (conservative accounting is active)\n")
		}
	}

	if _, err := io.WriteString(out, builder.String()); err != nil {
		return fmt.Errorf("write inspect output: %w", err)
	}
	return nil
}

// RenderFinal writes the bounded verified-runtime projection of a completed
// task. It refuses to render a completion report unless the durable task row
// and the latest persisted verifier attempt agree that completion passed.
func (s *Store) RenderFinal(ctx context.Context, out io.Writer, taskID string) error {
	projection, err := s.loadInspectProjection(ctx, taskID)
	if err != nil {
		return err
	}
	if projection.task.Status != "completed" || projection.task.Outcome != "completed" {
		return fmt.Errorf("%w: task status=%s outcome=%s", ErrFinalProjectionUnavailable, projection.task.Status, projection.task.Outcome)
	}
	if len(projection.verificationAttempts) == 0 {
		return fmt.Errorf("%w: no verification attempt", ErrFinalProjectionUnavailable)
	}
	latest := projection.verificationAttempts[len(projection.verificationAttempts)-1]
	if latest.Decision != "passed" {
		return fmt.Errorf("%w: latest verification decision=%s", ErrFinalProjectionUnavailable, latest.Decision)
	}

	var builder strings.Builder
	builder.WriteString("Verified runtime result:\n")
	fmt.Fprintf(&builder, "  task: %s\n", projection.task.TaskID)
	fmt.Fprintf(&builder, "  status: %s\n", projection.task.Status)
	fmt.Fprintf(&builder, "  outcome: %s\n", projection.task.Outcome)
	fmt.Fprintf(&builder, "  verifier: %s\n", latest.Decision)
	fmt.Fprintf(&builder, "  summary: %s\n", boundedRender(latest.Summary))

	builder.WriteString("  acceptance checks:\n")
	if len(latest.Checks) == 0 {
		builder.WriteString("    (none)\n")
	}
	for _, check := range latest.Checks {
		fmt.Fprintf(&builder, "    check=%s type=%s status=%s\n", check.CheckID, check.Type, check.Status)
		if check.Expected != "" {
			fmt.Fprintf(&builder, "      expected: %s\n", boundedRender(check.Expected))
		}
		if check.Observed != "" {
			fmt.Fprintf(&builder, "      observed: %s\n", boundedRender(check.Observed))
		}
		if len(check.Evidence) > 0 {
			fmt.Fprintf(&builder, "      evidence: %s\n", strings.Join(check.Evidence, ","))
		}
		if check.Reason != "" {
			fmt.Fprintf(&builder, "      reason: %s\n", boundedRender(check.Reason))
		}
	}

	builder.WriteString("  evidence IDs:\n")
	hasEvidence := false
	for _, attempt := range projection.toolAttempts {
		if attempt.EvidenceID == "" {
			continue
		}
		hasEvidence = true
		fmt.Fprintf(&builder, "    %s tool=%s\n", attempt.EvidenceID, attempt.Tool)
	}
	if !hasEvidence {
		builder.WriteString("    (none)\n")
	}

	builder.WriteString("  Git observation:\n")
	renderGitObservation(&builder, projection.verificationAttempts)
	builder.WriteString("  Process results:\n")
	renderProcessEvidence(&builder, projection.processEvidence)

	if _, err := io.WriteString(out, builder.String()); err != nil {
		return fmt.Errorf("write final output: %w", err)
	}
	return nil
}

func (s *Store) loadInspectProjection(ctx context.Context, taskID string) (inspectProjection, error) {
	task, err := s.loadInspectTask(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	events, err := s.loadEvents(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	actions, err := s.loadInspectActions(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	toolAttempts, err := s.loadInspectToolAttempts(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	providerAttempts, err := s.loadInspectProviderAttempts(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	receipts, err := s.loadInspectReceipts(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	writeDecisions, err := s.loadInspectWritePolicyDecisions(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	approvals, err := s.loadInspectApprovals(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	pending, err := s.PendingApprovals(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	processEvidence, err := s.loadInspectProcessEvidence(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	verificationAttempts, err := s.VerificationAttempts(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	governorState, err := s.loadInspectGovernor(ctx, taskID)
	if err != nil {
		return inspectProjection{}, err
	}
	return inspectProjection{
		task:                 task,
		events:               events,
		actions:              actions,
		toolAttempts:         toolAttempts,
		providerAttempts:     providerAttempts,
		receipts:             receipts,
		writeDecisions:       writeDecisions,
		approvals:            approvals,
		pending:              pending,
		processEvidence:      processEvidence,
		verificationAttempts: verificationAttempts,
		governorState:        governorState,
	}, nil
}

func (s *Store) loadInspectTask(ctx context.Context, taskID string) (inspectTask, error) {
	var task inspectTask
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id, objective, status, outcome, stop_reason, workspace, model, config_json, summary, resume_count, created_at, started_at, finished_at
		 FROM tasks WHERE task_id = ?`, taskID).Scan(
		&task.TaskID, &task.Objective, &task.Status, &task.Outcome, &task.StopReason,
		&task.Workspace, &task.Model, &task.ConfigJSON, &task.Summary, &task.ResumeCount, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	if err == sql.ErrNoRows {
		return inspectTask{}, ErrTaskNotFound
	}
	if err != nil {
		return inspectTask{}, fmt.Errorf("load task: %w", err)
	}
	return task, nil
}

func (s *Store) loadInspectActions(ctx context.Context, taskID string) ([]inspectAction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, tool, arguments_json, fingerprint, status, workspace_signature, created_at
		 FROM actions WHERE task_id = ? ORDER BY action_sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load actions: %w", err)
	}
	defer rows.Close()
	var actions []inspectAction
	for rows.Next() {
		var action inspectAction
		if err := rows.Scan(&action.ActionID, &action.Tool, &action.ArgumentsJSON, &action.Fingerprint, &action.Status, &action.WorkspaceSignature, &action.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (s *Store) loadInspectToolAttempts(ctx context.Context, taskID string) ([]inspectToolAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT execution_id, action_id, tool, status, classification, recovery_class, evidence_id, recovery_reason, effect_after_hash, duration_ns, created_at, prepared_at, completed_at
		 FROM tool_attempts WHERE task_id = ? ORDER BY created_at, execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load tool attempts: %w", err)
	}
	defer rows.Close()
	var attempts []inspectToolAttempt
	for rows.Next() {
		var attempt inspectToolAttempt
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ActionID, &attempt.Tool, &attempt.Status, &attempt.Classification,
			&attempt.RecoveryClass, &attempt.EvidenceID, &attempt.RecoveryReason, &attempt.EffectAfterHash, &attempt.DurationNanos, &attempt.CreatedAt, &attempt.PreparedAt, &attempt.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan tool attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadInspectProviderAttempts(ctx context.Context, taskID string) ([]inspectProviderAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.execution_id, p.client_request_id, p.provider, p.model, p.status, p.outcome, p.upstream_reached,
		        p.uncertain, p.attempt_debited, p.selected_backoff_ns, p.error_class, p.recovery_reason,
		        (SELECT COUNT(*) FROM provider_attempt_receipts r WHERE r.execution_id = p.execution_id),
		        p.created_at, p.prepared_at, p.completed_at
		 FROM provider_attempts p WHERE p.task_id = ? ORDER BY p.created_at, p.execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load provider attempts: %w", err)
	}
	defer rows.Close()
	var attempts []inspectProviderAttempt
	for rows.Next() {
		var attempt inspectProviderAttempt
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ClientRequestID, &attempt.Provider, &attempt.Model, &attempt.Status,
			&attempt.Outcome, &attempt.UpstreamReached, &attempt.Uncertain, &attempt.AttemptDebited,
			&attempt.SelectedBackoff, &attempt.ErrorClass, &attempt.RecoveryReason, &attempt.ReceiptCount, &attempt.CreatedAt,
			&attempt.PreparedAt, &attempt.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan provider attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadInspectReceipts(ctx context.Context, taskID string) ([]inspectReceipt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT receipt_attempt_id, execution_id, sequence, provider, model, started_at, completed_at, outcome, trigger, upstream_reached
		 FROM provider_attempt_receipts WHERE task_id = ? ORDER BY execution_id, sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load receipts: %w", err)
	}
	defer rows.Close()
	var receipts []inspectReceipt
	for rows.Next() {
		var receipt inspectReceipt
		if err := rows.Scan(&receipt.ReceiptAttemptID, &receipt.ExecutionID, &receipt.Sequence, &receipt.Provider,
			&receipt.Model, &receipt.StartedAt, &receipt.CompletedAt, &receipt.Outcome, &receipt.Trigger,
			&receipt.UpstreamReached); err != nil {
			return nil, fmt.Errorf("scan receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

type inspectWritePolicyDecision struct {
	ActionID  string
	Tool      string
	Decision  string
	Reason    string
	CreatedAt string
}

type inspectApproval struct {
	ActionID  string
	Decision  string
	Reason    string
	Actor     string
	CreatedAt string
}

// inspectProcessEvidence is the bounded render of one run_recipe attempt's
// persisted evidence.
type inspectProcessEvidence struct {
	ExecutionID      string
	EvidenceID       string
	RecipeID         string
	ExitCode         int
	Signal           string
	DurationNanos    int64
	TimedOut         bool
	Canceled         bool
	StdoutTruncated  bool
	StderrTruncated  bool
	NetworkIsolation string
	CreatedAt        string
}

// loadInspectProcessEvidence loads the citable process evidence of one task:
// tool_results rows for completed run_recipe attempts, joined with their
// execution id. Output content is never rendered; only the bounded summary
// fields.
func (s *Store) loadInspectProcessEvidence(ctx context.Context, taskID string) ([]inspectProcessEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.execution_id, r.evidence_id, r.data_json, r.created_at
		 FROM tool_results r JOIN tool_attempts t ON t.execution_id = r.execution_id
		 WHERE r.task_id = ? AND t.tool = 'run_recipe'
		 ORDER BY r.created_at, r.evidence_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load process evidence: %w", err)
	}
	defer rows.Close()
	var items []inspectProcessEvidence
	for rows.Next() {
		var item inspectProcessEvidence
		var dataJSON, createdAt string
		if err := rows.Scan(&item.ExecutionID, &item.EvidenceID, &dataJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan process evidence: %w", err)
		}
		var evidence struct {
			RecipeID         string `json:"recipe_id"`
			ExitCode         int    `json:"exit_code"`
			Signal           string `json:"signal"`
			DurationNanos    int64  `json:"duration_nanos"`
			TimedOut         bool   `json:"timed_out"`
			Canceled         bool   `json:"canceled"`
			StdoutTruncated  bool   `json:"stdout_truncated"`
			StderrTruncated  bool   `json:"stderr_truncated"`
			NetworkIsolation string `json:"network_isolation"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &evidence); err != nil {
			// An undecodable evidence row still renders as an attempt with
			// unknown details rather than failing the whole inspection.
			item.CreatedAt = createdAt
			items = append(items, item)
			continue
		}
		item.RecipeID = evidence.RecipeID
		item.ExitCode = evidence.ExitCode
		item.Signal = evidence.Signal
		item.DurationNanos = evidence.DurationNanos
		item.TimedOut = evidence.TimedOut
		item.Canceled = evidence.Canceled
		item.StdoutTruncated = evidence.StdoutTruncated
		item.StderrTruncated = evidence.StderrTruncated
		item.NetworkIsolation = evidence.NetworkIsolation
		item.CreatedAt = createdAt
		items = append(items, item)
	}
	return items, rows.Err()
}

func renderProcessEvidence(builder *strings.Builder, items []inspectProcessEvidence) {
	if len(items) == 0 {
		builder.WriteString("  (none)\n")
		return
	}
	for _, item := range items {
		fmt.Fprintf(builder, "  %s execution=%s recipe=%s evidence=%s exit=%d truncated=stdout:%t/stderr:%t",
			item.CreatedAt, item.ExecutionID, item.RecipeID, item.EvidenceID, item.ExitCode,
			item.StdoutTruncated, item.StderrTruncated)
		if item.Signal != "" {
			fmt.Fprintf(builder, " signal=%s", item.Signal)
		}
		if item.DurationNanos > 0 {
			fmt.Fprintf(builder, " duration=%dns", item.DurationNanos)
		}
		if item.TimedOut {
			builder.WriteString(" timed_out=yes")
		}
		if item.Canceled {
			builder.WriteString(" canceled=yes")
		}
		fmt.Fprintf(builder, " network_isolation=%s\n", item.NetworkIsolation)
	}
}

func (s *Store) loadInspectWritePolicyDecisions(ctx context.Context, taskID string) ([]inspectWritePolicyDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, tool, decision, reason, created_at FROM write_policy_decisions WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load write policy decisions: %w", err)
	}
	defer rows.Close()
	var decisions []inspectWritePolicyDecision
	for rows.Next() {
		var decision inspectWritePolicyDecision
		if err := rows.Scan(&decision.ActionID, &decision.Tool, &decision.Decision, &decision.Reason, &decision.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan write policy decision: %w", err)
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

func (s *Store) loadInspectApprovals(ctx context.Context, taskID string) ([]inspectApproval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, decision, reason, actor, created_at FROM approvals WHERE task_id = ? ORDER BY created_at, action_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load approvals: %w", err)
	}
	defer rows.Close()
	var approvals []inspectApproval
	for rows.Next() {
		var approval inspectApproval
		if err := rows.Scan(&approval.ActionID, &approval.Decision, &approval.Reason, &approval.Actor, &approval.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan approval: %w", err)
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

type inspectGovernor struct {
	AccountPolicyID        string
	ProviderID             string
	ModelPool              string
	AllowanceProfile       string
	AllowanceKind          governor.AllowanceKind
	Rolling3h              int
	Rolling1h              int
	Rolling10m             int
	Rolling3hCeiling       int
	Rolling1hCeiling       int
	Rolling10mCeiling      int
	TaskUsed               int
	TaskCeiling            int
	RetryCeiling           int
	ManualReserve          int
	ManualReserveRemaining int
	CooldownUntil          string
	CircuitState           string
	CircuitReason          string
	TelemetryUnsafe        bool
}

func (s *Store) loadInspectGovernor(ctx context.Context, taskID string) (*inspectGovernor, error) {
	var info inspectGovernor
	var cooldown, allowanceProfile string
	var telemetryAvailable sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT account_policy_id, provider_id, model_pool, allowance_profile, cooldown_until, circuit_state,
		        circuit_reason, telemetry_unsafe, telemetry_available,
		        rolling_3h_ceiling, rolling_1h_ceiling, rolling_10m_ceiling, task_budget_ceiling,
		        retry_budget_ceiling, manual_reserve_ceiling
		 FROM governor_state WHERE id = 1`).Scan(
		&info.AccountPolicyID, &info.ProviderID, &info.ModelPool, &allowanceProfile, &cooldown,
		&info.CircuitState, &info.CircuitReason, &info.TelemetryUnsafe, &telemetryAvailable,
		&info.Rolling3hCeiling, &info.Rolling1hCeiling, &info.Rolling10mCeiling, &info.TaskCeiling,
		&info.RetryCeiling, &info.ManualReserve)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load governor state: %w", err)
	}
	info.AllowanceProfile = allowanceProfile
	info.AllowanceKind = governor.AllowanceKindForProfile(governor.AllowanceProfile(allowanceProfile))
	info.CooldownUntil = cooldown
	info.ManualReserveRemaining = info.ManualReserve
	if telemetryAvailable.Valid && int(telemetryAvailable.Int64) < info.ManualReserveRemaining {
		info.ManualReserveRemaining = int(telemetryAvailable.Int64)
	}
	now := s.clock.Now()
	var ledgerRaw sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT group_concat(at, '|') FROM governor_ledger`).Scan(&ledgerRaw); err != nil {
		return nil, fmt.Errorf("load governor ledger: %w", err)
	}
	for _, raw := range strings.Split(ledgerRaw.String, "|") {
		at := parseTime(raw)
		if at.IsZero() {
			continue
		}
		if now.Sub(at) <= 3*time.Hour {
			info.Rolling3h++
		}
		if now.Sub(at) <= time.Hour {
			info.Rolling1h++
		}
		if now.Sub(at) <= 10*time.Minute {
			info.Rolling10m++
		}
	}
	// The inspected task's own attempt usage, not the number of tasks the
	// governor has retained.
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts FROM governor_task_states WHERE task_id = ?`, taskID).Scan(&info.TaskUsed); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load governor task usage: %w", err)
	}
	return &info, nil
}

// renderConfig parses the sanitized configuration snapshot into stable lines.
func renderConfig(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil, false
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, false
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s: %v", key, values[key]))
	}
	return lines, true
}

// compactEventPayload renders one journal payload as a single stable line.
func compactEventPayload(payload string) string {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "{}" {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(payload), &values); err != nil {
		return payload
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString("{")
	for index, key := range keys {
		if index > 0 {
			builder.WriteString(" ")
		}
		fmt.Fprintf(&builder, "%s=%v", key, values[key])
	}
	builder.WriteString("}")
	return builder.String()
}

func shortID(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:16] + "..."
}

// boundedRender caps one verification description line for human rendering so
// inspect output stays bounded even when a persisted description is long.
const maxInspectLine = 200

func boundedRender(value string) string {
	if len(value) <= maxInspectLine {
		return value
	}
	return value[:maxInspectLine] + "...(truncated)"
}

// renderGitObservation renders the bounded real git observation captured by
// the latest control-plane verification report (issue #12 final evidence):
// whether git was available, the pre-existing vs during-task changed files,
// and the explicit baseline-truncation limitation. The report is persisted
// verifier JSON; state renders it without importing the verifier package.
func renderGitObservation(builder *strings.Builder, attempts []VerificationAttemptRow) {
	if len(attempts) == 0 {
		builder.WriteString("  (no verification attempt)\n")
		return
	}
	latest := attempts[len(attempts)-1]
	var report struct {
		Git *struct {
			CurrentStatus     string `json:"current_status"`
			CurrentDiff       string `json:"current_diff,omitempty"`
			Truncated         bool   `json:"truncated"`
			Available         bool   `json:"available"`
			Failure           string `json:"failure,omitempty"`
			BaselineTruncated bool   `json:"baseline_truncated,omitempty"`
			PreExisting       []struct {
				Path   string `json:"path"`
				Status string `json:"status,omitempty"`
			} `json:"pre_existing,omitempty"`
			DuringTask []struct {
				Path   string `json:"path"`
				Status string `json:"status,omitempty"`
			} `json:"during_task,omitempty"`
		} `json:"git,omitempty"`
	}
	if err := json.Unmarshal([]byte(latest.ReportJSON), &report); err != nil || report.Git == nil {
		builder.WriteString("  (unavailable)\n")
		return
	}
	git := report.Git
	if !git.Available {
		failure := git.Failure
		if failure == "" {
			failure = "git observation unavailable"
		}
		fmt.Fprintf(builder, "  unavailable: %s\n", boundedRender(failure))
		return
	}
	builder.WriteString("  available: yes\n")
	if git.CurrentStatus != "" {
		fmt.Fprintf(builder, "  status: %s\n", boundedRender(singleLine(git.CurrentStatus)))
	}
	builder.WriteString("  pre-existing changes: " + renderChangedFiles(git.PreExisting) + "\n")
	builder.WriteString("  during-task changes: " + renderChangedFiles(git.DuringTask) + "\n")
	diff := singleLine(git.CurrentDiff)
	if diff == "" {
		builder.WriteString("  Git diff (bounded): (none)\n")
	} else {
		fmt.Fprintf(builder, "  Git diff (bounded): %s\n", boundedRender(diff))
	}
	fmt.Fprintf(builder, "  diff truncated: %t\n", git.Truncated || len(diff) > maxInspectLine)
	if git.BaselineTruncated {
		builder.WriteString("  limitation: the task-start git baseline was truncated; pre-existing changes outside the truncated baseline window may be attributed as during-task\n")
	}
}

func renderChangedFiles(files []struct {
	Path   string `json:"path"`
	Status string `json:"status,omitempty"`
}) string {
	if len(files) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(files))
	for _, file := range files {
		if file.Status == "" {
			parts = append(parts, file.Path)
		} else {
			parts = append(parts, file.Path+" ("+file.Status+")")
		}
	}
	return strings.Join(parts, ", ")
}

// singleLine collapses embedded newlines so a bounded status render stays one
// line.
func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
