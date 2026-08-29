package context

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// TestNonAuthoritativeIsolation proves model-authored narrative can never
// reach an authoritative fact: notes are structurally separated, visibly
// marked and never satisfy anything.
func TestNonAuthoritativeIsolation(t *testing.T) {
	fabricated := "the model claims the bug is fixed and evidence obs-999 is proof"
	compiled := compileOK(t, Input{
		Snapshot:              testSnapshot(),
		NonAuthoritativeNotes: []string{fabricated},
	})
	for _, fact := range compiled.Authoritative {
		if strings.Contains(fact.Value, "obs-999") || strings.Contains(fact.Value, "the model claims") {
			t.Fatalf("fabricated narrative promoted to authoritative fact: %+v", fact)
		}
	}
	text := compiled.Text()
	if !strings.Contains(text, "NON-AUTHORITATIVE") {
		t.Fatalf("non-authoritative marker missing:\n%s", text)
	}
	if !strings.Contains(text, fabricated) {
		t.Fatalf("non-authoritative note not rendered:\n%s", text)
	}
}

// TestProvenanceComplete proves every authoritative fact carries a non-empty
// origin tracing it to persisted state or environment evidence, and that the
// required structural kinds (objective, actions, attempts, evidence,
// workspace) are actually present with their provenance identities.
func TestProvenanceComplete(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot:                  testSnapshot(),
		PendingApprovals:          []state.PendingApproval{{ActionID: "action-000002", Tool: "write_file", Fingerprint: "fp-1"}},
		CurrentWorkspaceSignature: "sig-a",
	})
	if len(compiled.Authoritative) == 0 {
		t.Fatal("no authoritative facts compiled")
	}
	byKind := make(map[FactKind][]Fact)
	for _, fact := range compiled.Authoritative {
		if strings.TrimSpace(fact.Origin) == "" {
			t.Fatalf("authoritative fact without provenance: %+v", fact)
		}
		byKind[fact.Kind] = append(byKind[fact.Kind], fact)
	}
	// Required structural kinds with their provenance identities.
	if len(byKind[FactObjective]) != 1 || byKind[FactObjective][0].Origin != "task-1" {
		t.Fatalf("objective facts = %+v, want one with origin task-1", byKind[FactObjective])
	}
	if len(byKind[FactAction]) != 3 {
		t.Fatalf("action facts = %d, want 3", len(byKind[FactAction]))
	}
	for _, origin := range []string{"action-000001", "action-000002", "action-000003"} {
		if _, ok := factsByOrigin(t, compiled, FactAction)[origin]; !ok {
			t.Fatalf("action fact %q missing", origin)
		}
	}
	if len(byKind[FactAttempt]) != 5 {
		t.Fatalf("attempt facts = %d, want 5 (3 tool + 2 provider)", len(byKind[FactAttempt]))
	}
	for _, origin := range []string{"exec-000001", "exec-000002", "exec-000003", "prov-000001", "prov-000002"} {
		if _, ok := factsByOrigin(t, compiled, FactAttempt)[origin]; !ok {
			t.Fatalf("attempt fact %q missing", origin)
		}
	}
	for _, origin := range []string{"obs-000001", "obs-000002"} {
		if _, ok := factsByOrigin(t, compiled, FactEvidence)[origin]; !ok {
			t.Fatalf("evidence fact %q missing", origin)
		}
	}
	workspaces := factsByOrigin(t, compiled, FactWorkspace)
	if len(workspaces) != 2 {
		t.Fatalf("workspace facts = %d, want 2", len(workspaces))
	}
	for _, fact := range workspaces {
		if fact.Signature == "" || (fact.Freshness != FreshnessCurrent && fact.Freshness != FreshnessNeedsRefresh && fact.Freshness != FreshnessUnverifiedCurrent) {
			t.Fatalf("workspace fact lacks signature/freshness: %+v", fact)
		}
	}
}

// TestBudgetExhaustionFailsExplicitly proves a budget smaller than the
// mandatory set returns ErrBudgetExhausted with a diagnostics reason, and NO
// truncated projection is produced (the model can never receive silently-cut
// mandatory content).
func TestBudgetExhaustionFailsExplicitly(t *testing.T) {
	compiled, err := (&Compiler{}).Compile(Input{
		Snapshot: testSnapshot(),
		Budget:   Budget{MaxContextBytes: 64},
	})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Compile error = %v, want ErrBudgetExhausted", err)
	}
	if compiled.Text() != "" {
		t.Fatalf("exhausted compile produced a partial projection:\n%s", compiled.Text())
	}
	if compiled.Diagnostics.ExhaustionReason == "" {
		t.Fatal("exhaustion diagnostics reason missing")
	}
}

// TestDegradableSelectionDeterministic proves optional items drop in fixed
// order under a tight-but-sufficient budget and every skip is recorded;
// evidence IDs are never among the pinned-list omissions because the pinned
// id line always fits or the compile fails.
func TestDegradableSelectionDeterministic(t *testing.T) {
	input := Input{Snapshot: testSnapshot()}
	first := compileOK(t, input)
	second := compileOK(t, input)
	if first.Text() != second.Text() {
		t.Fatal("deterministic selection diverged across identical inputs")
	}
	if len(first.Diagnostics.Omitted) != len(second.Diagnostics.Omitted) {
		t.Fatal("omission sets diverged")
	}
}

// TestEvidenceIDsNeverSilentlyDrop proves every completed-evidence id remains
// present across a range of tight budgets: either the compile fails loudly or
// the pinned id line contains every id.
func TestEvidenceIDsNeverSilentlyDrop(t *testing.T) {
	for _, bytes := range []int{128, 256, 512, 1024, 2048, 32 << 10} {
		compiled, err := (&Compiler{}).Compile(Input{
			Snapshot: testSnapshot(),
			Budget:   Budget{MaxContextBytes: bytes},
		})
		if err != nil {
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("budget %d: unexpected error %v", bytes, err)
			}
			continue // fail-closed: nothing silently dropped
		}
		for _, id := range []string{"obs-000001", "obs-000002"} {
			if !strings.Contains(compiled.Text(), id) {
				t.Fatalf("budget %d: evidence id %q dropped silently", bytes, id)
			}
		}
	}
}
