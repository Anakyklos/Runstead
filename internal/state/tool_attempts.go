package state

import (
	"context"
	"encoding/json"
	"fmt"
)

// PrepareToolAttempt persists one concrete tool execution intent (TX 1) in
// status 'prepared' and returns its Runstead-owned execution_id. This
// transaction commits before the tool effect starts; a crash after this
// commit leaves durable evidence that the effect may have started.
func (s *Store) PrepareToolAttempt(ctx context.Context, record ToolAttemptPrepared) (string, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tool attempt preparation: %w", err)
	}
	defer tx.Rollback()
	executionID, err := nextIdentity(tx, "exec")
	if err != nil {
		return "", err
	}
	recoveryClass := record.RecoveryClass
	if recoveryClass < 1 || recoveryClass > 4 {
		recoveryClass = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tool_attempts (execution_id, task_id, action_id, tool, arguments_json, status, recovery_class, created_at, prepared_at)
		 VALUES (?, ?, ?, ?, ?, 'prepared', ?, ?, ?)`,
		executionID, record.TaskID, record.ActionID, Redact(record.Tool),
		string(RedactJSON(record.Arguments)), recoveryClass, now, now); err != nil {
		return "", fmt.Errorf("insert tool attempt: %w", err)
	}
	// The first concrete attempt intent moves the logical action from
	// planned to prepared (ADR action lifecycle), in the same transaction.
	if _, err := tx.ExecContext(ctx,
		`UPDATE actions SET status = 'prepared' WHERE action_id = ? AND task_id = ? AND status = 'planned'`,
		record.ActionID, record.TaskID); err != nil {
		return "", fmt.Errorf("prepare action: %w", err)
	}
	if err := appendEvent(ctx, tx, record.TaskID, "tool_attempt_prepared", map[string]any{
		"execution_id": executionID,
		"action_id":    record.ActionID,
		"tool":         record.Tool,
	}, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tool attempt preparation: %w", err)
	}
	hitCrashPoint("tool_tx1_after")
	return executionID, nil
}

// CompleteToolAttempt persists the outcome of one concrete tool execution
// (TX 2) after the effect returned. Successful observations store a citable
// tool_results row with the existing evidence id; failed observations store
// only the typed failure classification and never become citable evidence.
func (s *Store) CompleteToolAttempt(ctx context.Context, record ToolAttemptCompleted) error {
	// The effect already returned; this crash point simulates death before
	// TX 2 commits, leaving the attempt 'prepared'.
	hitCrashPoint("tool_tx2_before")
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tool attempt completion: %w", err)
	}
	defer tx.Rollback()
	status := record.Status
	if status == "" {
		status = "completed"
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tool_attempts
		 SET status = ?, classification = ?, evidence_id = ?, duration_ns = ?, completed_at = ?
		 WHERE execution_id = ? AND task_id = ?`,
		status, Redact(record.Classification), record.EvidenceID, record.DurationNanos, now,
		record.ExecutionID, record.TaskID)
	if err != nil {
		return fmt.Errorf("complete tool attempt: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("complete tool attempt: execution %q not found for task %q", record.ExecutionID, record.TaskID)
	}
	observation := record.Observation
	// The concrete attempt outcome moves the logical action to completed or
	// failed (ADR action lifecycle), in the same transaction.
	actionStatus := "completed"
	if status != "completed" {
		actionStatus = "failed"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE actions SET status = ? WHERE action_id = (SELECT action_id FROM tool_attempts WHERE execution_id = ?) AND status = 'prepared'`,
		actionStatus, record.ExecutionID); err != nil {
		return fmt.Errorf("complete action: %w", err)
	}
	if status == "completed" && observation.Success && record.EvidenceID != "" {
		dataJSON, marshalErr := json.Marshal(observation.Data)
		if marshalErr != nil {
			return fmt.Errorf("encode observation data: %w", marshalErr)
		}
		metadataJSON, marshalErr := json.Marshal(observation.Metadata)
		if marshalErr != nil {
			return fmt.Errorf("encode observation metadata: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tool_results (evidence_id, task_id, execution_id, success, untrusted, data_json, metadata_json, created_at)
			 VALUES (?, ?, ?, 1, ?, ?, ?, ?)`,
			record.EvidenceID, record.TaskID, record.ExecutionID, boolInt(observation.Metadata.Untrusted),
			string(RedactJSON(dataJSON)), string(RedactJSON(metadataJSON)), now); err != nil {
			return fmt.Errorf("insert tool result: %w", err)
		}
	}
	eventKind := "tool_attempt_completed"
	if status != "completed" {
		eventKind = "tool_attempt_failed"
	}
	if err := appendEvent(ctx, tx, record.TaskID, eventKind, map[string]any{
		"execution_id":   record.ExecutionID,
		"status":         status,
		"classification": record.Classification,
		"evidence_id":    record.EvidenceID,
		"duration_ns":    record.DurationNanos,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tool attempt completion: %w", err)
	}
	hitCrashPoint("tool_tx2_after")
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
