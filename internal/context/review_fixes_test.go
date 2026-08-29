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
		"prov-000001": "provider status completed outcome success",
		"prov-000002": "provider status uncertain outcome unknown",
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
	if fact, ok := factsByOrigin(t, compiledRequest, FactAttempt)["prov-000009"]; !ok ||
		!strings.Contains(fact.Value, "provider request cr-0009") || !strings.Contains(fact.Value, "status uncertain") {
		t.Fatalf("provider client request/status not preserved: %+v", fact)
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

// omissionKey is the identity of one degradable line: the same raw id can
// legitimately appear in different sections (an execution id is shared by the
// attempt, failure and uncertain sections), so uniqueness is per (kind, id).
func omissionKey(item OmittedItem) string { return string(item.Kind) + "\x00" + item.ID }

// TestOmittedNeverContainsRenderedNorDuplicates proves the byte-budget
// omission algorithm records exactly the degradable lines genuinely not
// selected: no id whose degradable line was rendered appears in
// Diagnostics.Omitted, and no omission line identity is duplicated.
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
			if seen[omissionKey(omitted)] {
				t.Fatalf("budget %d: duplicated omission record (%s, %s)", bytes, omitted.Kind, omitted.ID)
			}
			seen[omissionKey(omitted)] = true
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
		omitted[omissionKey(item)]++
	}
	for key, count := range omitted {
		if count != 1 {
			t.Fatalf("omission record %q appears %d times, want exactly once", key, count)
		}
	}
	for _, item := range first.Diagnostics.Omitted {
		if marker := degradedMarker(item.Kind, item.ID); marker != "" && strings.Contains(first.Text(), marker) {
			t.Fatalf("discarded item id %q appears rendered with marker %q", item.ID, marker)
		}
	}
}

// TestAttemptsReachModelFacingContext proves the concrete attempts and their
// action -> execution -> evidence/result relations are part of the RENDERED
// model-facing context (not only Compiled.Authoritative), through the
// recovery BuildContext boundary.
func TestAttemptsReachModelFacingContext(t *testing.T) {
	input := Input{Snapshot: testSnapshot()}
	compiled := compileOK(t, input)
	text := compiled.Text()
	for _, want := range []string{
		"attempt exec-000001: tool read_file action action-000001 status completed evidence obs-000001",
		"attempt exec-000002: tool run_recipe action action-000003 status failed",
		"attempt exec-000003: tool run_recipe action action-000003 status completed evidence obs-000002",
		"attempt prov-000001: provider status completed outcome success",
		"attempt prov-000002: provider status uncertain outcome unknown",
		"concrete attempts: 3 tool, 2 provider",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered context missing attempt relation %q:\n%s", want, text)
		}
	}
}

// boundaryBudget returns the default budget with only MaxContextBytes
// overridden, keeping the section caps identical for exact-fit accounting.
func boundaryBudget(bytes int) Budget {
	base := DefaultBudget()
	base.MaxContextBytes = bytes
	return base
}

// TestBudgetBoundarySingleDegradableLineExactFit proves the byte accounting is
// single-charge: with budget == mandatory bytes + one identifiable degradable
// line, that line is rendered (not omitted) and the output is exactly the
// budget. One byte below that capacity, the line is omitted exactly once and
// the output stays within budget (issue #51 review: no 2*required charge).
func TestBudgetBoundarySingleDegradableLineExactFit(t *testing.T) {
	input := Input{Snapshot: testSnapshot()}
	model := extract(input, DefaultBudget())
	required := pinnedBytes(model.sections)

	var target degradableLine
	for _, sec := range model.sections {
		if sec.kind == FactEvidence && len(sec.degradable) > 0 {
			target = sec.degradable[0]
			break
		}
	}
	if target.id == "" || target.text == "" {
		t.Fatal("no identifiable degradable evidence line found")
	}

	// Exact fit: mandatory + the single earliest degradable line.
	exact := required + len(target.text) + 1
	compiled, err := (&Compiler{}).Compile(Input{Snapshot: testSnapshot(), Budget: boundaryBudget(exact)})
	if err != nil {
		t.Fatalf("exact-fit budget %d failed: %v", exact, err)
	}
	if len(compiled.Text()) != exact {
		t.Fatalf("render = %d bytes, want exactly %d (single-charge accounting)", len(compiled.Text()), exact)
	}
	if !strings.Contains(compiled.Text(), target.text) {
		t.Fatalf("exactly-fitted line missing from render:\n%s", compiled.Text())
	}
	for _, omitted := range compiled.Diagnostics.Omitted {
		if omitted.ID == target.id {
			t.Fatalf("fitted line %q wrongly marked omitted", target.id)
		}
	}

	// One byte below the capacity the same line no longer fits.
	below := required + len(target.text)
	compiledBelow, err := (&Compiler{}).Compile(Input{Snapshot: testSnapshot(), Budget: boundaryBudget(below)})
	if err != nil {
		t.Fatalf("below-capacity budget %d failed: %v", below, err)
	}
	if len(compiledBelow.Text()) > below {
		t.Fatalf("render = %d bytes, exceeds budget %d", len(compiledBelow.Text()), below)
	}
	if strings.Contains(compiledBelow.Text(), target.text) {
		t.Fatalf("line rendered under a budget that cannot fit it:\n%s", compiledBelow.Text())
	}
	count := 0
	for _, omitted := range compiledBelow.Diagnostics.Omitted {
		if omitted.ID == target.id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("omission count for %q = %d, want exactly 1", target.id, count)
	}
}

// TestNonAuthoritativeBoundaryInRender proves the semantic section order is
// preserved in the rendered model-facing text (issue #51 review, round 3):
// every authoritative degradable detail appears BEFORE the NON-AUTHORITATIVE
// marker, and only the non-authoritative notes appear after it.
func TestNonAuthoritativeBoundaryInRender(t *testing.T) {
	compiled := compileOK(t, Input{
		Snapshot:              testSnapshot(),
		NonAuthoritativeNotes: []string{"note-1: a model hypothesis"},
	})
	text := compiled.Text()
	marker := "NON-AUTHORITATIVE"
	// The authority preamble also mentions the boundary; the SECTION marker
	// is the last occurrence and must terminate the authoritative material.
	markerIndex := strings.LastIndex(text, marker)
	if markerIndex < 0 {
		t.Fatalf("NON-AUTHORITATIVE marker missing:\n%s", text)
	}
	before := text[:markerIndex]
	after := text[markerIndex:]
	// Authoritative degradable material must precede the marker.
	for _, want := range []string{
		"evidence obs-000001:",
		"action action-000001:",
		"attempt exec-000001:",
		"failure exec-000002:",
		"execution prov-000002",
		"verification ver-000002:",
		"signature sig-a:",
	} {
		if !strings.Contains(before, want) {
			t.Fatalf("authoritative degradable detail %q appears after the NON-AUTHORITATIVE marker:\n%s", want, text)
		}
	}
	// Only non-authoritative notes follow the marker.
	if !strings.Contains(after, "note-1: a model hypothesis") {
		t.Fatalf("non-authoritative note missing after the marker:\n%s", text)
	}
	for _, forbidden := range []string{"evidence obs-000001:", "attempt exec-000001:", "signature sig-a:"} {
		if strings.Contains(after, forbidden) {
			t.Fatalf("authoritative detail %q appears AFTER the NON-AUTHORITATIVE marker:\n%s", forbidden, text)
		}
	}
}
