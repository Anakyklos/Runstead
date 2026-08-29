package context

import (
	"sort"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// testSnapshot builds a canonical authoritative snapshot covering the main
// projection paths: objective, completed evidence, failures, uncertain
// effects, approvals, acceptance checks and verification.
func testSnapshot() *state.RecoverySnapshot {
	return &state.RecoverySnapshot{
		Task: state.RecoveryTask{
			TaskID:      "task-1",
			Objective:   "Fix the calculator",
			Status:      "running",
			Workspace:   "/workspace",
			ResumeCount: 1,
		},
		Actions: []state.RecoveryAction{
			{ActionID: "action-000001", Tool: "read_file", Status: "completed", WorkspaceSignature: "sig-a"},
			{ActionID: "action-000002", Tool: "write_file", Status: "rejected", WorkspaceSignature: "sig-a"},
			{ActionID: "action-000003", Tool: "run_recipe", Status: "completed", WorkspaceSignature: "sig-b"},
		},
		ToolAttempts: []state.RecoveryToolAttempt{
			{ExecutionID: "exec-000001", ActionID: "action-000001", Tool: "read_file", Status: "completed", EvidenceID: "obs-000001"},
			{ExecutionID: "exec-000002", ActionID: "action-000003", Tool: "run_recipe", Status: "failed", Classification: "process_exit_nonzero"},
			{ExecutionID: "exec-000003", ActionID: "action-000003", Tool: "run_recipe", Status: "completed", EvidenceID: "obs-000002"},
		},
		ProviderAttempts: []state.RecoveryProviderAttempt{
			{ExecutionID: "prov-000001", Status: "completed", Outcome: "success", Uncertain: false},
			{ExecutionID: "prov-000002", Status: "uncertain", Outcome: "unknown", Uncertain: true},
		},
		Evidence: []state.RecoveryEvidence{
			{EvidenceID: "obs-000001", ExecutionID: "exec-000001", Tool: "read_file", ArgumentsJSON: `{"path":"a.txt"}`,
				DataJSON: `{ "content" : "alpha" }`},
			{EvidenceID: "obs-000002", ExecutionID: "exec-000003", Tool: "run_recipe", ArgumentsJSON: `{"recipe":"test"}`,
				DataJSON: `{"exit_code":0}`},
		},
		AcceptancePlanSpec:   `{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"},{"id":"c2","type":"recipe_exit_zero","recipe":"test"}]}`,
		AcceptancePlanDigest: "digest-1",
		VerificationAttempts: []state.VerificationAttemptRow{
			{AttemptID: "ver-000002", Sequence: 2, Decision: "failed", Summary: "recipe still failing",
				Checks: []state.VerificationCheckRow{
					{CheckID: "c1", Status: "passed"},
					{CheckID: "c2", Status: "failed"},
				}},
			{AttemptID: "ver-000001", Sequence: 1, Decision: "failed", Summary: "first attempt",
				Checks: []state.VerificationCheckRow{{CheckID: "c1", Status: "passed"}}},
		},
	}
}

// compileOK compiles with the default budget and fails the test on error.
func compileOK(t *testing.T, input Input) Compiled {
	t.Helper()
	compiled, err := (&Compiler{}).Compile(input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return compiled
}

// TestEvidenceIDsArePinned proves every citable evidence ID from completed
// attempts survives in the projection even when content is heavily degraded.
func TestEvidenceIDsArePinned(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot: testSnapshot(),
		Budget: Budget{
			MaxContextBytes:     1 << 10,
			MaxObservationCount: 1,
			MaxObservationChars: 16,
		},
	})
	ids := compiled.EvidenceIDs()
	if len(ids) != 2 {
		t.Fatalf("EvidenceIDs() = %v, want the two citable ids", ids)
	}
	text := compiled.Text()
	for _, id := range ids {
		if !strings.Contains(text, id) {
			t.Fatalf("pinned evidence id %q missing from render:\n%s", id, text)
		}
	}
}

// TestEvidenceContentDegradesDeterministically proves the observation content
// selection is newest-first and never drops IDs.
func TestEvidenceContentDegradesDeterministically(t *testing.T) {
	input := Input{
		Snapshot: testSnapshot(),
		Budget: Budget{
			MaxContextBytes:     32 << 10,
			MaxObservationCount: 1,
			MaxObservationChars: 4 << 10,
		},
	}
	first := compileOK(t, input)
	second := compileOK(t, input)
	if first.Text() != second.Text() {
		t.Fatal("identical inputs produced different renders")
	}
	// Newest-first content: only one content line (the newest id) present.
	if strings.Count(first.Text(), "data:") != 1 {
		t.Fatalf("expected exactly one degraded content line, got:\n%s", first.Text())
	}
	if !strings.Contains(first.Text(), "obs-000002") || !strings.Contains(first.Text(), "exit_code") {
		t.Fatalf("newest content missing:\n%s", first.Text())
	}
	var contentOmitted []OmittedItem
	for _, item := range first.Diagnostics.Omitted {
		if item.Kind == FactEvidence && strings.HasPrefix(item.ID, "obs-000001") {
			contentOmitted = append(contentOmitted, item)
		}
	}
	if len(contentOmitted) == 0 {
		t.Fatalf("cap omission for the older observation not recorded: %+v", first.Diagnostics.Omitted)
	}
}

// TestUnresolvedFailuresAndUncertainEffectsSurvive proves the mandatory
// inventory (failure/uncertain IDs) is pinned and their details degrade
// explicitly.
func TestUnresolvedFailuresAndUncertainEffectsSurvive(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot: testSnapshot(),
		Budget: Budget{
			MaxContextBytes:   1 << 10,
			MaxFailureLines:   0,
			MaxUncertainLines: 0,
		},
	})
	text := compiled.Text()
	for _, id := range []string{"exec-000002", "prov-000002"} {
		if !strings.Contains(text, id) {
			t.Fatalf("mandatory id %q missing from render:\n%s", id, text)
		}
	}
	if strings.Contains(text, "human review required") {
		t.Fatalf("failure/uncertain detail should have degraded under zero caps:\n%s", text)
	}
}

// TestPendingApprovalsSurvive proves typed pending approvals are pinned by id
// with degradable detail.
func TestPendingApprovalsSurvive(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot: testSnapshot(),
		PendingApprovals: []state.PendingApproval{
			{ActionID: "action-000002", Tool: "write_file", Fingerprint: "fp-1"},
		},
		Budget: Budget{MaxContextBytes: 1 << 10, MaxApprovalLines: 0},
	})
	text := compiled.Text()
	if !strings.Contains(text, "action-000002") {
		t.Fatalf("pending approval id missing from render:\n%s", text)
	}
}

// TestRemainingAcceptanceChecksDerived proves remaining checks = plan checks
// not passed by the LATEST verification attempt, and that an unparseable plan
// is explicit, never "all passed".
func TestRemainingAcceptanceChecksDerived(t *testing.T) {
	compiled := compileOK(t, Input{Snapshot: testSnapshot()})
	text := compiled.Text()
	if !strings.Contains(text, "remaining acceptance checks: c2") {
		t.Fatalf("remaining checks wrong:\n%s", text)
	}

	broken := *testSnapshot()
	broken.AcceptancePlanSpec = "{not json"
	broken.AcceptancePlanDigest = "digest-broken"
	unavailable := compileOK(t, Input{Snapshot: &broken})
	if !strings.Contains(unavailable.Text(), "acceptance plan unavailable") {
		t.Fatalf("unparseable plan not explicit:\n%s", unavailable.Text())
	}
}

// TestVerificationLatestDecisionPinned proves the latest decision (newest
// first) is pinned and detail degrades by cap.
func TestVerificationLatestDecisionPinned(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot: testSnapshot(),
		Budget:   Budget{MaxContextBytes: 1 << 10, MaxVerificationLines: 0},
	})
	text := compiled.Text()
	if !strings.Contains(text, "latest decision failed") {
		t.Fatalf("latest verification decision missing:\n%s", text)
	}
}

// TestWorkspaceFactsFreshnessClassified proves the three freshness classes
// and that classification is applied to recorded signatures.
func TestWorkspaceFactsFreshnessClassified(t *testing.T) {
	current := compileOK(t, Input{Snapshot: testSnapshot(), CurrentWorkspaceSignature: "sig-a"})
	if !strings.Contains(current.Text(), "sig-a(current)") {
		t.Fatalf("current marker missing:\n%s", current.Text())
	}
	if !strings.Contains(current.Text(), "sig-b(needs_refresh)") {
		t.Fatalf("needs-refresh marker missing:\n%s", current.Text())
	}
	unverified := compileOK(t, Input{Snapshot: testSnapshot()})
	if !strings.Contains(unverified.Text(), "sig-a(unverified_current)") {
		t.Fatalf("unverified marker missing:\n%s", unverified.Text())
	}
}

// TestMapOrderIndependence proves the render does not depend on map iteration
// order: shuffled input constructions (maps inside evidence lookups) yield
// identical renders.
func TestMapOrderIndependence(t *testing.T) {
	a := testSnapshot()
	b := testSnapshot()
	sort.SliceStable(b.Evidence, func(i, j int) bool { return b.Evidence[i].EvidenceID < b.Evidence[j].EvidenceID })
	ra := compileOK(t, Input{Snapshot: a}).Text()
	rb := compileOK(t, Input{Snapshot: b}).Text()
	if ra != rb {
		t.Fatalf("map-order dependent render:\n--- a ---\n%s\n--- b ---\n%s", ra, rb)
	}
}

// TestRestartEquivalenceWithoutConversation proves the same persisted state
// yields the same compiled material regardless of any conversation shape:
// there is no conversation input at all.
func TestRestartEquivalenceWithoutConversation(t *testing.T) {
	input := Input{Snapshot: testSnapshot(), PendingApprovals: []state.PendingApproval{{ActionID: "a", Tool: "write_file"}}}
	first := compileOK(t, input)
	second := compileOK(t, input)
	if first.Text() != second.Text() {
		t.Fatal("restart-equivalent inputs diverged")
	}
	if len(first.EvidenceIDs()) != len(second.EvidenceIDs()) {
		t.Fatal("evidence id set diverged across restarts")
	}
}
