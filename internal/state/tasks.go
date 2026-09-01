package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrOpenWorkUnitsBlockFinalize is returned by FinalizeTask when a task would
// be persisted as 'completed' while persisted Work Units are still open. The
// parent completion gate lives at the state layer (issue #106): omitting
// --workunits on resume can never bypass it.
var ErrOpenWorkUnitsBlockFinalize = errors.New("task has open work units; parent completion is gated")

// terminalStatus maps a typed agent outcome to the persisted task status
// projection. `completed` maps to completed; `canceled` maps to canceled;
// every other terminal loop outcome is a typed failure.
func terminalStatus(outcome string) string {
	switch outcome {
	case "completed":
		return "completed"
	case "canceled":
		return "canceled"
	default:
		return "failed"
	}
}

// CreateTask persists a new task in status 'planned' with its task_created
// event in one transaction.
func (s *Store) CreateTask(ctx context.Context, task TaskRecord) error {
	if err := validateExecutionContractPair(string(task.ExecutionContractJSON), task.ExecutionContractHash); err != nil {
		return fmt.Errorf("validate execution contract: %w", err)
	}
	createdAt := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task creation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (task_id, objective, status, workspace, model, config_json, execution_contract_json, execution_contract_hash, created_at, started_at)
		 VALUES (?, ?, 'planned', ?, ?, ?, ?, ?, ?, ?)`,
		task.TaskID, Redact(task.Objective), Redact(task.Workspace), Redact(task.Model),
		string(RedactJSON(task.ConfigJSON)), string(task.ExecutionContractJSON), task.ExecutionContractHash, createdAt, createdAt); err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	if err := appendEvent(ctx, tx, task.TaskID, "task_created", map[string]any{
		"status": "planned",
	}, createdAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task creation: %w", err)
	}
	hitCrashPoint("task_created_after")
	return nil
}

// StartTask moves a task from 'planned' to 'running' and appends task_started
// in one transaction.
func (s *Store) StartTask(ctx context.Context, taskID string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task start: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'running', started_at = ? WHERE task_id = ? AND status = 'planned'`,
		now, taskID)
	if err != nil {
		return fmt.Errorf("start task: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("start task: task %q does not exist or is not planned", taskID)
	}
	if err := appendEvent(ctx, tx, taskID, "task_started", map[string]any{
		"status": "running",
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task start: %w", err)
	}
	hitCrashPoint("task_started_after")
	return nil
}

// FinalizeTask persists the terminal outcome, summary and evidence, updates
// the task status projection and appends task_finalized in one transaction.
// A task with a pending write approval can never be finalized as completed:
// the state machine refuses the transition (ErrPendingApprovals) so a
// mandatory write is never silently skipped around a completed task.
func (s *Store) FinalizeTask(ctx context.Context, record TaskFinalize) error {
	now := s.now()
	status := terminalStatus(record.Outcome)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task finalize: %w", err)
	}
	defer tx.Rollback()
	if status == "completed" {
		// Work Unit completion gate (issue #106 review): a task with persisted
		// Work Units that are not all completed can NEVER be persisted as
		// 'completed'. The check lives at the state layer so no alternate
		// code path (resume without --workunits, a future scheduling change)
		// can finalize the parent around open units.
		var openUnits int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_units WHERE task_id = ? AND status != 'completed'`,
			record.TaskID).Scan(&openUnits); err != nil {
			return fmt.Errorf("check open work units before finalize: %w", err)
		}
		if openUnits > 0 {
			return fmt.Errorf("finalize task %q as completed: %w (%d open work unit(s))", record.TaskID, ErrOpenWorkUnitsBlockFinalize, openUnits)
		}
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*)
			 FROM write_policy_decisions d
			 JOIN actions a ON a.task_id = d.task_id AND a.action_id = d.action_id
			 WHERE d.task_id = ? AND d.decision = 'approval_required'
			   AND NOT EXISTS (
			       SELECT 1 FROM approvals ap
			       WHERE ap.task_id = d.task_id AND ap.fingerprint = `+effectiveFingerprintExpr+`
			   )`, record.TaskID).Scan(&pending); err != nil {
			return fmt.Errorf("check pending approvals before finalize: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("finalize task %q as completed: %w (%d)", record.TaskID, ErrPendingApprovals, pending)
		}
		// Issue #11 completion gate: a task may only be persisted as
		// 'completed' when the latest control-plane verification attempt
		// decided 'passed'. This defends the invariant at the state layer, so
		// no alternate code path can persist completed without a valid
		// verification.
		var verificationDecision string
		err := tx.QueryRowContext(ctx,
			`SELECT decision FROM verification_attempts WHERE task_id = ? ORDER BY sequence DESC LIMIT 1`,
			record.TaskID).Scan(&verificationDecision)
		if err == sql.ErrNoRows {
			return fmt.Errorf("finalize task %q as completed: %w", record.TaskID, ErrVerificationRequired)
		}
		if err != nil {
			return fmt.Errorf("check verification before finalize: %w", err)
		}
		if verificationDecision != "passed" {
			return fmt.Errorf("finalize task %q as completed: %w (decision %s)", record.TaskID, ErrVerificationNotPassed, verificationDecision)
		}
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks
		 SET status = ?, outcome = ?, stop_reason = ?, summary = ?, finished_at = ?
		 WHERE task_id = ?`,
		status, Redact(record.Outcome), Redact(record.StopReason), Redact(record.Summary), now, record.TaskID)
	if err != nil {
		return fmt.Errorf("finalize task: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("finalize task: task %q not found", record.TaskID)
	}
	hitCrashPoint("task_finalized_before")
	if err := appendEvent(ctx, tx, record.TaskID, "task_finalized", map[string]any{
		"status":         status,
		"outcome":        record.Outcome,
		"stop_reason":    record.StopReason,
		"classification": record.Classification,
		"summary":        record.Summary,
		"evidence":       record.Evidence,
		"turns":          record.Turns,
		"attempts":       record.Attempts,
		"observations":   record.Observations,
		"corrections":    record.Corrections,
		"repeated":       record.Repeated,
		"mixed_prose":    record.MixedProse,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task finalize: %w", err)
	}
	hitCrashPoint("task_finalized_after")
	return nil
}

// TaskStatus returns the current status projection of one task.
func (s *Store) TaskStatus(ctx context.Context, taskID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM tasks WHERE task_id = ?`, taskID).Scan(&status)
	if err != nil {
		return "", err
	}
	return status, nil
}

// TaskExists reports whether a task row exists.
func (s *Store) TaskExists(ctx context.Context, taskID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE task_id = ?`, taskID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
