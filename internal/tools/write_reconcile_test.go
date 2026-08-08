package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// ReconcileWrite tests: an interrupted write attempt (class 2) is classified
// from observable filesystem state alone. The persisted intent carries the
// before precondition and the expected after-state hash recorded at TX 1.

func reconcileIntent(tool, arguments string, expectedAfterHash string) tools.WriteIntent {
	return tools.WriteIntent{Tool: tool, Arguments: []byte(arguments), ExpectedAfterHash: expectedAfterHash}
}

func TestReconcileWriteBeforeEffectFileUnchanged(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`,
		after,
	))
	if result.Status != tools.ReconcileNotStarted {
		t.Fatalf("status = %q, want effect_not_started (file still matches the precondition)", result.Status)
	}
}

func TestReconcileWriteBeforeEffectNewFileStillAbsent(t *testing.T) {
	workspace := t.TempDir()
	after := tools.HashBytes([]byte("new\n"))

	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"new.txt","content":"new\n","expected_before_hash":"absent"}`,
		after,
	))
	if result.Status != tools.ReconcileNotStarted {
		t.Fatalf("status = %q, want effect_not_started", result.Status)
	}
}

func TestReconcileWriteAfterEffectFileMatchesExpectedAfter(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`,
		after,
	))
	if result.Status != tools.ReconcileCompleted {
		t.Fatalf("status = %q, want effect_completed", result.Status)
	}
	if result.Evidence.Path != "a.txt" || result.Evidence.BeforeHash != before || result.Evidence.AfterHash != after {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if result.Evidence.ChangeKind != "modified" {
		t.Fatalf("change kind = %q, want modified", result.Evidence.ChangeKind)
	}
}

func TestReconcileWriteAfterEffectNewFileCreated(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("created\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := tools.HashBytes([]byte("created\n"))

	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"new.txt","content":"created\n","expected_before_hash":"absent"}`,
		after,
	))
	if result.Status != tools.ReconcileCompleted {
		t.Fatalf("status = %q, want effect_completed", result.Status)
	}
	if result.Evidence.ChangeKind != "created" {
		t.Fatalf("change kind = %q, want created", result.Evidence.ChangeKind)
	}
}

func TestReconcileWriteUnreconcilableStateRequiresHumanReview(t *testing.T) {
	workspace := t.TempDir()
	// The file matches neither the recorded precondition nor the expected
	// after-state: an external change happened after the crash.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("someone-else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`,
		after,
	))
	if result.Status != tools.ReconcileHumanReview {
		t.Fatalf("status = %q, want human_review_required", result.Status)
	}
}

func TestReconcileWriteUnreconcilableWhenExpectedBeforeHashMissing(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A persisted intent whose precondition hash is impossible (file existed
	// at intent time per the expected-after hash, but is now absent).
	after := tools.HashBytes([]byte("x\n"))
	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"missing.txt","content":"x\n","expected_before_hash":"`+tools.HashBytes([]byte("old\n"))+`"}`,
		after,
	))
	if result.Status != tools.ReconcileHumanReview {
		t.Fatalf("status = %q, want human_review_required (precondition state vanished)", result.Status)
	}
}

func TestReconcileWriteMalformedIntentRequiresHumanReview(t *testing.T) {
	workspace := t.TempDir()
	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"a.txt"}`,
		"",
	))
	if result.Status != tools.ReconcileHumanReview {
		t.Fatalf("status = %q, want human_review_required for malformed intent", result.Status)
	}
}

func TestReconcileWriteRejectsPathEscapes(t *testing.T) {
	workspace := t.TempDir()
	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolWriteFile,
		`{"path":"../escape.txt","content":"x\n","expected_before_hash":"absent"}`,
		tools.HashBytes([]byte("x\n")),
	))
	if result.Status != tools.ReconcileHumanReview {
		t.Fatalf("status = %q, want human_review_required for escaped path", result.Status)
	}
}

func TestReconcileWriteApplyPatchCompleted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("line1\nline2\n"))
	patched := []byte("line1\nline2-edited\n")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), patched, 0o644); err != nil {
		t.Fatal(err)
	}
	after := tools.HashBytes(patched)

	patch := "--- a.txt\n+++ a.txt\n@@ -1,2 +1,2 @@\n line1\n-line2\n+line2-edited\n"
	result := tools.ReconcileWrite(context.Background(), workspace, reconcileIntent(
		tools.ToolApplyPatch,
		`{"path":"a.txt","patch":"`+escapeJSON(patch)+`","expected_before_hash":"`+before+`"}`,
		after,
	))
	if result.Status != tools.ReconcileCompleted {
		t.Fatalf("status = %q, want effect_completed", result.Status)
	}
	if result.Evidence.AfterHash != after {
		t.Fatalf("evidence after hash = %q, want %q", result.Evidence.AfterHash, after)
	}
}

func escapeJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded[1 : len(encoded)-1])
}
