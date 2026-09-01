package state

// Improvement proposal persistence (issue #55). Three integrity properties
// are enforced here, not just in the CLI:
//
//  1. PROVENANCE COHERENCE: declared source tasks must exist, every evidence
//     ref must point into the declared (or derived) source task set, and
//     every work-unit ref must belong to a declared source task.
//  2. REVISION-BOUND EVIDENCE: validating a proposal requires durable proof
//     that each cited task's FROZEN EXECUTION CONTRACT carries the same M10
//     profile identity as the applied revision, so `validated/positive` can
//     never be produced from evidence of a different configuration.
//  3. DIGEST INTEGRITY: every version load recomputes the SHA-256 of the
//     stored artifact bytes and fails closed on mismatch (tampered SQLite
//     can never make `show --artifact` or rollback deliver different bytes).
//     Every verified load then REPARSES the artifact with the strict M10
//     parser, RE-DERIVES the canonical profile material projection and its
//     digest, and requires equality with the persisted profile_id,
//     profile_version, profile_material_json and profile_material_digest
//     columns: the artifact is the single source of truth, and a row whose
//     material fields diverge from it (even self-consistently) is corrupt.
//
// The artifact FILE is a projection; the verified durable bytes are truth.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/composition"
	"github.com/RenyEnnos/Runstead/internal/improvement"
)

// ErrImprovementCorrupt is the fail-closed state for missing, malformed or
// inconsistent proposal rows.
var ErrImprovementCorrupt = errors.New("corrupt improvement proposal state")

// ProposeImprovement atomically validates the proposal provenance against
// durable state and persists the proposal as pending. Rules:
//
//   - every evidence ref must exist in tool_results;
//   - every declared source task must exist; when none are declared, the
//     source task set is derived from the evidence refs;
//   - every evidence ref task must belong to the source task set;
//   - every work-unit ref must exist AND belong to a source task.
//
// Any inconsistency fails closed with no row written. Text fields are
// redacted before persistence.
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
	// Derive the effective source task set: declared tasks when present,
	// otherwise exactly the evidence tasks.
	sourceTasks := proposal.SourceTaskIDs
	if len(sourceTasks) == 0 {
		seen := make(map[string]struct{})
		for _, ref := range refs {
			seen[ref.TaskID] = struct{}{}
		}
		sourceTasks = make([]string, 0, len(seen))
		for taskID := range seen {
			sourceTasks = append(sourceTasks, taskID)
		}
		sort.Strings(sourceTasks)
	}
	sourceSet := make(map[string]struct{}, len(sourceTasks))
	for _, taskID := range sourceTasks {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			return improvement.Proposal{}, fmt.Errorf("%w: source task ids must be non-empty", improvement.ErrInvalidProposal)
		}
		if err := requireTaskRow(ctx, tx, taskID); err != nil {
			return improvement.Proposal{}, err
		}
		sourceSet[taskID] = struct{}{}
	}
	for _, ref := range refs {
		if err := requireEvidenceRow(ctx, tx, ref.TaskID, ref.EvidenceID); err != nil {
			return improvement.Proposal{}, fmt.Errorf("%w: %v", improvement.ErrInvalidEvidence, err)
		}
		if _, ok := sourceSet[ref.TaskID]; !ok {
			return improvement.Proposal{}, fmt.Errorf("%w: evidence %q/%q belongs to task %q outside the declared source tasks", improvement.ErrInvalidEvidence, ref.TaskID, ref.EvidenceID, ref.TaskID)
		}
	}
	for _, workUnitID := range proposal.SourceWorkUnitIDs {
		if err := requireWorkUnitOfTask(ctx, tx, workUnitID, sourceSet); err != nil {
			return improvement.Proposal{}, err
		}
	}
	proposal.ProposalID = proposalID
	proposal.Status = improvement.StatusPending
	proposal.SourceTaskIDs = sourceTasks
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
// present, derives and enforces the artifact's M10 profile identity (unique
// per target, so a task's frozen contract can later be bound to exactly one
// revision), stores the canonical artifact + digest and returns the created
// version. The artifact FILE is a projection; the verified durable bytes are
// truth.
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
	// The declared base revision is loaded through the VERIFIED path (digest
	// recomputed, target checked). A corrupt base fails closed BEFORE any
	// derived revision is created and BEFORE the proposal lifecycle advances
	// or any artifact is materialized.
	if proposal.TargetBaseVersion != "" {
		if _, err := loadVerifiedVersionTx(ctx, tx, proposal.TargetBaseVersion, proposal.TargetID, ""); err != nil {
			return improvement.Version{}, err
		}
	}
	profile, err := composition.ParseProfile(proposal.ProposedChangeJSON)
	if err != nil {
		return improvement.Version{}, fmt.Errorf("%w: applied change is not a strict Profile document: %v", improvement.ErrInvalidProposal, err)
	}
	material := improvement.ProfileMaterialFromProfile(profile)
	materialDigest, err := material.Digest()
	if err != nil {
		return improvement.Version{}, fmt.Errorf("%w: material digest: %v", ErrImprovementCorrupt, err)
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
		ProfileID: profile.ProfileID, ProfileVersion: profile.ProfileVersion,
		ProfileMaterialJSON: string(material.Canonical()), ProfileMaterialDigest: materialDigest,
		ArtifactDigest: digest, ArtifactJSON: append([]byte(nil), artifact...), CreatedAt: at,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO improvement_versions (version_id, proposal_id, target_id, revision, base_version_id, profile_id, profile_version, profile_material_json, profile_material_digest, artifact_digest, artifact_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		version.VersionID, version.ProposalID, version.TargetID, version.Revision, version.BaseVersionID,
		version.ProfileID, version.ProfileVersion, version.ProfileMaterialJSON, version.ProfileMaterialDigest,
		version.ArtifactDigest, string(version.ArtifactJSON), version.CreatedAt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return improvement.Version{}, fmt.Errorf("%w: target %q already has a revision with profile identity %q@%q; a revision must change the declared profile version", improvement.ErrInvalidProposal, version.TargetID, version.ProfileID, version.ProfileVersion)
		}
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

// ValidateImprovement attaches one later objective validation record to the
// proposal's applied version. Every evidence reference must (a) exist in
// tool_results, and (b) belong to a task whose FROZEN EXECUTION CONTRACT
// carries the SAME M10 profile identity as the applied revision -- durable
// proof the evidence was produced under this exact material, never another
// configuration. A narrative, a phantom ref or a revision mismatch fails
// closed.
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
	version, err := loadVerifiedVersionTx(ctx, tx, proposal.VersionID, proposal.TargetID, proposalID)
	if err != nil {
		return improvement.ValidationRecord{}, err
	}
	for _, ref := range refs {
		if err := requireEvidenceRow(ctx, tx, ref.TaskID, ref.EvidenceID); err != nil {
			return improvement.ValidationRecord{}, fmt.Errorf("%w: %v", improvement.ErrInvalidEvidence, err)
		}
		if err := requireTaskContractProfile(ctx, tx, ref.TaskID, version); err != nil {
			return improvement.ValidationRecord{}, err
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
		ValidationID: validationID, ProposalID: proposalID, VersionID: version.VersionID,
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
// the VERIFIED base artifact bytes and marks the proposal rolled_back. The
// base version's target is checked and its digest recomputed before use. A
// first revision has no base and fails closed. The returned artifact bytes
// are the previous revision; the caller rewrites the materialized file
// projection.
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
	base, err := loadVerifiedVersionTx(ctx, tx, proposal.TargetBaseVersion, proposal.TargetID, "")
	if err != nil {
		return nil, err
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
	return append([]byte(nil), base.ArtifactJSON...), nil
}

// LoadImprovementVersion returns one stored version AFTER recomputing and
// verifying its artifact digest: tampered bytes fail closed instead of being
// handed to show/rollback.
func (s *Store) LoadImprovementVersion(ctx context.Context, versionID string) (improvement.Version, error) {
	return s.loadVerifiedVersion(ctx, versionID)
}

func (s *Store) loadVerifiedVersion(ctx context.Context, versionID string) (improvement.Version, error) {
	var version improvement.Version
	var artifactJSON, proposalID, targetID string
	err := s.db.QueryRowContext(ctx, `
		SELECT version_id, proposal_id, target_id, revision, base_version_id, profile_id, profile_version,
		       profile_material_json, profile_material_digest, artifact_digest, artifact_json, created_at
		FROM improvement_versions WHERE version_id = ?`, versionID).
		Scan(&version.VersionID, &proposalID, &targetID, &version.Revision,
			&version.BaseVersionID, &version.ProfileID, &version.ProfileVersion,
			&version.ProfileMaterialJSON, &version.ProfileMaterialDigest,
			&version.ArtifactDigest, &artifactJSON, &version.CreatedAt)
	if err == sql.ErrNoRows {
		return improvement.Version{}, improvement.ErrUnknownProposal
	}
	if err != nil {
		return improvement.Version{}, fmt.Errorf("%w: %v", ErrImprovementCorrupt, err)
	}
	version.ProposalID = proposalID
	version.TargetID = targetID
	version.ArtifactJSON = []byte(artifactJSON)
	if sum := sha256.Sum256(version.ArtifactJSON); hex.EncodeToString(sum[:]) != version.ArtifactDigest {
		return improvement.Version{}, fmt.Errorf("%w: version %q artifact bytes do not match its persisted digest", ErrImprovementCorrupt, versionID)
	}
	if err := verifyVersionMaterial(&version); err != nil {
		return improvement.Version{}, err
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

// loadVerifiedVersionTx reads one version inside a transaction, verifies its
// artifact digest and (when wanted) its target/proposal identity.
func loadVerifiedVersionTx(ctx context.Context, tx *sql.Tx, versionID, wantTargetID, wantProposalID string) (improvement.Version, error) {
	var version improvement.Version
	var artifactJSON, proposalID, targetID string
	err := tx.QueryRowContext(ctx, `
		SELECT version_id, proposal_id, target_id, revision, base_version_id, profile_id, profile_version,
		       profile_material_json, profile_material_digest, artifact_digest, artifact_json, created_at
		FROM improvement_versions WHERE version_id = ?`, versionID).
		Scan(&version.VersionID, &proposalID, &targetID, &version.Revision,
			&version.BaseVersionID, &version.ProfileID, &version.ProfileVersion,
			&version.ProfileMaterialJSON, &version.ProfileMaterialDigest,
			&version.ArtifactDigest, &artifactJSON, &version.CreatedAt)
	if err == sql.ErrNoRows {
		return improvement.Version{}, improvement.ErrUnknownProposal
	}
	if err != nil {
		return improvement.Version{}, fmt.Errorf("%w: %v", ErrImprovementCorrupt, err)
	}
	version.ProposalID = proposalID
	version.TargetID = targetID
	version.ArtifactJSON = []byte(artifactJSON)
	if sum := sha256.Sum256(version.ArtifactJSON); hex.EncodeToString(sum[:]) != version.ArtifactDigest {
		return improvement.Version{}, fmt.Errorf("%w: version %q artifact bytes do not match its persisted digest", ErrImprovementCorrupt, versionID)
	}
	if err := verifyVersionMaterial(&version); err != nil {
		return improvement.Version{}, err
	}
	if wantTargetID != "" && version.TargetID != wantTargetID {
		return improvement.Version{}, fmt.Errorf("%w: version %q belongs to target %q, not %q", improvement.ErrInvalidProposal, versionID, version.TargetID, wantTargetID)
	}
	if wantProposalID != "" && version.ProposalID != wantProposalID {
		return improvement.Version{}, fmt.Errorf("%w: version %q belongs to proposal %q, not %q", ErrImprovementCorrupt, versionID, version.ProposalID, wantProposalID)
	}
	return version, nil
}

// verifyVersionMaterial reconciles the persisted PROFILE-DETERMINED material
// projection with the VERIFIED artifact. The artifact bytes (already checked
// against artifact_digest) are reparsed with the strict M10 parser and the
// canonical ProfileMaterial projection and its SHA-256 digest are re-derived
// from them; every persisted field (profile_id, profile_version,
// profile_material_json, profile_material_digest) must equal the re-derived
// values. The versioned artifact is therefore the SINGLE source of truth: a
// row whose material columns were altered even self-consistently (material
// JSON plus a recomputed digest) can never change what validation, rollback
// or show decide.
func verifyVersionMaterial(version *improvement.Version) error {
	profile, err := composition.ParseProfile(version.ArtifactJSON)
	if err != nil {
		return fmt.Errorf("%w: version %q artifact is not a strict Profile document: %v", ErrImprovementCorrupt, version.VersionID, err)
	}
	material := improvement.ProfileMaterialFromProfile(profile)
	canonical := string(material.Canonical())
	digest, err := material.Digest()
	if err != nil {
		return fmt.Errorf("%w: version %q material digest: %v", ErrImprovementCorrupt, version.VersionID, err)
	}
	if material.ProfileID != version.ProfileID {
		return fmt.Errorf("%w: version %q profile_id %q does not match the verified artifact profile_id %q", ErrImprovementCorrupt, version.VersionID, version.ProfileID, material.ProfileID)
	}
	if material.ProfileVersion != version.ProfileVersion {
		return fmt.Errorf("%w: version %q profile_version %q does not match the verified artifact profile_version %q", ErrImprovementCorrupt, version.VersionID, version.ProfileVersion, material.ProfileVersion)
	}
	if canonical != version.ProfileMaterialJSON {
		return fmt.Errorf("%w: version %q profile_material_json does not match the material projection re-derived from the verified artifact", ErrImprovementCorrupt, version.VersionID)
	}
	if digest != version.ProfileMaterialDigest {
		return fmt.Errorf("%w: version %q profile_material_digest %q does not match the digest %q re-derived from the verified artifact", ErrImprovementCorrupt, version.VersionID, version.ProfileMaterialDigest, digest)
	}
	return nil
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

func requireTaskRow(ctx context.Context, tx *sql.Tx, taskID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM tasks WHERE task_id = ?`, taskID).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: source task %q does not exist", improvement.ErrInvalidProposal, taskID)
	}
	if err != nil {
		return fmt.Errorf("check task: %w", err)
	}
	return nil
}

func requireWorkUnitOfTask(ctx context.Context, tx *sql.Tx, workUnitID string, sourceTasks map[string]struct{}) error {
	var taskID string
	err := tx.QueryRowContext(ctx,
		`SELECT task_id FROM work_units WHERE work_unit_id = ?`, workUnitID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: work unit %q does not exist", improvement.ErrInvalidProposal, workUnitID)
	}
	if err != nil {
		return fmt.Errorf("check work unit: %w", err)
	}
	if _, ok := sourceTasks[taskID]; !ok {
		return fmt.Errorf("%w: work unit %q belongs to task %q, not to a declared source task", improvement.ErrInvalidProposal, workUnitID, taskID)
	}
	return nil
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

// requireTaskContractProfile proves the cited task's durable frozen execution
// contract carries EXACTLY the applied revision's PROFILE-DETERMINED
// material: profile identity, exact package selections, declared recipe ids
// and declared provider id must all re-derive the revision's canonical
// material digest. The version string is only one input of the projection;
// two revisions sharing id/version but differing in packages, provider or
// recipe material produce different digests and fail closed. The contract
// bytes are revalidated (bytes, hash) before use, so a tampered or missing
// contract can never certify validation evidence.
func requireTaskContractProfile(ctx context.Context, tx *sql.Tx, taskID string, version improvement.Version) error {
	var contractJSON, contractHash string
	err := tx.QueryRowContext(ctx,
		`SELECT execution_contract_json, execution_contract_hash FROM tasks WHERE task_id = ?`, taskID).
		Scan(&contractJSON, &contractHash)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %v", improvement.ErrInvalidEvidence, "task does not exist")
	}
	if err != nil {
		return fmt.Errorf("load task contract: %w", err)
	}
	if strings.TrimSpace(contractJSON) == "" {
		return fmt.Errorf("%w: task %q has no frozen execution contract; evidence cannot be bound to this revision", improvement.ErrEvidenceRevisionMismatch, taskID)
	}
	if err := validateExecutionContractPair(contractJSON, contractHash); err != nil {
		return fmt.Errorf("%w: task %q frozen execution contract is corrupt: %v", ErrImprovementCorrupt, taskID, err)
	}
	var identity struct {
		Profile struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"profile"`
		Packages []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"packages"`
		Provider struct {
			ProviderID string `json:"provider_id"`
		} `json:"provider"`
		RecipeCatalog struct {
			RecipeIDs []string `json:"recipe_ids"`
		} `json:"recipe_catalog"`
	}
	if err := json.Unmarshal([]byte(contractJSON), &identity); err != nil {
		return fmt.Errorf("%w: task %q frozen execution contract is not decodable: %v", ErrImprovementCorrupt, taskID, err)
	}
	var schema improvement.ProfileMaterial
	if err := json.Unmarshal([]byte(version.ProfileMaterialJSON), &schema); err != nil {
		return fmt.Errorf("%w: revision %q material projection is not decodable: %v", ErrImprovementCorrupt, version.VersionID, err)
	}
	packageIDs := make([]string, 0, len(identity.Packages))
	packageVersions := make([]string, 0, len(identity.Packages))
	for _, pkg := range identity.Packages {
		packageIDs = append(packageIDs, pkg.ID)
		packageVersions = append(packageVersions, pkg.Version)
	}
	taskMaterial := improvement.ProfileMaterialFromContract(
		identity.Profile.ID, identity.Profile.Version,
		packageIDs, packageVersions,
		identity.RecipeCatalog.RecipeIDs, identity.Provider.ProviderID, schema)
	taskDigest, err := taskMaterial.Digest()
	if err != nil {
		return fmt.Errorf("%w: material digest: %v", ErrImprovementCorrupt, err)
	}
	if taskDigest != version.ProfileMaterialDigest || !bytes.Equal(taskMaterial.Canonical(), []byte(version.ProfileMaterialJSON)) {
		return fmt.Errorf("%w: task %q ran under profile material %s, not the evaluated revision material %s (profile identity, packages, declared recipe ids or provider material differ)",
			improvement.ErrEvidenceRevisionMismatch, taskID, taskDigest, version.ProfileMaterialDigest)
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
