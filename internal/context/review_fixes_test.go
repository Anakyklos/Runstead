package context

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// attemptsByOrigin indexes compiled facts by Kind+Origin for structural
// assertions.
func factsByOrigin(t *testing.T, compiled Compiled, kind FactKind) map[string]Fact {
	t.Helper()
	byOrigin := make(map[string]Fact)
	for _, fact := range compiled.Authoritative {
		if fact.Kind == kind {
			byOrigin[fact.Origin] = fact
		}
	}
	return byOrigin
}

// TestFactAttemptsForToolAndProviderAttempts proves every concrete tool and
// provider attempt of the snapshot is represented as an authoritative typed
// FactAttempt carrying the action -> attempt -> result/evidence relation.
func TestFactAttemptsForToolAndProviderAttempts(t *testing.T) {
	snapshot := testSnapshot()
	compiled := compileOK(t, Input{Snapshot: snapshot})

	attempts := factsByOrigin(t, compiled, FactAttempt)
	wantTool := map[string]string{
		"exec-000001": "tool read_file action action-000001 status completed evidence obs-000001",
		"exec-000002": "tool run_recipe action action-000003 status failed",
		"exec-000003": "tool run_recipe action action-000003 status completed evidence obs-000002",
	}
	for origin, wantSub := range wantTool {
		fact, ok := attempts[origin]
		if !ok {
			t.Fatalf("tool attempt fact %q missing from Compiled.Authoritative", origin)
		}
		if !strings.Contains(fact.Value, wantSub) {
			t.Fatalf("tool attempt fact %q value = %q, want substring %q", origin, fact.Value, wantSub)
		}
	}
	wantProvider := map[string]string{
		"prov-000001": "provider outcome success",
		"prov-000002": "provider outcome unknown",
	}
	for origin, wantSub := range wantProvider {
		fact, ok := attempts[origin]
		if !ok {
			t.Fatalf("provider attempt fact %q missing from Compiled.Authoritative", origin)
		}
		if !strings.Contains(fact.Value, wantSub) {
			t.Fatalf("provider attempt fact %q value = %q, want substring %q", origin, fact.Value, wantSub)
		}
	}
	// The provider client request identity is preserved when relevant.
	withRequest := *testSnapshot()
	withRequest.ProviderAttempts = []state.RecoveryProviderAttempt{
		{ExecutionID: "prov-000009", ClientRequestID: "cr-0009", Status: "uncertain", Outcome: "unknown", Uncertain: true},
	}
	compiledRequest := compileOK(t, Input{Snapshot: &withRequest})
	if fact, ok := factsByOrigin(t, compiledRequest, FactAttempt)["prov-000009"]; !ok || !strings.Contains(fact.Value, "provider request cr-0009") {
		t.Fatalf("provider client request identity not preserved: %+v", fact)
	}
}

// TestFactWorkspaceStructural proves workspace facts are structurally present
// in the typed projection with recorded signature and freshness, and that the
// render and Compiled.Authoritative describe the same authority boundary.
func TestFactWorkspaceStructural(t *testing.T) {
	compiled := compileOK(t, Input{Snapshot: testSnapshot(), CurrentWorkspaceSignature: "sig-a"})
	workspaces := factsByOrigin(t, compiled, FactWorkspace)
	if len(workspaces) != 2 {
		t.Fatalf("workspace facts = %d, want 2 (sig-a, sig-b)", len(workspaces))
	}
	bySignature := make(map[string]Fact)
	for _, fact := range workspaces {
		bySignature[fact.Signature] = fact
	}
	for signature, wantFreshness := range map[string]Freshness{
		"sig-a": FreshnessCurrent,
		"sig-b": FreshnessNeedsRefresh,
	} {
		fact, ok := bySignature[signature]
		if !ok {
			t.Fatalf("workspace fact for signature %q missing", signature)
		}
		if fact.Signature != signature || fact.Freshness != wantFreshness {
			t.Fatalf("workspace fact = %+v, want signature %q freshness %q", fact, signature, wantFreshness)
		}
		if fact.Origin == "" || fact.Value == "" {
			t.Fatalf("workspace fact lacks provenance/value: %+v", fact)
		}
	}
	// Unverified class when no current signature is known.
	unverified := compileOK(t, Input{Snapshot: testSnapshot()})
	for _, fact := range factsByOrigin(t, unverified, FactWorkspace) {
		if fact.Freshness != FreshnessUnverifiedCurrent {
			t.Fatalf("workspace fact = %+v, want unverified_current without current signature", fact)
		}
	}
	// The render describes the same freshness boundary.
	text := compiled.Text()
	for _, want := range []string{"sig-a(current)", "sig-b(needs_refresh)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("render missing freshness marker %q:\n%s", want, text)
		}
	}
}

// degradedMarker returns the render prefix of the degradable line that carries
// the given id for a kind. Pinned lines use different formats, so a marker
// occurrence proves the degradable line itself was rendered.
func degradedMarker(kind FactKind, id string) string {
	switch kind {
	case FactEvidence:
		return "evidence " + id + ":"
	case FactFailure:
		return "failure " + id + ":"
	case FactUncertainEffect:
		return "execution " + id
	case FactApproval:
		return "approval " + id + ":"
	case FactAction:
		return "action " + id + ":"
	case FactAcceptanceCheck:
		return "check " + id + ":"
	case FactVerification:
		return "verification " + id + ":"
	case FactWorkspace:
		return "signature " + id + ":"
	default:
		return ""
	}
}

// TestOmittedNeverContainsRenderedNorDuplicates proves the byte-budget
// omission algorithm records exactly the degradable lines genuinely not
// selected: no id whose degradable line was rendered appears in
// Diagnostics.Omitted, and no omission id is duplicated.
func TestOmittedNeverContainsRenderedNorDuplicates(t *testing.T) {
	for _, bytes := range []int{512, 768, 1024, 1536, 2048, 32 << 10} {
		compiled, err := (&Compiler{}).Compile(Input{
			Snapshot: testSnapshot(),
			Budget:   Budget{MaxContextBytes: bytes},
		})
		if err != nil {
			continue // fail-closed budgets skip the omission assertion
		}
		text := compiled.Text()
		seen := make(map[string]bool)
		for _, omitted := range compiled.Diagnostics.Omitted {
			if omitted.ID == "" {
				t.Fatalf("budget %d: omission without id: %+v", bytes, omitted)
			}
			if seen[omitted.ID] {
				t.Fatalf("budget %d: duplicated omission id %q", bytes, omitted.ID)
			}
			seen[omitted.ID] = true
			marker := degradedMarker(omitted.Kind, omitted.ID)
			if marker != "" && strings.Contains(text, marker) {
				t.Fatalf("budget %d: rendered degradable line %q also marked omitted", bytes, marker)
			}
		}
	}
}

// TestOmittedDiscardedItemsAppearExactlyOnce proves every degradable item
// that was genuinely discarded and identifiable appears exactly once in
// Diagnostics.Omitted, and the selection is deterministic.
func TestOmittedDiscardedItemsAppearExactlyOnce(t *testing.T) {
	input := Input{Snapshot: testSnapshot()}
	first := compileOK(t, input)
	second := compileOK(t, input)
	if first.Text() != second.Text() || len(first.Diagnostics.Omitted) != len(second.Diagnostics.Omitted) {
		t.Fatal("omission selection is not deterministic")
	}
	omitted := make(map[string]int)
	for _, item := range first.Diagnostics.Omitted {
		omitted[item.ID]++
	}
	for id, count := range omitted {
		if count != 1 {
			t.Fatalf("omission id %q appears %d times, want exactly once", id, count)
		}
	}
	for _, item := range first.Diagnostics.Omitted {
		if marker := degradedMarker(item.Kind, item.ID); marker != "" && strings.Contains(first.Text(), marker) {
			t.Fatalf("discarded item id %q appears rendered with marker %q", item.ID, marker)
		}
	}
}
