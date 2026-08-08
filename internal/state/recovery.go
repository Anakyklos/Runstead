package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

// Recovery persistence (issue #9). Every recovery transition keeps the
// projection+journal invariant: the projection change (attempt status,
// task status, resume count) and its matching journal event commit in one
// SQLite transaction, and no transaction spans external work.

// ErrNotReconcilable is returned when a recovery transition targets an attempt
// that is already terminal or absent.
var ErrNotReconcilable = errors.New("attempt is not reconcilable")

// ErrNotResumable is returned when a task cannot be resumed because its status
// projection is terminal.
var ErrNotResumable = errors.New("task is not resumable")

// ReconcileToolAttempt is one recovery decision for an interrupted tool
// attempt.
type ReconcileToolAttempt struct {
	TaskID      string
	ExecutionID string
	// Status is the terminal recovery state: 'reconciled' or
	// 'human_review_required'.
	Status string
	// Reason is the typed recovery decision (for example
	// "replay_safe_observation").
	Reason string
}

// ReconcileProviderAttempt is one recovery decision for an interrupted
// provider attempt.
type ReconcileProviderAttempt struct {
	TaskID      string
	ExecutionID string
	// ClientRequestID is the correlation identity of the interrupted request.
	ClientRequestID string
	// Status is the terminal recovery state: 'reconciled' or
	// 'human_review_required'.
	Status string
	// Reason is the typed recovery decision (for example
	// "upstream_may_have_been_reached").
	Reason string
	// Uncertain records that the upstream may have been reached; the
	// conservative debit stands.
	Uncertain bool
	// AttemptDebited preserves the conservative debit recorded in the
	// governor ledger at TX 1.
	AttemptDebited int
	// DebitAt is the ORIGINAL permit start time of the interrupted attempt
	// (provider_attempts.prepared_at). The conservative rolling-ledger debit
	// is dated with this timestamp so the 10m/1h/3h windows represent when the
	// upstream attempt possibly happened, exactly like the governor's own
	// finishReceiptFailureLocked (which uses p.startedAt with a fallback to
	// now). Zero falls back to the reconciliation time defensively.
	DebitAt time.Time
	// ApplyConservativeDebit applies the #29 conservative accounting to the
	// persisted governor protection projection in the same transaction: the
	// task attempt count is incremented, a rolling ledger event is appended at
	// DebitAt, telemetry is marked unsafe, lastStart is moved to DebitAt and
	// telemetry.available is decremented when known. It is true only for
	// receipt-aware attempts interrupted before TX 2: StartReceiptAware defers
	// all debits to the receipt finish path, so the TX 1 projection has no
	// debit and restart must not reset that protection. Plain attempts were
	// already debited at TX 1 (Start) and receipt-aware attempts persisted as
	// 'uncertain' were already debited at TX 2, so neither re-applies.
	ApplyConservativeDebit bool
}

// MarkRecoveryStarted moves the task to 'running' (if it was still planned),
// increments the persisted resume count and appends the recovery_started
// journal event in one transaction.
func (s *Store) MarkRecoveryStarted(ctx context.Context, taskID, reason string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recovery start: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'running', resume_count = resume_count + 1 WHERE task_id = ? AND status IN ('planned', 'running')`,
		taskID)
	if err != nil {
		return fmt.Errorf("mark recovery start: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("mark recovery start: %w", ErrNotResumable)
	}
	if err := appendEvent(ctx, tx, taskID, "recovery_started", map[string]any{
		"reason": Redact(reason),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recovery start: %w", err)
	}
	hitCrashPoint("recovery_started_after")
	return nil
}

// ReconcileToolAttempt persists one tool-attempt recovery decision and its
// journal event atomically. Only non-terminal attempts are reconcilable; a
// second resume never rewrites a terminal attempt.
func (s *Store) ReconcileToolAttempt(ctx context.Context, record ReconcileToolAttempt) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tool attempt reconciliation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tool_attempts
		 SET status = ?, recovery_reason = ?, completed_at = ?
		 WHERE execution_id = ? AND task_id = ?
		   AND status IN ('planned', 'prepared', 'running', 'observed', 'verified', 'uncertain', 'verification_failed')`,
		record.Status, Redact(record.Reason), now, record.ExecutionID, record.TaskID)
	if err != nil {
		return fmt.Errorf("reconcile tool attempt: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("reconcile tool attempt %q for task %q: %w", record.ExecutionID, record.TaskID, ErrNotReconcilable)
	}
	if record.Status == "human_review_required" {
		// A tool attempt that cannot be reconciled safely blocks its logical
		// action: the action moves to human_review_required in the same
		// transaction so no future execution is created from it.
		if _, err := tx.ExecContext(ctx,
			`UPDATE actions SET status = 'human_review_required'
			 WHERE action_id = (SELECT action_id FROM tool_attempts WHERE execution_id = ?) AND status = 'prepared'`,
			record.ExecutionID); err != nil {
			return fmt.Errorf("mark action human review: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, record.TaskID, "tool_attempt_reconciled", map[string]any{
		"execution_id": record.ExecutionID,
		"status":       record.Status,
		"reason":       record.Reason,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tool attempt reconciliation: %w", err)
	}
	hitCrashPoint("recovery_tool_reconciled_after")
	return nil
}

// ReconcileProviderAttempt persists one provider-attempt recovery decision and
// its journal event atomically. The conservative debit and the
// may-have-reached-upstream marker are preserved on the row. For receipt-aware
// attempts interrupted before TX 2 the conservative #29 debit is applied to
// the persisted governor protection projection in the same transaction, so a
// restart never resets account protection for a request that may have reached
// upstream.
func (s *Store) ReconcileProviderAttempt(ctx context.Context, record ReconcileProviderAttempt) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider attempt reconciliation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE provider_attempts
		 SET status = ?, recovery_reason = ?, uncertain = ?, attempt_debited = ?, completed_at = ?
		 WHERE execution_id = ? AND task_id = ?
		   AND status IN ('planned', 'prepared', 'running', 'uncertain')`,
		record.Status, Redact(record.Reason), boolInt(record.Uncertain), record.AttemptDebited, now,
		record.ExecutionID, record.TaskID)
	if err != nil {
		return fmt.Errorf("reconcile provider attempt: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("reconcile provider attempt %q for task %q: %w", record.ExecutionID, record.TaskID, ErrNotReconcilable)
	}
	if record.ApplyConservativeDebit {
		if err := s.applyConservativeGovernorDebit(ctx, tx, record.TaskID, record.DebitAt); err != nil {
			return err
		}
	}
	if err := appendEvent(ctx, tx, record.TaskID, "provider_attempt_reconciled", map[string]any{
		"execution_id":       record.ExecutionID,
		"status":             record.Status,
		"reason":             record.Reason,
		"uncertain":          record.Uncertain,
		"attempt_debited":    record.AttemptDebited,
		"client_request_id":  record.ClientRequestID,
		"conservative_debit": record.ApplyConservativeDebit,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider attempt reconciliation: %w", err)
	}
	hitCrashPoint("recovery_provider_reconciled_after")
	return nil
}

// applyConservativeGovernorDebit applies the #29 fail-closed accounting to the
// persisted governor protection projection inside the caller's transaction,
// mirroring Permit.finishReceiptFailureLocked semantics:
//
//   - the rolling ledger event is dated with debitAt (the ORIGINAL permit
//     start, provider_attempts.prepared_at) so the 10m/1h/3h windows represent
//     when the upstream attempt possibly happened, exactly like the governor's
//     own p.startedAt fallback-to-now;
//   - lastStart is moved to debitAt;
//   - telemetry.available is decremented when known;
//   - telemetry is marked unsafe;
//   - the task attempt count is incremented (the retry count is deliberately
//     NOT incremented: recovery reconciliation is not a retry, it is the
//     conservative settlement of an uncertain attempt).
func (s *Store) applyConservativeGovernorDebit(ctx context.Context, tx *sql.Tx, taskID string, debitAt time.Time) error {
	state, ok, err := loadGovernorState(ctx, tx)
	if err != nil {
		return fmt.Errorf("load governor state for conservative debit: %w", err)
	}
	if !ok {
		state = governor.PersistedState{
			AccountPolicyID:  "runstead-cli",
			ProviderID:       "scripted",
			ModelPool:        "instant",
			AllowanceProfile: governor.ProfileInstant,
			Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		}
	}
	if debitAt.IsZero() {
		// Defensive fallback identical to finishReceiptFailureLocked: only when
		// the persisted permit start is absent/invalid.
		debitAt = time.Now().UTC()
	}
	debitAt = debitAt.UTC()
	state.Telemetry.Unsafe = true
	if state.Telemetry.Available != nil && *state.Telemetry.Available > 0 {
		value := *state.Telemetry.Available - 1
		state.Telemetry.Available = &value
	}
	state.LastStart = debitAt
	found := false
	for index := range state.TaskStates {
		if state.TaskStates[index].TaskID == taskID {
			state.TaskStates[index].Attempts++
			state.TaskStates[index].LastTouched = debitAt
			found = true
			break
		}
	}
	if !found {
		state.TaskStates = append(state.TaskStates, governor.TaskStateRecord{
			TaskID: taskID, Attempts: 1, LastTouched: debitAt,
		})
	}
	state.RollingEvents = append(state.RollingEvents, governor.LedgerEvent{At: debitAt, TaskID: taskID})
	if err := s.saveGovernorStateInTx(ctx, tx, state, taskID, s.now()); err != nil {
		return fmt.Errorf("persist conservative governor debit: %w", err)
	}
	return nil
}

// MarkHumanReviewRequired moves the task to the terminal human-review state
// with a typed stop reason and appends the task_human_review_required event.
// It is used when recovery cannot safely decide whether automatic continuation
// would duplicate an uncertain effect.
func (s *Store) MarkHumanReviewRequired(ctx context.Context, taskID, reason string, attemptIDs []string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin human review: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks
		 SET status = 'human_review_required', outcome = 'human_review_required', stop_reason = ?, finished_at = ?
		 WHERE task_id = ? AND status IN ('planned', 'running')`,
		Redact(reason), now, taskID)
	if err != nil {
		return fmt.Errorf("mark task human review: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("mark task human review: %w", ErrNotResumable)
	}
	if err := appendEvent(ctx, tx, taskID, "task_human_review_required", map[string]any{
		"reason":   Redact(reason),
		"attempts": attemptIDs,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit human review: %w", err)
	}
	hitCrashPoint("recovery_human_review_after")
	return nil
}

// AppendRecoveryEvent appends a journal-only recovery transition (for example
// recovery_continued or recovery_blocked) that has no projection change. The
// event commits in its own transaction; it is never used for transitions that
// also alter a projection (those use the typed recovery methods above).
func (s *Store) AppendRecoveryEvent(ctx context.Context, taskID, kind string, payload any) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recovery event: %w", err)
	}
	defer tx.Rollback()
	if err := appendEvent(ctx, tx, taskID, kind, payload, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recovery event: %w", err)
	}
	hitCrashPoint("recovery_event_after")
	return nil
}

// RecoverySnapshot is the authoritative persisted history one resume needs:
// the task root, logical actions, concrete attempts and citable evidence.
type RecoverySnapshot struct {
	Task             RecoveryTask
	Actions          []RecoveryAction
	ToolAttempts     []RecoveryToolAttempt
	ProviderAttempts []RecoveryProviderAttempt
	Evidence         []RecoveryEvidence
}

// RecoveryTask is the durable task root used by the recovery pipeline.
type RecoveryTask struct {
	TaskID      string
	Objective   string
	Status      string
	Workspace   string
	Model       string
	ConfigJSON  string
	ResumeCount int
}

// RecoveryAction is one logical action with its repeat/loop evidence.
type RecoveryAction struct {
	ActionID           string
	Tool               string
	ArgumentsJSON      string
	Fingerprint        string
	Status             string
	WorkspaceSignature string
}

// RecoveryToolAttempt is one concrete tool execution attempt.
type RecoveryToolAttempt struct {
	ExecutionID    string
	ActionID       string
	Tool           string
	ArgumentsJSON  string
	Status         string
	Classification string
	RecoveryClass  int
	EvidenceID     string
	RecoveryReason string
}

// RecoveryProviderAttempt is one concrete governed provider execution.
type RecoveryProviderAttempt struct {
	ExecutionID     string
	ClientRequestID string
	Status          string
	Outcome         string
	UpstreamReached bool
	Uncertain       bool
	AttemptDebited  int
	AttemptSequence int
	ReceiptAware    bool
	// PreparedAt is the ORIGINAL permit start time persisted at TX 1
	// (provider_attempts.prepared_at). Recovery dates the conservative ledger
	// debit with it so rolling windows represent when the upstream attempt
	// possibly happened, not when the resume ran.
	PreparedAt     time.Time
	RecoveryReason string
}

// RecoveryEvidence is one citable observation reconstructed from tool_results
// joined with its tool attempt (tool name and arguments live on the attempt).
type RecoveryEvidence struct {
	EvidenceID    string
	ExecutionID   string
	Tool          string
	ArgumentsJSON string
	DataJSON      string
	MetadataJSON  string
}

// LoadRecoverySnapshot loads the full persisted history one resume needs. It
// returns ErrTaskNotFound when the task row is absent.
func (s *Store) LoadRecoverySnapshot(ctx context.Context, taskID string) (*RecoverySnapshot, error) {
	snapshot := &RecoverySnapshot{}
	task, err := s.loadRecoveryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	snapshot.Task = task
	if snapshot.Actions, err = s.loadRecoveryActions(ctx, taskID); err != nil {
		return nil, err
	}
	if snapshot.ToolAttempts, err = s.loadRecoveryToolAttempts(ctx, taskID); err != nil {
		return nil, err
	}
	if snapshot.ProviderAttempts, err = s.loadRecoveryProviderAttempts(ctx, taskID); err != nil {
		return nil, err
	}
	if snapshot.Evidence, err = s.loadRecoveryEvidence(ctx, taskID); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) loadRecoveryTask(ctx context.Context, taskID string) (RecoveryTask, error) {
	var task RecoveryTask
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id, objective, status, workspace, model, config_json, resume_count
		 FROM tasks WHERE task_id = ?`, taskID).Scan(
		&task.TaskID, &task.Objective, &task.Status, &task.Workspace, &task.Model, &task.ConfigJSON, &task.ResumeCount)
	if err == sql.ErrNoRows {
		return RecoveryTask{}, ErrTaskNotFound
	}
	if err != nil {
		return RecoveryTask{}, fmt.Errorf("load recovery task: %w", err)
	}
	return task, nil
}

func (s *Store) loadRecoveryActions(ctx context.Context, taskID string) ([]RecoveryAction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_id, tool, arguments_json, fingerprint, status, workspace_signature
		 FROM actions WHERE task_id = ? ORDER BY action_sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load recovery actions: %w", err)
	}
	defer rows.Close()
	var actions []RecoveryAction
	for rows.Next() {
		var action RecoveryAction
		if err := rows.Scan(&action.ActionID, &action.Tool, &action.ArgumentsJSON, &action.Fingerprint, &action.Status, &action.WorkspaceSignature); err != nil {
			return nil, fmt.Errorf("scan recovery action: %w", err)
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (s *Store) loadRecoveryToolAttempts(ctx context.Context, taskID string) ([]RecoveryToolAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT execution_id, action_id, tool, arguments_json, status, classification, recovery_class, evidence_id, recovery_reason
		 FROM tool_attempts WHERE task_id = ? ORDER BY created_at, execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load recovery tool attempts: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryToolAttempt
	for rows.Next() {
		var attempt RecoveryToolAttempt
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ActionID, &attempt.Tool, &attempt.ArgumentsJSON,
			&attempt.Status, &attempt.Classification, &attempt.RecoveryClass, &attempt.EvidenceID, &attempt.RecoveryReason); err != nil {
			return nil, fmt.Errorf("scan recovery tool attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadRecoveryProviderAttempts(ctx context.Context, taskID string) ([]RecoveryProviderAttempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT execution_id, client_request_id, status, outcome, upstream_reached, uncertain, attempt_debited, attempt_sequence, receipt_aware, prepared_at, recovery_reason
		 FROM provider_attempts WHERE task_id = ? ORDER BY created_at, execution_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load recovery provider attempts: %w", err)
	}
	defer rows.Close()
	var attempts []RecoveryProviderAttempt
	for rows.Next() {
		var attempt RecoveryProviderAttempt
		var preparedAt string
		if err := rows.Scan(&attempt.ExecutionID, &attempt.ClientRequestID, &attempt.Status, &attempt.Outcome,
			&attempt.UpstreamReached, &attempt.Uncertain, &attempt.AttemptDebited, &attempt.AttemptSequence,
			&attempt.ReceiptAware, &preparedAt, &attempt.RecoveryReason); err != nil {
			return nil, fmt.Errorf("scan recovery provider attempt: %w", err)
		}
		attempt.PreparedAt = parseTime(preparedAt)
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Store) loadRecoveryEvidence(ctx context.Context, taskID string) ([]RecoveryEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.evidence_id, r.execution_id, t.tool, t.arguments_json, r.data_json, r.metadata_json
		 FROM tool_results r JOIN tool_attempts t ON t.execution_id = r.execution_id
		 WHERE r.task_id = ? ORDER BY r.created_at, r.evidence_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load recovery evidence: %w", err)
	}
	defer rows.Close()
	var evidence []RecoveryEvidence
	for rows.Next() {
		var item RecoveryEvidence
		if err := rows.Scan(&item.EvidenceID, &item.ExecutionID, &item.Tool, &item.ArgumentsJSON, &item.DataJSON, &item.MetadataJSON); err != nil {
			return nil, fmt.Errorf("scan recovery evidence: %w", err)
		}
		evidence = append(evidence, item)
	}
	return evidence, rows.Err()
}
