package state

import (
	"context"
	"fmt"
)

// RecordAction persists one accepted envelope as a logical action in status
// 'planned' and returns its action_id. The action row exists before any
// repeat guard or policy decision, so a proposal the guard rejects is still
// represented as a distinct logical action.
func (s *Store) RecordAction(ctx context.Context, record ActionRecord) (string, error) {
	createdAt := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin action record: %w", err)
	}
	defer tx.Rollback()
	actionID, err := nextIdentity(tx, "action")
	if err != nil {
		return "", err
	}
	var sequence int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(action_sequence), 0) + 1 FROM actions WHERE task_id = ?`,
		record.TaskID).Scan(&sequence); err != nil {
		return "", fmt.Errorf("allocate action sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO actions (action_id, task_id, action_sequence, tool, arguments_json, fingerprint, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'planned', ?)`,
		actionID, record.TaskID, sequence, Redact(record.Tool),
		string(RedactJSON(record.Arguments)), Redact(record.Fingerprint), createdAt); err != nil {
		return "", fmt.Errorf("insert action: %w", err)
	}
	if err := appendEvent(ctx, tx, record.TaskID, "action_planned", map[string]any{
		"action_id": actionID,
		"tool":      record.Tool,
	}, createdAt); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit action record: %w", err)
	}
	hitCrashPoint("action_planned_after")
	return actionID, nil
}

// RejectAction marks a planned action as rejected (repeat guard or policy)
// without creating any tool attempt.
func (s *Store) RejectAction(ctx context.Context, taskID, actionID, reason string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin action rejection: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE actions SET status = 'rejected' WHERE action_id = ? AND task_id = ? AND status = 'planned'`,
		actionID, taskID)
	if err != nil {
		return fmt.Errorf("reject action: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("reject action: action %q not found or not planned", actionID)
	}
	if err := appendEvent(ctx, tx, taskID, "action_rejected", map[string]any{
		"action_id": actionID,
		"reason":    Redact(reason),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit action rejection: %w", err)
	}
	hitCrashPoint("action_rejected_after")
	return nil
}
