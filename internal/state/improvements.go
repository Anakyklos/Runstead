package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/improvement"
)

// ErrImprovementCorrupt is the fail-closed state for missing, malformed or
// inconsistent proposal rows.
var ErrImprovementCorrupt = errors.New("corrupt improvement proposal state")

// ProposeImprovement atomically validates the proposal provenance against
// durable state and persists the proposal as pending. Evidence refs must
// exist in tool_results and work-unit refs must belong to a source task;
// any missing or incompatible reference fails closed with no row written.
// Text fields are redacted before persistence.
func (s *Store) ProposeImprovement(ctx context.Context, proposal improvement.Proposal, refs []improvement.EvidenceRef) (improvement.Proposal, error) {
	if err := improvement.ValidateProposal(proposal); err != nil {
		return improvement.Proposal{}, err
	}
	if err := improvement.ValidateEvidenceRefs(refs); err != nil {
		return improvement.Proposal{}, err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return improvement.Proposal{}, fmt.Errorf("begin proposal: %w", err)
	}
	defer tx.Rollback()
	proposalID, err := nextIdentity(tx, "prop")
	if err != nil {
		return improvement.Proposal{}, err
	}
	for _, ref := range refs {
		if err := requireEvidenceRow(ctx, tx, ref.TaskID, ref.EvidenceID); err != nil {
			return improvement.Proposal{}, fmt.Errorf("%w: %v", improvement.ErrInvalidEvidence, err)
		}
	}
	for _, workUnitID := range proposal.SourceWorkUnitIDs {
		if err := requireWorkUnitOfTask(ctx, tx, workUnitID, proposal.SourceTaskIDs); err != nil {
			return improvement.Proposal{}, err
		}
	}
	proposal.ProposalID = proposalID
	proposal.Status = improvement.StatusPending
	proposal.Title = Redact(strings.TrimSpace(proposal.Title))
	proposal.Rationale = Redact(strings.TrimSpace(proposal.Rationale))
	proposal.ExpectedBenefit = Redact(strings.TrimSpace(proposal.ExpectedBenefit))
	proposal.ProposedChangeJSON = RedactJSON(proposal.ProposedChangeJSON)
	proposal.CreatedAt = now
	proposal.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO improvement_proposals (
			proposal_id, kind, scope_id, title, target_id, target_base_version,
			source_task_ids, source_work_unit_ids, proposed_change_json,
			rationale, expected_benefit, invariants_touched, validation_plan,
			lifecycle_status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.ProposalID, string(proposal.Kind), proposal.ScopeID, proposal.Title, proposal.TargetID, proposal.TargetBaseVersion,
		jsonArray(proposal.SourceTaskIDs), jsonArray(proposal.SourceWorkUnitIDs),
		string(proposal.ProposedChangeJSON),
		proposal.Rationale, proposal.ExpectedBenefit, jsonArray(proposal.InvariantsTouched), jsonArray(proposal.ValidationPlan),
		string(improvement.StatusPending), proposal.CreatedAt, proposal.UpdatedAt,
	); err != nil {
		return improvement.Proposal{}, fmt.Errorf("insert proposal: %w", err)
	}
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO improvement_proposal_evidence (proposal_id, task_id, evidence_id) VALUES (?, ?, ?)`,
			proposal.ProposalID, ref.TaskID, ref.EvidenceID); err != nil {
			return improvement.Proposal{}, fmt.Errorf("insert proposal evidence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return improvement.Proposal{}, fmt.Errorf("commit proposal: %w", err)
	}
	return proposal, nil
}

// LoadImprovement returns the full non-secret proposal record.
func (s *Store) LoadImprovement(ctx context.Context, proposalID string) (improvement.Proposal, error) {
	return s.loadImprovementRow(ctx, proposalID)
}

func (s *Store) loadImprovementRow(ctx context.Context, proposalID string) (improvement.Proposal, error) {
	var proposal improvement.Proposal
	var sourceTasks, sourceUnits, invariants, plan string
	var proposalKind, status string
	err := s.db.QueryRowContext(ctx, `
		SELECT proposal_id, kind, scope_id, title, target_id, target_base_version,
		       source_task_ids, source_work_unit_ids, proposed_change_json,
		       rationale, expected_benefit, invariants_touched, validation_plan,
		       lifecycle_status, review_decision, review_reason, reviewed_by,
		       decided_at, version_id, artifact_path, rolled_back_to, rolled_back_at,
		       created_at, updated_at
		FROM improvement_proposals WHERE proposal_id = ?`, proposalID).
		Scan(&proposal.ProposalID, &proposalKind, &proposal.ScopeID, &proposal.Title, &proposal.TargetID, &proposal.TargetBaseVersion,
			&sourceTasks, &sourceUnits, &proposal.ProposedChangeJSON,
			&proposal.Rationale, &proposal.ExpectedBenefit, &invariants, &plan,
			&status, &proposal.ReviewDecision, &proposal.ReviewReason, &proposal.ReviewedBy,
			&proposal.DecidedAt, &proposal.VersionID, &proposal.ArtifactPath, &proposal.RolledBackTo, &proposal.RolledBackAt,
			&proposal.CreatedAt, &proposal.UpdatedAt)
	if err == sql.ErrNoRows {
		return improvement.Proposal{}, improvement.ErrUnknownProposal
	}
	if err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: %v", ErrImprovementCorrupt, err)
	}
	proposal.Kind = improvement.Kind(proposalKind)
	proposal.Status = improvement.Status(status)
	if err := json.Unmarshal([]byte(sourceTasks), &proposal.SourceTaskIDs); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: source_task_ids: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(sourceUnits), &proposal.SourceWorkUnitIDs); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: source_work_unit_ids: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(invariants), &proposal.InvariantsTouched); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: invariants_touched: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(plan), &proposal.ValidationPlan); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: validation_plan: %v", ErrImprovementCorrupt, err)
	}
	return proposal, nil
}

// ListImprovements lists proposal summaries. scope and status are optional
// filters; an empty value matches everything.
func (s *Store) ListImprovements(ctx context.Context, scope, status string) ([]improvement.Summary, error) {
	query := `SELECT proposal_id, kind, scope_id, target_id, title, lifecycle_status, review_decision, version_id, created_at
	          FROM improvement_proposals`
	var args []any
	switch {
	case strings.TrimSpace(scope) != "" && strings.TrimSpace(status) != "":
		query += ` WHERE scope_id = ? AND lifecycle_status = ?`
		args = append(args, strings.TrimSpace(scope), strings.TrimSpace(status))
	case strings.TrimSpace(scope) != "":
		query += ` WHERE scope_id = ?`
		args = append(args, strings.TrimSpace(scope))
	case strings.TrimSpace(status) != "":
		query += ` WHERE lifecycle_status = ?`
		args = append(args, strings.TrimSpace(status))
	}
	query += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()
	var summaries []improvement.Summary
	for rows.Next() {
		var summary improvement.Summary
		var proposalKind, lifecycle string
		if err := rows.Scan(&summary.ProposalID, &proposalKind, &summary.ScopeID, &summary.TargetID, &summary.Title,
			&lifecycle, &summary.ReviewDecision, &summary.VersionID, &summary.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		summary.Kind = improvement.Kind(proposalKind)
		summary.Status = improvement.Status(lifecycle)
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

// ReviewImprovement applies the explicit operator decision. approve is only
// valid from pending; reject is valid from pending or approved. The
// transition is atomic and fail-closed.
func (s *Store) ReviewImprovement(ctx context.Context, proposalID string, decision improvement.Decision, reason, reviewer, at string) error {
	decision = improvement.Decision(strings.TrimSpace(string(decision)))
	reviewer = strings.TrimSpace(reviewer)
	if decision != improvement.DecisionApprove && decision != improvement.DecisionReject {
		return fmt.Errorf("%w: unsupported decision %q", improvement.ErrInvalidTransition, decision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review: %w", err)
	}
	defer tx.Rollback()
	proposal, err := loadImprovementRowTx(ctx, tx, proposalID)
	if err != nil {
		return err
	}
	target, err := proposal.ApplyReviewTransition(decision)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE improvement_proposals SET lifecycle_status = ?, review_decision = ?,
			review_reason = ?, reviewed_by = ?, decided_at = ?, updated_at = ?
		WHERE proposal_id = ?`,
		string(target), string(decision), Redact(strings.TrimSpace(reason)), reviewer, at, at, proposalID); err != nil {
		return fmt.Errorf("update review: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review: %w", err)
	}
	return nil
}

// ApplyImprovement atomically versions an approved proposal: it computes the
// next revision for the target, validates the declared base revision when
// present, stores the canonical artifact and returns the created version.
// The artifact FILE is a projection: the caller writes it after this
// transaction, and the stored bytes always remain recoverable.
func (s *Store) ApplyImprovement(ctx context.Context, proposalID, artifactPath, at string) (improvement.Version, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return improvement.Version{}, fmt.Errorf("begin apply: %w", err)
	}
	defer tx.Rollback()
	proposal, err := loadImprovementRowTx(ctx, tx, proposalID)
	if err != nil {
		return improvement.Version{}, err
	}
	if _, err := proposal.Transition("apply"); err != nil {
		return improvement.Version{}, err
	}
	if proposal.TargetBaseVersion != "" {
		if err := requireVersionRow(ctx, tx, proposal.TargetBaseVersion, proposal.TargetID); err != nil {
			return improvement.Version{}, err
		}
	}
	var revision int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM improvement_versions WHERE target_id = ?`, proposal.TargetID).
		Scan(&revision); err != nil {
		return improvement.Version{}, fmt.Errorf("compute revision: %w", err)
	}
	revision++
	versionID, err := nextIdentity(tx, "ver")
	if err != nil {
		return improvement.Version{}, err
	}
	artifact := proposal.ProposedChangeJSON
	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	version := improvement.Version{
		VersionID: versionID, ProposalID: proposalID, TargetID: proposal.TargetID,
		Revision: revision, BaseVersionID: proposal.TargetBaseVersion,
		ArtifactDigest: digest, ArtifactJSON: append([]byte(nil), artifact...), CreatedAt: at,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO improvement_versions (version_id, proposal_id, target_id, revision, base_version_id, artifact_digest, artifact_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		version.VersionID, version.ProposalID, version.TargetID, version.Revision, version.BaseVersionID,
		version.ArtifactDigest, string(version.ArtifactJSON), version.CreatedAt); err != nil {
		return improvement.Version{}, fmt.Errorf("insert version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE improvement_proposals SET lifecycle_status = ?, version_id = ?, artifact_path = ?, updated_at = ?
		WHERE proposal_id = ?`,
		string(improvement.StatusApplied), versionID, strings.TrimSpace(artifactPath), at, proposalID); err != nil {
		return improvement.Version{}, fmt.Errorf("update applied proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return improvement.Version{}, fmt.Errorf("commit apply: %w", err)
	}
	return version, nil
}

// ValidateImprovement attaches one later objective validation record. Every
// evidence reference must exist in tool_results; a narrative without
// durable evidence can never create a validation or set validated.
func (s *Store) ValidateImprovement(ctx context.Context, proposalID string, outcome improvement.Outcome, refs []improvement.EvidenceRef, notes, at string) (improvement.ValidationRecord, error) {
	outcome = improvement.Outcome(strings.TrimSpace(string(outcome)))
	switch outcome {
	case improvement.OutcomePositive, improvement.OutcomeNegative, improvement.OutcomeUncertain:
	default:
		return improvement.ValidationRecord{}, fmt.Errorf("%w: unsupported outcome %q", improvement.ErrInvalidProposal, outcome)
	}
	if err := improvement.ValidateEvidenceRefs(refs); err != nil {
		return improvement.ValidationRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return improvement.ValidationRecord{}, fmt.Errorf("begin validation: %w", err)
	}
	defer tx.Rollback()
	proposal, err := loadImprovementRowTx(ctx, tx, proposalID)
	if err != nil {
		return improvement.ValidationRecord{}, err
	}
	if _, err := proposal.Transition("validate"); err != nil {
		return improvement.ValidationRecord{}, err
	}
	if proposal.VersionID == "" {
		return improvement.ValidationRecord{}, fmt.Errorf("%w: applied proposal missing version", ErrImprovementCorrupt)
	}
	for _, ref := range refs {
		if err := requireEvidenceRow(ctx, tx, ref.TaskID, ref.EvidenceID); err != nil {
			return improvement.ValidationRecord{}, fmt.Errorf("%w: %v", improvement.ErrInvalidEvidence, err)
		}
	}
	validationID, err := nextIdentity(tx, "val")
	if err != nil {
		return improvement.ValidationRecord{}, err
	}
	evidenceJSON, err := json.Marshal(refs)
	if err != nil {
		return improvement.ValidationRecord{}, fmt.Errorf("%w: encode validation evidence", ErrImprovementCorrupt)
	}
	record := improvement.ValidationRecord{
		ValidationID: validationID, ProposalID: proposalID, VersionID: proposal.VersionID,
		Outcome: outcome, Evidence: refs, Notes: Redact(strings.TrimSpace(notes)), ObservedAt: at, CreatedAt: at,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO improvement_validations (validation_id, proposal_id, version_id, outcome, evidence, notes, observed_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ValidationID, record.ProposalID, record.VersionID, string(record.Outcome),
		string(evidenceJSON), record.Notes, record.ObservedAt, record.CreatedAt); err != nil {
		return improvement.ValidationRecord{}, fmt.Errorf("insert validation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE improvement_proposals SET lifecycle_status = ?, updated_at = ? WHERE proposal_id = ?`,
		string(improvement.StatusValidated), at, proposalID); err != nil {
		return improvement.ValidationRecord{}, fmt.Errorf("update validated proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return improvement.ValidationRecord{}, fmt.Errorf("commit validation: %w", err)
	}
	return record, nil
}

// RollbackImprovement restores the previous revision deterministically from
// the stored base artifact bytes and marks the proposal rolled_back. The
// first revision of a target has no base and fails closed with
// ErrNoBaseRevision. The returned artifact bytes are the previous revision;
// the caller rewrites the materialized file projection.
func (s *Store) RollbackImprovement(ctx context.Context, proposalID, reason, at string) ([]byte, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin rollback: %w", err)
	}
	defer tx.Rollback()
	proposal, err := loadImprovementRowTx(ctx, tx, proposalID)
	if err != nil {
		return nil, err
	}
	if _, err := proposal.Transition("rollback"); err != nil {
		return nil, err
	}
	var artifact string
	if err := tx.QueryRowContext(ctx,
		`SELECT artifact_json FROM improvement_versions WHERE version_id = ?`, proposal.TargetBaseVersion).
		Scan(&artifact); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: base revision %q missing", ErrImprovementCorrupt, proposal.TargetBaseVersion)
		}
		return nil, fmt.Errorf("load base revision: %w", err)
	}
	trail := proposal.ReviewReason
	if strings.TrimSpace(reason) != "" {
		redacted := Redact(strings.TrimSpace(reason))
		if strings.TrimSpace(trail) == "" {
			trail = "rolled back: " + redacted
		} else {
			trail = trail + " | rolled back: " + redacted
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE improvement_proposals SET lifecycle_status = ?, rolled_back_to = ?, rolled_back_at = ?,
			review_reason = ?, updated_at = ?
		WHERE proposal_id = ?`,
		string(improvement.StatusRolledBack), proposal.TargetBaseVersion, at, trail, at, proposalID); err != nil {
		return nil, fmt.Errorf("update rolled back proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit rollback: %w", err)
	}
	return []byte(artifact), nil
}

// LoadImprovementVersion returns one stored version.
func (s *Store) LoadImprovementVersion(ctx context.Context, versionID string) (improvement.Version, error) {
	var version improvement.Version
	err := s.db.QueryRowContext(ctx, `
		SELECT version_id, proposal_id, target_id, revision, base_version_id, artifact_digest, artifact_json, created_at
		FROM improvement_versions WHERE version_id = ?`, versionID).
		Scan(&version.VersionID, &version.ProposalID, &version.TargetID, &version.Revision,
			&version.BaseVersionID, &version.ArtifactDigest, &version.ArtifactJSON, &version.CreatedAt)
	if err == sql.ErrNoRows {
		return improvement.Version{}, improvement.ErrUnknownProposal
	}
	if err != nil {
		return improvement.Version{}, fmt.Errorf("%w: %v", ErrImprovementCorrupt, err)
	}
	return version, nil
}

// LoadImprovementValidations returns every validation record of a proposal.
func (s *Store) LoadImprovementValidations(ctx context.Context, proposalID string) ([]improvement.ValidationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT validation_id, proposal_id, version_id, outcome, evidence, notes, observed_at, created_at
		FROM improvement_validations WHERE proposal_id = ? ORDER BY created_at`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("list validations: %w", err)
	}
	defer rows.Close()
	var records []improvement.ValidationRecord
	for rows.Next() {
		var record improvement.ValidationRecord
		var outcome, evidenceJSON string
		if err := rows.Scan(&record.ValidationID, &record.ProposalID, &record.VersionID, &outcome,
			&evidenceJSON, &record.Notes, &record.ObservedAt, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan validation: %w", err)
		}
		record.Outcome = improvement.Outcome(outcome)
		if err := json.Unmarshal([]byte(evidenceJSON), &record.Evidence); err != nil {
			return nil, fmt.Errorf("%w: validation evidence: %v", ErrImprovementCorrupt, err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt < records[j].CreatedAt })
	return records, rows.Err()
}

// LoadImprovementEvidence returns the durable evidence refs of a proposal.
func (s *Store) LoadImprovementEvidence(ctx context.Context, proposalID string) ([]improvement.EvidenceRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, evidence_id FROM improvement_proposal_evidence WHERE proposal_id = ? ORDER BY task_id, evidence_id`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("list proposal evidence: %w", err)
	}
	defer rows.Close()
	var refs []improvement.EvidenceRef
	for rows.Next() {
		var ref improvement.EvidenceRef
		if err := rows.Scan(&ref.TaskID, &ref.EvidenceID); err != nil {
			return nil, fmt.Errorf("scan proposal evidence: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// -- tx helpers ------------------------------------------------------------

func loadImprovementRowTx(ctx context.Context, tx *sql.Tx, proposalID string) (improvement.Proposal, error) {
	var proposal improvement.Proposal
	var sourceTasks, sourceUnits, invariants, plan string
	var proposalKind, status string
	err := tx.QueryRowContext(ctx, `
		SELECT proposal_id, kind, scope_id, title, target_id, target_base_version,
		       source_task_ids, source_work_unit_ids, proposed_change_json,
		       rationale, expected_benefit, invariants_touched, validation_plan,
		       lifecycle_status, review_decision, review_reason, reviewed_by,
		       decided_at, version_id, artifact_path, rolled_back_to, rolled_back_at,
		       created_at, updated_at
		FROM improvement_proposals WHERE proposal_id = ?`, proposalID).
		Scan(&proposal.ProposalID, &proposalKind, &proposal.ScopeID, &proposal.Title, &proposal.TargetID, &proposal.TargetBaseVersion,
			&sourceTasks, &sourceUnits, &proposal.ProposedChangeJSON,
			&proposal.Rationale, &proposal.ExpectedBenefit, &invariants, &plan,
			&status, &proposal.ReviewDecision, &proposal.ReviewReason, &proposal.ReviewedBy,
			&proposal.DecidedAt, &proposal.VersionID, &proposal.ArtifactPath, &proposal.RolledBackTo, &proposal.RolledBackAt,
			&proposal.CreatedAt, &proposal.UpdatedAt)
	if err == sql.ErrNoRows {
		return improvement.Proposal{}, improvement.ErrUnknownProposal
	}
	if err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: %v", ErrImprovementCorrupt, err)
	}
	proposal.Kind = improvement.Kind(proposalKind)
	proposal.Status = improvement.Status(status)
	if err := json.Unmarshal([]byte(sourceTasks), &proposal.SourceTaskIDs); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: source_task_ids: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(sourceUnits), &proposal.SourceWorkUnitIDs); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: source_work_unit_ids: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(invariants), &proposal.InvariantsTouched); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: invariants_touched: %v", ErrImprovementCorrupt, err)
	}
	if err := json.Unmarshal([]byte(plan), &proposal.ValidationPlan); err != nil {
		return improvement.Proposal{}, fmt.Errorf("%w: validation_plan: %v", ErrImprovementCorrupt, err)
	}
	return proposal, nil
}

func requireEvidenceRow(ctx context.Context, tx *sql.Tx, taskID, evidenceID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM tool_results WHERE task_id = ? AND evidence_id = ?`, taskID, evidenceID).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("evidence %q/%q does not exist in durable state", taskID, evidenceID)
	}
	if err != nil {
		return fmt.Errorf("check evidence: %w", err)
	}
	return nil
}

func requireWorkUnitOfTask(ctx context.Context, tx *sql.Tx, workUnitID string, sourceTaskIDs []string) error {
	var taskID string
	err := tx.QueryRowContext(ctx,
		`SELECT task_id FROM work_units WHERE work_unit_id = ?`, workUnitID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: work unit %q does not exist", improvement.ErrInvalidProposal, workUnitID)
	}
	if err != nil {
		return fmt.Errorf("check work unit: %w", err)
	}
	for _, candidate := range sourceTaskIDs {
		if taskID == candidate {
			return nil
		}
	}
	return fmt.Errorf("%w: work unit %q belongs to task %q, not to a declared source task", improvement.ErrInvalidProposal, workUnitID, taskID)
}

func requireVersionRow(ctx context.Context, tx *sql.Tx, versionID, targetID string) error {
	var storedTarget string
	err := tx.QueryRowContext(ctx,
		`SELECT target_id FROM improvement_versions WHERE version_id = ?`, versionID).Scan(&storedTarget)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: base version %q does not exist", improvement.ErrInvalidProposal, versionID)
	}
	if err != nil {
		return fmt.Errorf("check base version: %w", err)
	}
	if storedTarget != targetID {
		return fmt.Errorf("%w: base version %q belongs to target %q, not %q", improvement.ErrInvalidProposal, versionID, storedTarget, targetID)
	}
	return nil
}

func jsonArray(values []string) string {
	data, err := json.Marshal(values)
	if err != nil || data == nil {
		return "[]"
	}
	return string(data)
}
