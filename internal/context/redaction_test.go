package context

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// TestRedactionNoSecretsInProjection proves the compiled render and the
// sanitized diagnostics never carry credential-shaped content, even when the
// persisted evidence or the non-authoritative input contains it (the
// persistence boundary must already redact; the compiler re-sanitizes as
// defense in depth).
func TestRedactionNoSecretsInProjection(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.ToolAttempts = append(snapshot.ToolAttempts, state.RecoveryToolAttempt{
		ExecutionID: "exec-000004", ActionID: "action-000003", Tool: "search_text",
		Status: "completed", EvidenceID: "obs-000003",
	})
	snapshot.Evidence = append(snapshot.Evidence, state.RecoveryEvidence{
		EvidenceID:    "obs-000003",
		ExecutionID:   "exec-000004",
		Tool:          "search_text",
		ArgumentsJSON: `{"q":"sk-live-ABCDEFGH123"}`,
		DataJSON:      `{"match":"Bearer sk-live-ABCDEFGH123 secret"}`,
	})
	compiled := compileOK(t, Input{
		Snapshot:              snapshot,
		NonAuthoritativeNotes: []string{"sk-live-ABCDEFGH123 noticed in output"},
	})
	for _, forbidden := range []string{"sk-live-ABCDEFGH123", "Bearer sk-live"} {
		if strings.Contains(compiled.Text(), forbidden) {
			t.Fatalf("render leaks %q:\n%s", forbidden, compiled.Text())
		}
	}
	diag := compiled.RenderDiagnostics()
	for _, forbidden := range []string{"sk-live-ABCDEFGH123", "ABCDEFGH123", "data:"} {
		if strings.Contains(diag, forbidden) {
			t.Fatalf("diagnostics leak %q: %s", forbidden, diag)
		}
	}
	// The redacted evidence is still present as a pinned id.
	if !strings.Contains(compiled.Text(), "obs-000003") {
		t.Fatalf("evidence id lost during redaction:\n%s", compiled.Text())
	}
}

// TestDiagnosticsSanitizedMetadata proves diagnostics expose only counts and
// structure, never item values.
func TestDiagnosticsSanitizedMetadata(t *testing.T) {
	compiled := compileOK(t, Input{Snapshot: testSnapshot()})
	diag := compiled.RenderDiagnostics()
	if !strings.Contains(diag, "version=context-compiler-v0.1") {
		t.Fatalf("diagnostics missing version: %s", diag)
	}
	if !strings.Contains(diag, "budget_bytes=") {
		t.Fatalf("diagnostics missing budget: %s", diag)
	}
	if !strings.Contains(diag, "counts=") {
		t.Fatalf("diagnostics missing counts: %s", diag)
	}
	if strings.Contains(diag, "Fix the calculator") || strings.Contains(diag, "run_recipe") {
		t.Fatalf("diagnostics leak item values: %s", diag)
	}
}
