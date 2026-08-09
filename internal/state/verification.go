package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrVerificationRequired is returned by FinalizeTask when a task would be
// finalized as completed but no control-plane verification attempt exists.
// Completion is only ever decided by the runtime verifier (issue #11).
var ErrVerificationRequired = errors.New("task has no verification attempt; completion requires a passed verification")

// ErrVerificationNotPassed is returned by FinalizeTask when a task would be
// finalized as completed but the latest verification attempt did not pass.
var ErrVerificationNotPassed = errors.New("task completion refused: latest verification did not pass")

// Verification persistence (issue #11).
//
// Verification is part of the authoritative task history. One verification
// attempt (projection: decision + bounded report + per-check results) and its
// journal event commit in one SQLite transaction AFTER the external
// observations completed; no transaction is ever open during filesystem/git
// observation. The completion gate in FinalizeTask refuses status 'completed'
// unless the latest verification attempt of the task has decision 'passed',
// so no alternate code path can persist completed without a valid
// verification.

// VerificationAttemptRecord is one control-plane verification run to persist.
type VerificationAttemptRecord struct {
	TaskID string
	// Decision is the typed verifier decision: passed, failed, blocked or
	// uncertain.
	Decision string
	// Summary is the bounded one-line decision summary.
	Summary string
	// ReportJSON is the bounded structured verification report.
	ReportJSON []byte
	// Checks are the per-check results.
	Checks []VerificationCheckRecord
}

// VerificationCheckRecord is one per-check result to persist.
type VerificationCheckRecord struct {
	CheckID  string
	Type     string
	Status   string
	Expected string
	Observed string
	Evidence []string
	Reason   string
}

// VerificationAttemptRow is one persisted verification attempt.
type VerificationAttemptRow struct {
	AttemptID  string
	Sequence   int
	Decision   string
	Summary    string
	ReportJSON string
	CreatedAt  string
	Checks     []VerificationCheckRow
}

// VerificationCheckRow is one persisted per-check result.
type VerificationCheckRow struct {
	CheckID  string
	Type     string
	Status   string
	Expected string
	Observed string
	Evidence []string
	Reason   string
}

// SaveAcceptancePlan persists the operator acceptance plan of one task with
// its journal event atomically. A second save for the same task replaces the
// plan (the digest is re-validated by the resume pre-flight).
func (s *Store) SaveAcceptancePlan(ctx context.Context, taskID string, planJSON []byte, digest string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin acceptance plan save: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT version FROM acceptance_plans WHERE task_id = ?`, taskID).Scan(&version); err == sql.ErrNoRows {
		version = 0
	} else if err != nil {
		return fmt.Errorf("load acceptance plan version: %w", err)
	}
	encoded := string(RedactJSON(planJSON))
	if version == 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO acceptance_plans (task_id, version, spec_json, digest, created_at)
			 VALUES (?, 1, ?, ?, ?)`,
			taskID, encoded, Redact(digest), now); err != nil {
			return fmt.Errorf("insert acceptance plan: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE acceptance_plans SET spec_json = ?, digest = ?, created_at = ? WHERE task_id = ?`,
			encoded, Redact(digest), now, taskID); err != nil {
			return fmt.Errorf("update acceptance plan: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, taskID, "acceptance_plan_saved", map[string]any{
		"digest": Redact(digest),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit acceptance plan save: %w", err)
	}
	return nil
}

// AcceptancePlan returns the persisted operator acceptance plan spec JSON and
// its digest. The second result reports whether a plan exists.
func (s *Store) AcceptancePlan(ctx context.Context, taskID string) (specJSON []byte, digest string, ok bool, err error) {
	var spec, planDigest string
	err = s.db.QueryRowContext(ctx,
		`SELECT spec_json, digest FROM acceptance_plans WHERE task_id = ?`, taskID).Scan(&spec, &planDigest)
	if err == sql.ErrNoRows {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("load acceptance plan: %w", err)
	}
	return []byte(spec), planDigest, true, nil
}

// SaveWorkspaceBaseline persists the bounded real git status/diff observed at
// task start with its journal event atomically. The truncation flags record
// that the bounded baseline observations were truncated, so verification can
// surface the limitation explicitly (issue #11 review).
func (s *Store) SaveWorkspaceBaseline(ctx context.Context, taskID, gitStatus, gitDiff string, statusTruncated, diffTruncated bool) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workspace baseline save: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workspace_baselines (task_id, git_status_json, git_diff_json, git_status_truncated, git_diff_truncated, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (task_id) DO UPDATE SET git_status_json = excluded.git_status_json,
		   git_diff_json = excluded.git_diff_json,
		   git_status_truncated = excluded.git_status_truncated,
		   git_diff_truncated = excluded.git_diff_truncated,
		   created_at = excluded.created_at`,
		taskID, Redact(gitStatus), Redact(gitDiff), boolInt(statusTruncated), boolInt(diffTruncated), now); err != nil {
		return fmt.Errorf("insert workspace baseline: %w", err)
	}
	if err := appendEvent(ctx, tx, taskID, "workspace_baseline_saved", map[string]any{
		"git_status_bytes":     len(gitStatus),
		"git_diff_bytes":       len(gitDiff),
		"git_status_truncated": statusTruncated,
		"git_diff_truncated":   diffTruncated,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace baseline save: %w", err)
	}
	return nil
}

// WorkspaceBaseline returns the persisted git status/diff baseline of one task
// and its truncation flags. The third result reports whether a baseline
// exists.
func (s *Store) WorkspaceBaseline(ctx context.Context, taskID string) (gitStatus, gitDiff string, statusTruncated, diffTruncated bool, ok bool, err error) {
	var statusFlag, diffFlag int
	err = s.db.QueryRowContext(ctx,
		`SELECT git_status_json, git_diff_json, git_status_truncated, git_diff_truncated FROM workspace_baselines WHERE task_id = ?`, taskID).
		Scan(&gitStatus, &gitDiff, &statusFlag, &diffFlag)
	if err == sql.ErrNoRows {
		return "", "", false, false, false, nil
	}
	if err != nil {
		return "", "", false, false, false, fmt.Errorf("load workspace baseline: %w", err)
	}
	return gitStatus, gitDiff, statusFlag != 0, diffFlag != 0, true, nil
}

// SaveVerificationAttempt persists one verification attempt (projection:
// decision, report, per-check results) and its journal event atomically. The
// attempt id is allocated from the Runstead identity sequence; the sequence is
// task-scoped and monotonic.
func (s *Store) SaveVerificationAttempt(ctx context.Context, record VerificationAttemptRecord) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin verification attempt save: %w", err)
	}
	defer tx.Rollback()
	attemptID, err := nextIdentity(tx, "verif")
	if err != nil {
		return err
	}
	var sequence int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM verification_attempts WHERE task_id = ?`,
		record.TaskID).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate verification sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO verification_attempts (attempt_id, task_id, sequence, decision, report_json, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		attemptID, record.TaskID, sequence, Redact(record.Decision),
		string(RedactJSON(record.ReportJSON)), Redact(record.Summary), now); err != nil {
		return fmt.Errorf("insert verification attempt: %w", err)
	}
	for _, check := range record.Checks {
		evidence, marshalErr := json.Marshal(check.Evidence)
		if marshalErr != nil {
			return fmt.Errorf("encode verification check evidence: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO verification_checks (task_id, attempt_id, check_id, type, status, expected, observed, evidence_json, reason, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.TaskID, attemptID, Redact(check.CheckID), Redact(check.Type), Redact(check.Status),
			Redact(check.Expected), Redact(check.Observed), string(RedactJSON(evidence)), Redact(check.Reason), now); err != nil {
			return fmt.Errorf("insert verification check: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, record.TaskID, "verification_recorded", map[string]any{
		"attempt_id": attemptID,
		"decision":   record.Decision,
		"summary":    record.Summary,
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verification attempt: %w", err)
	}
	hitCrashPoint("verification_recorded_after")
	return nil
}

// VerificationAttempts returns the persisted verification attempts of one task
// in deterministic order (oldest first), each with its per-check results.
func (s *Store) VerificationAttempts(ctx context.Context, taskID string) ([]VerificationAttemptRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT attempt_id, sequence, decision, report_json, summary, created_at
		 FROM verification_attempts WHERE task_id = ? ORDER BY sequence`, taskID)
	if err != nil {
		return nil, fmt.Errorf("load verification attempts: %w", err)
	}
	defer rows.Close()
	var attempts []VerificationAttemptRow
	for rows.Next() {
		var attempt VerificationAttemptRow
		if err := rows.Scan(&attempt.AttemptID, &attempt.Sequence, &attempt.Decision, &attempt.ReportJSON, &attempt.Summary, &attempt.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan verification attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range attempts {
		checks, err := s.loadVerificationChecks(ctx, taskID, attempts[index].AttemptID)
		if err != nil {
			return nil, err
		}
		attempts[index].Checks = checks
	}
	return attempts, nil
}

func (s *Store) loadVerificationChecks(ctx context.Context, taskID, attemptID string) ([]VerificationCheckRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT check_id, type, status, expected, observed, evidence_json, reason
		 FROM verification_checks WHERE task_id = ? AND attempt_id = ? ORDER BY check_id`, taskID, attemptID)
	if err != nil {
		return nil, fmt.Errorf("load verification checks: %w", err)
	}
	defer rows.Close()
	var checks []VerificationCheckRow
	for rows.Next() {
		var check VerificationCheckRow
		var evidenceJSON string
		if err := rows.Scan(&check.CheckID, &check.Type, &check.Status, &check.Expected, &check.Observed, &evidenceJSON, &check.Reason); err != nil {
			return nil, fmt.Errorf("scan verification check: %w", err)
		}
		_ = json.Unmarshal([]byte(evidenceJSON), &check.Evidence)
		checks = append(checks, check)
	}
	return checks, rows.Err()
}

// MarkTaskVerificationPaused records a control-plane verification pause: the
// task stays durably resumable (status running) with the typed outcome and
// stop reason and a task_verification_paused event; no terminal finalize
// happens (issue #11). It is used when completion was refused by an uncertain
// effect, a pending approval at completion time, or a blocked acceptance
// check.
func (s *Store) MarkTaskVerificationPaused(ctx context.Context, taskID, stopReason string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin verification pause: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'running', outcome = 'verification_blocked', stop_reason = ?
		 WHERE task_id = ? AND status IN ('running', 'planned')`,
		Redact(stopReason), taskID)
	if err != nil {
		return fmt.Errorf("mark verification pause: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return fmt.Errorf("mark verification pause: task %q not running", taskID)
	}
	if err := appendEvent(ctx, tx, taskID, "task_verification_paused", map[string]any{
		"stop_reason": Redact(stopReason),
	}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verification pause: %w", err)
	}
	hitCrashPoint("verification_paused_after")
	return nil
}

// LatestVerificationDecision returns the decision of the latest verification
// attempt of one task. The second result reports whether any attempt exists.
func (s *Store) LatestVerificationDecision(ctx context.Context, taskID string) (string, bool, error) {
	var decision string
	err := s.db.QueryRowContext(ctx,
		`SELECT decision FROM verification_attempts WHERE task_id = ? ORDER BY sequence DESC LIMIT 1`,
		taskID).Scan(&decision)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load latest verification decision: %w", err)
	}
	return decision, true, nil
}
