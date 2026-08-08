package state

import (
	"context"
	"database/sql"
	"fmt"
)

// Write-policy and approval persistence (issue #10).
//
// Policy decisions are control-plane state recorded independently from the
// proposed write and from any model output: the loop persists every typed
// decision (allowed, denied, approval_required, approved, rejected) with its
// reason before any execution decision is acted on. Approvals can only be
// created by the operator control plane (`runstead decide`), never by model
// prose or repository content.

// WritePolicyDecision is one durable, typed policy decision for a write
// action.
type WritePolicyDecision struct {
	TaskID   string
	ActionID string
	Tool     string
	// Decision is the typed policy outcome: allowed, denied, approval_required,
	// approved or rejected.
	Decision string
	// Reason is the typed reason for the decision.
	Reason string
}

// RecordWritePolicyDecision persists one policy decision and its journal
// event atomically.
func (s *Store) RecordWritePolicyDecision(ctx context.Context, record WritePolicyDecision) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write policy decision: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO write_policy_decisions (task_id, action_id, tool, decision, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		record.TaskID, Redact(record.ActionID), Redact(record.Tool),
		Redact(record.Decision), Redact(record.Reason), now); err != nil {
		return fmt.Errorf("insert write policy decision: %w", err)
	}
	if err := appendEvent(ctx, tx, record.TaskID, "write_policy_decision", map[string]any{
		"action_id": record.ActionID,
		"tool":      record.Tool,
		"decision":  record.Decision,
		"reason":    record.Reason,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write policy decision: %w", err)
	}
	hitCrashPoint("write_policy_decision_after")
	return nil
}

// Approval is one operator control-plane approval record. Approvals are keyed
// by (task_id, fingerprint), the repeat/loop identity of the write proposal:
// an approval survives re-proposals of the same write (each re-proposal is a
// new action id with the same fingerprint), so a resumed or corrected run can
// execute an approved write without a second operator round trip.
type Approval struct {
	ApprovalID string
	TaskID     string
	ActionID   string
	// Fingerprint is the repeat/loop identity of the write proposal the
	// approval governs.
	Fingerprint string
	// Decision is "approved" or "rejected".
	Decision string
	Reason   string
	Actor    string
}

// RecordApproval persists one operator approval (approved or rejected) for a
// write proposal. The action id is resolved to its fingerprint from the
// actions table (the action must exist for the task); re-deciding the same
// fingerprint replaces the previous decision and keeps one durable row per
// (task_id, fingerprint). The approval id is a deterministic function of task
// and fingerprint: it must never consume the shared Runstead identity sequence
// (action/execution ids depend on that sequence being predictable across a
// run).
func (s *Store) RecordApproval(ctx context.Context, record Approval) (string, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin approval record: %w", err)
	}
	defer tx.Rollback()
	var fingerprint string
	if err := tx.QueryRowContext(ctx,
		`SELECT fingerprint FROM actions WHERE task_id = ? AND action_id = ?`,
		record.TaskID, record.ActionID).Scan(&fingerprint); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("approve action %q for task %q: action not found", record.ActionID, record.TaskID)
		}
		return "", fmt.Errorf("resolve approval fingerprint: %w", err)
	}
	if fingerprint == "" {
		return "", fmt.Errorf("approve action %q for task %q: action has no fingerprint", record.ActionID, record.TaskID)
	}
	approvalID := "approval-" + record.TaskID + "-" + fingerprint
	actor := record.Actor
	if actor == "" {
		actor = "operator"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals (approval_id, task_id, action_id, fingerprint, decision, reason, actor, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (task_id, fingerprint)
		 DO UPDATE SET action_id = excluded.action_id, decision = excluded.decision,
		               reason = excluded.reason, actor = excluded.actor, created_at = excluded.created_at`,
		approvalID, record.TaskID, Redact(record.ActionID), Redact(fingerprint), Redact(record.Decision),
		Redact(record.Reason), Redact(actor), now); err != nil {
		return "", fmt.Errorf("insert approval: %w", err)
	}
	if err := appendEvent(ctx, tx, record.TaskID, "approval_recorded", map[string]any{
		"action_id":   record.ActionID,
		"fingerprint": fingerprint,
		"decision":    record.Decision,
		"reason":      record.Reason,
		"actor":       actor,
	}, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit approval record: %w", err)
	}
	hitCrashPoint("approval_recorded_after")
	return approvalID, nil
}

// Approval returns the current operator decision for one write proposal,
// identified by its fingerprint.
func (s *Store) Approval(ctx context.Context, taskID, fingerprint string) (Approval, bool, error) {
	var approval Approval
	err := s.db.QueryRowContext(ctx,
		`SELECT approval_id, task_id, action_id, fingerprint, decision, reason, actor
		 FROM approvals WHERE task_id = ? AND fingerprint = ?`, taskID, fingerprint).Scan(
		&approval.ApprovalID, &approval.TaskID, &approval.ActionID, &approval.Fingerprint,
		&approval.Decision, &approval.Reason, &approval.Actor)
	if err == sql.ErrNoRows {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("load approval: %w", err)
	}
	return approval, true, nil
}
