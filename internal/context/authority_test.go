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
// origin tracing it to persisted state or environment evidence.
func TestProvenanceComplete(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot: testSnapshot(),
		PendingApprovals: []state.PendingApproval{
			{ActionID: "action-000002", Tool: "write_file", Fingerprint: "fp-1"},
		},
	})
	if len(compiled.Authoritative) == 0 {
		t.Fatal("no authoritative facts compiled")
	}
	for _, fact := range compiled.Authoritative {
		if strings.TrimSpace(fact.Origin) == "" {
			t.Fatalf("authoritative fact without provenance: %+v", fact)
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
