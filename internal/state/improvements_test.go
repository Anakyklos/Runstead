package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/improvement"
)

// seedImprovementTask creates a task with a durable evidence row and a work
// unit belonging to it.
func seedImprovementTask(t *testing.T, store *Store) (taskID, evidenceID, workUnitID string) {
	return seedImprovementTaskID(t, store, "task-evidence")
}

func seedImprovementTaskID(t *testing.T, store *Store, taskID string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	mustTask(t, store, taskID)
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
	proposal.TargetBaseVersion = ""
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

func TestImprovementProposeForeignWorkUnitFailsClosed(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-evidence")
	proposal := pendingProposal()
	proposal.SourceWorkUnitIDs = []string{"wu-other-task"}
	if _, err := store.ProposeImprovement(context.Background(), proposal,
		[]improvement.EvidenceRef{{TaskID: "task-evidence", EvidenceID: "obs-999999"}}); err == nil {
		t.Fatal("propose must fail before evidence check when the work unit does not exist")
	}
	// With valid evidence but a work unit of a DIFFERENT task.
	taskID, evidenceID, otherUnit := seedImprovementTaskID(t, store, "task-evidence-2")
	other := pendingProposal()
	other.SourceTaskIDs = []string{taskID}
	other.SourceWorkUnitIDs = []string{"wu-" + otherUnit}
	if _, err := store.ProposeImprovement(context.Background(), other,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}); err == nil || !errors.Is(err, improvement.ErrInvalidProposal) {
		t.Fatalf("foreign work unit error = %v, want ErrInvalidProposal", err)
	}
}

func TestImprovementLifecycleEndToEndAndRollback(t *testing.T) {
	store := openTestStore(t)
	taskID, evidenceID, _ := seedImprovementTask(t, store)
	ctx := context.Background()
	at := "2026-09-01T13:00:00Z"
	refs := []improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}

	// Revision 1 (no base).
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
	// Rollback of the FIRST revision has no base and fails closed.
	if _, err := store.RollbackImprovement(ctx, first.ProposalID, "", at); err == nil || !errors.Is(err, improvement.ErrNoBaseRevision) {
		t.Fatalf("first-revision rollback error = %v, want ErrNoBaseRevision", err)
	}
	// Validate with real evidence.
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

	// Revision 2 based on revision 1.
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
	// Apply is single-use: a third apply fails closed.
	if _, err := store.ApplyImprovement(ctx, second.ProposalID, "/tmp/profiles/coding.json", at); err == nil || !errors.Is(err, improvement.ErrInvalidTransition) {
		t.Fatalf("double apply error = %v, want ErrInvalidTransition", err)
	}

	// Rollback restores revision 1 bytes deterministically.
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
	all, err := store.ListImprovements(ctx, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all scope = %v / %v", all, err)
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

func TestImprovementValidationRequiresEvidence(t *testing.T) {
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
	// Narrative-only validation (no evidence refs) fails closed.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive, nil, "the model says it helped", at); err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("narrative-only validation error = %v, want ErrInvalidEvidence", err)
	}
	// Validation citing nonexistent evidence fails closed.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: "obs-999999"}}, "", at); err == nil || !errors.Is(err, improvement.ErrInvalidEvidence) {
		t.Fatalf("phantom evidence validation error = %v, want ErrInvalidEvidence", err)
	}
	// A pending proposal can never jump straight to validated.
	if _, err := store.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}, "", at); err != nil {
		// proposal is applied at this point, so this must succeed; the
		// pending->validated jump is proven by the transition unit tests.
		t.Fatal(err)
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
	// Crash right after apply: the version row and status are committed, the
	// artifact FILE was never written (it is a projection).
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
	// No duplicate application possible: applying again fails closed.
	if _, err := reopened.ApplyImprovement(ctx, proposal.ProposalID, "/tmp/profiles/coding.json", at); err == nil || !errors.Is(err, improvement.ErrInvalidTransition) {
		t.Fatalf("re-apply after restart error = %v, want ErrInvalidTransition", err)
	}
	// The lifecycle continues cleanly: validation after restart works.
	if _, err := reopened.ValidateImprovement(ctx, proposal.ProposalID, improvement.OutcomePositive,
		[]improvement.EvidenceRef{{TaskID: taskID, EvidenceID: evidenceID}}, "observed after restart", at); err != nil {
		t.Fatalf("validate after restart = %v", err)
	}
}
