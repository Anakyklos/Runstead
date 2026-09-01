package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/composition"
	"github.com/RenyEnnos/Runstead/internal/improvement"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// attachFrozenContract writes a canonical M10 frozen execution contract with
// the given profile identity and package selections onto an existing task,
// so the revision-bound validation can prove which EXACT material the task
// ran under.
func attachFrozenContract(t *testing.T, store *Store, taskID, profileID, profileVersion string, packageIDs ...string) {
	t.Helper()
	if len(packageIDs) == 0 {
		packageIDs = []string{"repo.read", "repo.write"}
	}
	packages := make([]composition.PackageRef, 0, len(packageIDs))
	for _, id := range packageIDs {
		packages = append(packages, composition.PackageRef{ID: id, Version: "1.0.0"})
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := composition.Resolve(composition.ResolveInput{
		Profile: composition.Profile{
			Version: composition.ProfileSchemaVersion, ProfileID: profileID,
			ProfileVersion: profileVersion,
			Packages:       packages,
		},
		PackageRegistry: composition.NewBuiltinRegistry(),
		ToolRegistry:    registry,
	})
	if err != nil {
		t.Fatalf("composition.Resolve() error = %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(),
		`UPDATE tasks SET execution_contract_json = ?, execution_contract_hash = ? WHERE task_id = ?`,
		string(resolved.ContractJSON), resolved.ContractHash, taskID); err != nil {
		t.Fatalf("attach contract: %v", err)
	}
}

// seedImprovementTask creates a task with a durable evidence row, a work
// unit belonging to it and a frozen contract whose profile identity matches
// pendingProposal()'s applied artifact (coding@2.0.0).
func seedImprovementTask(t *testing.T, store *Store) (taskID, evidenceID, workUnitID string) {
	return seedImprovementTaskProfile(t, store, "task-evidence", "coding", "2.0.0")
}

func seedImprovementTaskProfile(t *testing.T, store *Store, taskID, profileID, profileVersion string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	mustTask(t, store, taskID)
	attachFrozenContract(t, store, taskID, profileID, profileVersion)
	actionID := mustAction(t, store, taskID, "read_file", `{"path":"a.txt"}`, "fp", "sig")
	mustToolAttempt(t, store, taskID, actionID)
	workUnitID := "wu-" + taskID
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		WorkUnitID: workUnitID, TaskID: taskID, Objective: "read a.txt",
		Tools: []string{"read_file"},
	}); err != nil {
		t.Fatalf("CreateWorkUnit() error = %v", err)
	}
	return taskID, "obs-000001", workUnitID
}

// seedImprovementTaskPackages seeds a task whose frozen contract carries the
// given package selections (default: repo.read + repo.write).
func seedImprovementTaskPackages(t *testing.T, store *Store, taskID, profileID, profileVersion string, packageIDs ...string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	mustTask(t, store, taskID)
	attachFrozenContract(t, store, taskID, profileID, profileVersion, packageIDs...)
	actionID := mustAction(t, store, taskID, "read_file", `{"path":"a.txt"}`, "fp", "sig")
	mustToolAttempt(t, store, taskID, actionID)
	workUnitID := "wu-" + taskID
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		WorkUnitID: workUnitID, TaskID: taskID, Objective: "read a.txt",
		Tools: []string{"read_file"},
	}); err != nil {
		t.Fatalf("CreateWorkUnit() error = %v", err)
	}
	return taskID, "obs-000001", workUnitID
}

func pendingProposal() improvement.Proposal {
	return improvement.Proposal{
		Kind:               improvement.KindComposition,
		ScopeID:            "proj-a",
		Title:              "add repo.write",
		TargetID:           "profiles/coding",
		ProposedChangeJSON: []byte(`{"version":1,"profile_id":"coding","profile_version":"2.0.0","packages":[{"id":"repo.read","version":"1.0.0"},{"id":"repo.write","version":"1.0.0"}]}`),
		Rationale:          "seed",
		ValidationPlan:     []string{"one run"},
	}
}

func TestImprovementProposeWithRealEvidenceAndProvenance(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, workUnitID := seedImprovementTask(t, store)
	proposal := pendingProposal()
	proposal.SourceTaskIDs = []string{taskID}
	proposal.SourceWorkUnitIDs = []string{workUnitID}

	stored, err := store.ProposeImprovement(context.Background(), proposal, []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatalf("ProposeImprovement() error = %v", err)
	}
	if stored.Status != improvement.StatusPending {
		t.Fatalf("status = %q, want pending", stored.Status)
	}
	if !strings.HasPrefix(stored.ProposalID, "prop-") {
		t.Fatalf("proposal id = %q, want prop- prefix", stored.ProposalID)
	}
	loaded, err := store.LoadImprovement(context.Background(), stored.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != improvement.StatusPending || loaded.ScopeID != "proj-a" {
		t.Fatalf("loaded proposal = %+v", loaded)
	}
	refs, err := store.LoadImprovementEvidence(context.Background(), stored.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].TaskID != taskID || refs[0].EvidenceID != evidenceID {
		t.Fatalf("evidence refs = %+v, want the seeded evidence", refs)
	}
}

func TestImprovementProposeMissingEvidenceFailsClosed(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-evidence")
	_, err := store.ProposeImprovement(context.Background(), pendingProposal(),
		[]improvement.EvidenceRef{{TaskID: "task-evidence", EvidenceID: "obs-999999"}})
	if err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("missing evidence error = %v, want ErrInvalidEvidence", err)
	}
	if summaries, listErr := store.ListImprovements(context.Background(), "", ""); listErr != nil || len(summaries) != 0 {
		t.Fatalf("no proposal may be persisted on failed propose: %v / %v", summaries, listErr)
	}
}

func TestImprovementProposeSourceTaskCoherence(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	refs := []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}

	// Declared source task that does not exist fails closed.
	ghost := pendingProposal()
	ghost.SourceTaskIDs = []string{"task-ghost"}
	if _, err := store.ProposeImprovement(ctx, ghost, refs); err == nil || !errors.Is(err, improvement.ErrInvalidProposal) {
		t.Fatalf("ghost source task error = %v, want ErrInvalidProposal", err)
	}
	// Evidence from a task OUTSIDE the declared source set fails closed.
	otherTask, otherEvidence, _ := seedImprovementTaskProfile(t, store, "task-other", "coding", "2.0.0")
	declared := pendingProposal()
	declared.SourceTaskIDs = []string{taskID}
	if _, err := store.ProposeImprovement(ctx, declared,
		[]improvement.EvidenceRef{{TaskID: otherTask, EvidenceID: otherEvidence}}); err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("evidence outside declared source error = %v, want ErrInvalidEvidence", err)
	}
	// A REAL work unit of another task, declared against a different source
	// task, fails closed (regression: previously the test double-prefixed the
	// id and validated a nonexistent unit instead).
	_, _, otherUnit := seedImprovementTaskProfile(t, store, "task-other-2", "coding", "2.0.0")
	foreign := pendingProposal()
	foreign.SourceTaskIDs = []string{taskID}
	foreign.SourceWorkUnitIDs = []string{otherUnit}
	if _, err := store.ProposeImprovement(ctx, foreign, refs); err == nil || !errors.Is(err, improvement.ErrInvalidProposal) {
		t.Fatalf("foreign real work unit error = %v, want ErrInvalidProposal", err)
	}
	// The SAME work unit declared with its own task as source passes.
	matched := pendingProposal()
	matched.SourceTaskIDs = []string{"task-other-2"}
	matched.SourceWorkUnitIDs = []string{otherUnit}
	if _, err := store.ProposeImprovement(ctx, matched,
		[]improvement.EvidenceRef{{TaskID: "task-other-2", EvidenceID: otherEvidence}}); err != nil {
		t.Fatalf("matched work unit rejected: %v", err)
	}
}

func TestImprovementLifecycleEndToEndAndRollback(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"
	refs := []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}

	first, err := store.ProposeImprovement(ctx, pendingProposal(), refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, first.ProposalID, improvement.DecisionApprove, "looks good", "operator", at); err != nil {
		t.Fatal(err)
	}
	version1, err := store.ApplyImprovement(ctx, first.ProposalID, "/tmp/profiles/coding.json", at)
	if err != nil {
		t.Fatalf("ApplyImprovement() error = %v", err)
	}
	if version1.Revision != 1 || version1.BaseVersionID != "" || version1.ArtifactDigest == "" {
		t.Fatalf("version1 = %+v", version1)
	}
	if version1.ProfileID != "coding" || version1.ProfileVersion != "2.0.0" {
		t.Fatalf("version1 profile identity = %q@%q, want coding@2.0.0", version1.ProfileID, version1.ProfileVersion)
	}
	if _, err := store.RollbackImprovement(ctx, first.ProposalID, "", at); err == nil || !errors.Is(err, improvement.ErrNoBaseRevision) {
		t.Fatalf("first-revision rollback error = %v, want ErrNoBaseRevision", err)
	}
	record, err := store.ValidateImprovement(ctx, first.ProposalID, improvement.OutcomePositive, refs, "verified", at)
	if err != nil {
		t.Fatal(err)
	}
	if record.VersionID != version1.VersionID {
		t.Fatalf("validation version = %q, want %q", record.VersionID, version1.VersionID)
	}
	validations, err := store.LoadImprovementValidations(ctx, first.ProposalID)
	if err != nil || len(validations) != 1 {
		t.Fatalf("validations = %v / %v", validations, err)
	}

	// Revision 2 must change the profile version (identity uniqueness).
	secondProposal := pendingProposal()
	secondProposal.TargetBaseVersion = version1.VersionID
	secondProposal.ProposedChangeJSON = []byte(`{"version":1,"profile_id":"coding","profile_version":"2.1.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	second, err := store.ProposeImprovement(ctx, secondProposal, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, second.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	version2, err := store.ApplyImprovement(ctx, second.ProposalID, "/tmp/profiles/coding.json", at)
	if err != nil {
		t.Fatal(err)
	}
	if version2.Revision != 2 || version2.BaseVersionID != version1.VersionID {
		t.Fatalf("version2 = %+v", version2)
	}
	if _, err := store.ApplyImprovement(ctx, second.ProposalID, "/tmp/profiles/coding.json", at); err == nil || !errors.Is(err, improvement.ErrInvalidTransition) {
		t.Fatalf("double apply error = %v, want ErrInvalidTransition", err)
	}
	// A revision reusing a profile identity of the same target fails closed.
	duplicateIdentity := pendingProposal()
	duplicateIdentity.ProposedChangeJSON = []byte(`{"version":1,"profile_id":"coding","profile_version":"2.0.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	duplicateIdentity.TargetBaseVersion = version1.VersionID
	dup, err := store.ProposeImprovement(ctx, duplicateIdentity, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, dup.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyImprovement(ctx, dup.ProposalID, "/tmp/profiles/coding.json", at); err == nil || !errors.Is(err, improvement.ErrInvalidProposal) {
		t.Fatalf("duplicate profile identity apply error = %v, want ErrInvalidProposal", err)
	}

	// Rollback restores revision 1 bytes deterministically (digest-verified).
	restored, err := store.RollbackImprovement(ctx, second.ProposalID, "worse", at)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(version1.ArtifactJSON) {
		t.Fatalf("rollback artifact != revision 1 artifact")
	}
	loadedSecond, err := store.LoadImprovement(ctx, second.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedSecond.Status != improvement.StatusRolledBack || loadedSecond.RolledBackTo != version1.VersionID {
		t.Fatalf("rolled back proposal = %+v", loadedSecond)
	}
	if !strings.Contains(loadedSecond.ReviewReason, "rolled back: worse") {
		t.Fatalf("rollback reason trail missing: %q", loadedSecond.ReviewReason)
	}
}

func TestImprovementRejectedStaysInspectableAndTerminal(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"
	proposal, err := store.ProposeImprovement(ctx, pendingProposal(), []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, proposal.ProposalID, improvement.DecisionReject, "not needed", "operator", at); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadImprovement(ctx, proposal.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != improvement.StatusRejected || loaded.ReviewDecision != string(improvement.DecisionReject) {
		t.Fatalf("rejected proposal = %+v", loaded)
	}
	if err := store.ReviewImprovement(ctx, proposal.ProposalID, improvement.DecisionApprove, "", "operator", at); err == nil || !errors.Is(err, improvement.ErrInvalidTransition) {
		t.Fatalf("re-approve of rejected error = %v, want ErrInvalidTransition", err)
	}
}

func TestImprovementScopeIsolationAndRedaction(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	refs := []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}
	a := pendingProposal()
	a.ScopeID = "proj-a"
	b := pendingProposal()
	b.ScopeID = "proj-b"
	b.Rationale = "Authorization: Bearer sk-live-secret-123"
	if _, err := store.ProposeImprovement(ctx, a, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProposeImprovement(ctx, b, refs); err != nil {
		t.Fatal(err)
	}
	onlyA, err := store.ListImprovements(ctx, "proj-a", "")
	if err != nil || len(onlyA) != 1 || onlyA[0].ScopeID != "proj-a" {
		t.Fatalf("proj-a scope = %v / %v", onlyA, err)
	}
	onlyB, err := store.ListImprovements(ctx, "proj-b", "")
	if err != nil || len(onlyB) != 1 {
		t.Fatalf("proj-b scope = %v / %v", onlyB, err)
	}
	loadedB, err := store.LoadImprovement(ctx, onlyB[0].ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(loadedB.Rationale, "sk-live-secret") {
		t.Fatalf("secret leaked into persisted rationale: %q", loadedB.Rationale)
	}
	if !strings.Contains(loadedB.Rationale, "<redacted>") {
		t.Fatalf("rationale must be redacted: %q", loadedB.Rationale)
	}
}

func TestImprovementValidationRequiresEvidenceAndRevisionMatch(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"
	proposal, err := store.ProposeImprovement(ctx, pendingProposal(), []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, proposal.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyImprovement(ctx, proposal.ProposalID, "/tmp/x.json", at); err != nil {
		t.Fatal(err)
	}
	// Narrative-only validation fails closed.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive, nil, "the model says it helped", at); err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("narrative-only validation error = %v, want ErrInvalidEvidence", err)
	}
	// Phantom evidence fails closed.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: "obs-999999"}}, "", at); err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("phantom evidence validation error = %v, want ErrInvalidEvidence", err)
	}
	// Evidence from a task frozen under a DIFFERENT profile identity fails
	// closed: the validation must prove the task ran under the evaluated
	// revision material.
	otherTask, otherEvidence, _ := seedImprovementTaskProfile(t, store, "task-other-profile", "coding", "9.9.9")
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: otherTask, EvidenceID: otherEvidence}}, "", at); err == nil || !errors.Is(err, improvement.ErrEvidenceRevisionMismatch) {
		t.Fatalf("revision-mismatched evidence error = %v, want ErrEvidenceRevisionMismatch", err)
	}
	// Evidence from a task WITH NO frozen contract fails closed.
	noContractTask := "task-no-contract"
	mustTask(t, store, noContractTask)
	actionID := mustAction(t, store, noContractTask, "read_file", `{"path":"a.txt"}`, "fp", "sig")
	mustToolAttempt(t, store, noContractTask, actionID)
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: noContractTask, EvidenceID: "obs-000001"}}, "", at); err == nil || !errors.Is(err, improvement.ErrEvidenceRevisionMismatch) {
		t.Fatalf("no-contract evidence error = %v, want ErrEvidenceRevisionMismatch", err)
	}
	// Same profile id/version but DIFFERENT packages must fail closed: the
	// version string is never the trust boundary, the material projection is.
	sameVersionTask, sameVersionEvidence, _ := seedImprovementTaskPackages(t, store, "task-same-version", "coding", "2.0.0", "repo.read")
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: sameVersionTask, EvidenceID: sameVersionEvidence}}, "", at); err == nil || !errors.Is(err, improvement.ErrEvidenceRevisionMismatch) {
		t.Fatalf("same-version different-packages evidence error = %v, want ErrEvidenceRevisionMismatch", err)
	}
	// Evidence from a task that DID run under the evaluated revision material
	// passes.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}, "", at); err != nil {
		t.Fatalf("matched-revision validation failed: %v", err)
	}
}

func TestImprovementVersionDigestIntegrity(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"
	proposal, err := store.ProposeImprovement(ctx, pendingProposal(), []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, proposal.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	version, err := store.ApplyImprovement(ctx, proposal.ProposalID, "/tmp/x.json", at)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the stored artifact bytes: every subsequent load must fail
	// closed instead of delivering different bytes.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE improvement_versions SET artifact_json = ? WHERE version_id = ?`,
		`{"version":1,"profile_id":"coding","profile_version":"2.0.0","packages":[{"id":"repo.read","version":"1.0.0"}],"tampered":true}`, version.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadImprovementVersion(ctx, version.VersionID); err == nil || !errors.Is(err, ErrImprovementCorrupt) {
		t.Fatalf("tampered load error = %v, want ErrImprovementCorrupt", err)
	}
	// A DERIVED revision from a corrupt base fails closed BEFORE any new
	// version is created and BEFORE the proposal lifecycle advances.
	second := pendingProposal()
	second.TargetBaseVersion = version.VersionID
	second.ProposedChangeJSON = []byte(`{"version":1,"profile_id":"coding","profile_version":"2.1.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	secondStored, err := store.ProposeImprovement(ctx, second, []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, secondStored.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyImprovement(ctx, secondStored.ProposalID, "/tmp/x.json", at); err == nil || !errors.Is(err, ErrImprovementCorrupt) {
		t.Fatalf("apply over corrupt base error = %v, want ErrImprovementCorrupt", err)
	}
	var versionCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM improvement_versions`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("corrupt base must not create a derived version, got %d rows", versionCount)
	}
	secondReloaded, err := store.LoadImprovement(ctx, secondStored.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if secondReloaded.Status != improvement.StatusApproved {
		t.Fatalf("corrupt base must not advance the proposal lifecycle, status = %q", secondReloaded.Status)
	}
	// Validation over a tampered version also fails closed.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}, "", at); err == nil || !errors.Is(err, ErrImprovementCorrupt) {
		t.Fatalf("validate over tampered version error = %v, want ErrImprovementCorrupt", err)
	}
	// Repairing the tamper restores the happy path: the derived revision can
	// then be applied and rolled back over the restored base.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE improvement_versions SET artifact_json = ? WHERE version_id = ?`,
		string(version.ArtifactJSON), version.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyImprovement(ctx, secondStored.ProposalID, "/tmp/x.json", at); err != nil {
		t.Fatalf("apply after base repair = %v", err)
	}
}

func TestImprovementCrashRestartPreservesLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"

	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	proposal, err := store.ProposeImprovement(ctx, pendingProposal(), []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReviewImprovement(ctx, proposal.ProposalID, improvement.DecisionApprove, "", "operator", at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyImprovement(ctx, proposal.ProposalID, "/tmp/profiles/coding.json", at); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadImprovement(ctx, proposal.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != improvement.StatusApplied || loaded.VersionID == "" {
		t.Fatalf("after restart proposal = %+v", loaded)
	}
	version, err := reopened.LoadImprovementVersion(ctx, loaded.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if string(version.ArtifactJSON) != string(proposal.ProposedChangeJSON) {
		t.Fatal("stored artifact lost across restart")
	}
	if _, err := reopened.ApplyImprovement(ctx, proposal.ProposalID, "/tmp/profiles/coding.json", at); err == nil || !errors.Is(err, improvement.ErrInvalidTransition) {
		t.Fatalf("re-apply after restart error = %v, want ErrInvalidTransition", err)
	}
	if _, err := reopened.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}, "observed after restart", at); err != nil {
		t.Fatalf("validate after restart = %v", err)
	}
}
