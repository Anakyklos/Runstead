package state

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Write-attempt reconciliation (issue #10, extending #9).
//
// An interrupted write attempt (ADR recovery class 2) left 'prepared' after a
// crash is reconciled from observable filesystem state, never re-executed
// blindly. When the current file hash matches the recorded expected after
// state, the effect is verified completed: the attempt is marked 'reconciled'
// with the typed reason "write_effect_completed" and the observed evidence is
// persisted as a citable tool_results row in the same transaction, so a
// resumed run can cite it. When the current state matches the recorded
// precondition, the effect never started and the attempt reconciles without
// evidence. When neither matches, the outcome is unreconcilable and the
// recovery pipeline escalates to human_review_required.

// ReconcileWriteAttempt is one verified write reconciliation: the effect
// completed and the observed evidence was captured from the filesystem.
type ReconcileWriteAttempt struct {
	TaskID      string
	ExecutionID string
	Status      string
	Reason      string
	// EvidenceID is the task-scoped citable evidence id allocated by the
	// recovery pipeline (obs-NNNNNN, continuing the persisted sequence).
	EvidenceID string
	// Evidence is the observed write evidence (path, hashes, byte count,
	// change kind) captured from the current filesystem state.
	Evidence tools.WriteEvidence
}

// ReconcileWriteAttempt persists one verified write reconciliation and its
// journal event atomically: the attempt moves to 'reconciled' with the typed
// reason, the action completes in the same transaction (the effect was
// verified), and the observed evidence becomes citable via tool_results.
func (s *Store) ReconcileWriteAttempt(ctx context.Context, record ReconcileWriteAttempt) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write reconciliation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tool_attempts
		 SET status = ?, recovery_reason = ?, evidence_id = ?, completed_at = ?
		 WHERE execution_id = ? AND task_id = ?
		   AND status IN ('planned', 'prepared', 'running', 'observed', 'verified', 'uncertain', 'verification_failed')`,
		record.Status, Redact(record.Reason), record.EvidenceID, now, record.ExecutionID, record.TaskID)
	if err != nil {
		return fmt.Errorf("reconcile write attempt: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("reconcile write attempt %q for task %q: %w", record.ExecutionID, record.TaskID, ErrNotReconcilable)
	}
	if record.Status == "reconciled" && record.EvidenceID != "" {
		evidence := record.Evidence
		evidence.EvidenceID = record.EvidenceID
		evidence.ExecutionID = record.ExecutionID
		dataJSON, marshalErr := json.Marshal(evidence)
		if marshalErr != nil {
			return fmt.Errorf("encode write evidence: %w", marshalErr)
		}
		metadataJSON, marshalErr := json.Marshal(tools.Metadata{
			Source:    "reconciled-write",
			Untrusted: true,
			Path:      evidence.Path,
			ExitCode:  -1,
		})
		if marshalErr != nil {
			return fmt.Errorf("encode write evidence metadata: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tool_results (evidence_id, task_id, execution_id, success, untrusted, data_json, metadata_json, created_at)
			 VALUES (?, ?, ?, 1, 1, ?, ?, ?)`,
			record.EvidenceID, record.TaskID, record.ExecutionID,
			string(RedactJSON(dataJSON)), string(RedactJSON(metadataJSON)), now); err != nil {
			return fmt.Errorf("insert reconciled write evidence: %w", err)
		}
	}
	if record.Status == "reconciled" && record.Reason == "write_effect_completed" {
		// The verified write completed the effect of its logical action: the
		// action completes in the same transaction so a resumed run never
		// re-proposes an executed write as if it were still pending.
		if _, err := tx.ExecContext(ctx,
			`UPDATE actions SET status = 'completed'
			 WHERE action_id = (SELECT action_id FROM tool_attempts WHERE execution_id = ?) AND status = 'prepared'`,
			record.ExecutionID); err != nil {
			return fmt.Errorf("complete write action: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, record.TaskID, "tool_attempt_reconciled", map[string]any{
		"execution_id": record.ExecutionID,
		"status":       record.Status,
		"reason":       record.Reason,
		"evidence_id":  record.EvidenceID,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write reconciliation: %w", err)
	}
	hitCrashPoint("recovery_write_reconciled_after")
	return nil
}
