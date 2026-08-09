package state

import (
	"context"
	"errors"
	"fmt"
)

// Approval-flow persistence (issue #10 review).
//
// A write proposal that requires operator approval is a control-plane
// dependency, not a protocol error: the run pauses with the typed
// OutcomeApprovalRequired, the task stays durably resumable (status running),
// and the pending action is derived from write_policy_decisions joined with
// actions and approvals. `runstead decide` records the operator decision;
// `runstead resume` then continues the same task under the persisted policy.

// effectiveFingerprintExpr is the approval identity of one action row:
// run_recipe actions are bound to their digest-bound recipe fingerprint
// (issue #26 review), every other action uses the plain repeat/loop
// fingerprint. Approval rows are keyed by this identity, so an approval for
// one recipe definition never matches a different definition of the same id.
const effectiveFingerprintExpr = `CASE WHEN a.tool = 'run_recipe' AND a.recipe_fingerprint != '' THEN a.recipe_fingerprint ELSE a.fingerprint END`

// ErrPendingApprovals is returned by FinalizeTask when a task would be
// finalized as completed while one or more mandatory writes are still awaiting
// an operator approval. The state machine refuses the transition: the task
// must be decided and resumed, never completed around a pending write.
var ErrPendingApprovals = errors.New("task has pending write approvals")

// PendingApproval is one write action awaiting an operator decision.
type PendingApproval struct {
	ActionID    string
	Tool        string
	Fingerprint string
}

// MarkTaskApprovalRequired records a control-plane pause: the task stays in
// status 'running' (durably resumable), the outcome/stop_reason projection
// records the pause, and the task_approval_required journal event is appended.
// No terminal finalize happens: the operator decides the pending action with
// `runstead decide` and a normal `runstead resume` continues the task.
func (s *Store) MarkTaskApprovalRequired(ctx context.Context, taskID, actionID, reason string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin task approval pause: %w", err)
	}
	defer tx.Rollback()
	stopReason := "waiting for operator approval"
	if actionID != "" {
		stopReason = "waiting for operator approval: action " + actionID
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET outcome = 'approval_required', stop_reason = ? WHERE task_id = ? AND status = 'running'`,
		Redact(stopReason), taskID)
	if err != nil {
		return fmt.Errorf("mark task approval required: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		// A task that is not running cannot pause for approval; this is a
		// state-machine violation and fails closed.
		return fmt.Errorf("mark task approval required: task %q is not running", taskID)
	}
	if err := appendEvent(ctx, tx, taskID, "task_approval_required", map[string]any{
		"action_id": actionID,
		"reason":    Redact(reason),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task approval pause: %w", err)
	}
	hitCrashPoint("task_approval_required_after")
	return nil
}

// PendingApprovals returns the write actions of one task that are still
// awaiting an operator decision: they have a persisted approval_required
// policy decision and their effective fingerprint has no approvals row yet
// (neither approved nor rejected). The effective fingerprint is the
// digest-bound recipe fingerprint for run_recipe actions and the plain
// fingerprint otherwise, so a pending recipe approval is bound to the recipe
// definition that was actually proposed. Deterministic order follows the
// decision insert order (oldest first).
func (s *Store) PendingApprovals(ctx context.Context, taskID string) ([]PendingApproval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.action_id, a.tool, a.fingerprint
		 FROM write_policy_decisions d
		 JOIN actions a ON a.task_id = d.task_id AND a.action_id = d.action_id
		 WHERE d.task_id = ? AND d.decision = 'approval_required'
		   AND NOT EXISTS (
		       SELECT 1 FROM approvals ap
		       WHERE ap.task_id = d.task_id AND ap.fingerprint = `+effectiveFingerprintExpr+`
		   )
		 ORDER BY d.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load pending approvals: %w", err)
	}
	defer rows.Close()
	var pending []PendingApproval
	for rows.Next() {
		var item PendingApproval
		if err := rows.Scan(&item.ActionID, &item.Tool, &item.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan pending approval: %w", err)
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

// HasPendingApprovals reports whether the task has at least one write action
// still awaiting an operator decision.
func (s *Store) HasPendingApprovals(ctx context.Context, taskID string) (bool, error) {
	pending, err := s.PendingApprovals(ctx, taskID)
	if err != nil {
		return false, err
	}
	return len(pending) > 0, nil
}
