package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

func patchAction(path, patch, expected string) protocol.Action {
	return protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolApplyPatch,
		Arguments: protocol.Arguments{
			"path":                 jsonString(path),
			"patch":                jsonString(patch),
			"expected_before_hash": jsonString(expected),
		},
	}
}

func TestApplyPatchAppliesCleanly(t *testing.T) {
	workspace := t.TempDir()
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	patch := "--- a.txt\n+++ a.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n+line2-edited\n line3\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if !observation.Success {
		t.Fatalf("patch failed: %+v", observation.Failure)
	}
	evidence := observation.Data.(tools.WriteEvidence)
	if evidence.ChangeKind != "modified" || evidence.Outcome != tools.WriteSuccess {
		t.Fatalf("change kind/outcome = %q/%q", evidence.ChangeKind, evidence.Outcome)
	}
	if evidence.BeforeHash != before {
		t.Fatalf("before hash = %q", evidence.BeforeHash)
	}
	want := "line1\nline2-edited\nline3\n"
	if evidence.AfterHash != tools.HashBytes([]byte(want)) {
		t.Fatalf("after hash = %q, want %q", evidence.AfterHash, tools.HashBytes([]byte(want)))
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != want {
		t.Fatalf("patched content = %q, want %q", got, want)
	}
	if !containsSubstring(evidence.Diff, "-line2") || !containsSubstring(evidence.Diff, "+line2-edited") {
		t.Fatalf("diff evidence missing patch content:\n%s", evidence.Diff)
	}
}

func TestApplyPatchAddsAndRemovesLines(t *testing.T) {
	workspace := t.TempDir()
	content := "a\nb\nc\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	patch := "--- a.txt\n+++ a.txt\n@@ -1,3 +1,4 @@\n a\n-b\n c\n+d\n+e\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if !observation.Success {
		t.Fatalf("patch failed: %+v", observation.Failure)
	}
	want := "a\nc\nd\ne\n"
	if got := mustReadFile(t, workspace, "a.txt"); got != want {
		t.Fatalf("patched content = %q, want %q", got, want)
	}
}

func TestApplyPatchRejectsStaleBeforeHash(t *testing.T) {
	workspace := t.TempDir()
	content := "v1\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	observed := tools.HashBytes([]byte(content))
	// External change invalidates the precondition.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a.txt\n+++ a.txt\n@@ -1 +1 @@\n-v1\n+v3\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, observed))
	if observation.Success {
		t.Fatal("stale patch must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "v2\n" {
		t.Fatalf("stale patch modified the file: %q", got)
	}
}

func TestApplyPatchRejectsMalformedPatch(t *testing.T) {
	workspace := t.TempDir()
	content := "line1\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	malformed := []string{
		"no headers at all",
		"--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n line1\n-line1\n",             // count mismatch: header says 1 removal but body has 1 context + 1 removal
		"--- other.txt\n+++ other.txt\n@@ -1,1 +1,1 @@\n-line1\n+x\n",         // path mismatch
		"--- a.txt\n+++ a.txt\nindex 123..456\n@@ -1,1 +1,1 @@\n-line1\n+x\n", // unsupported index header
		"--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n line1\n\\ No newline at end of file\n",
	}
	for _, patch := range malformed {
		observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
		if observation.Success {
			t.Fatalf("malformed patch must fail: %q", patch)
		}
		if observation.Failure == nil || observation.Failure.Code != tools.FailureInvalidPatch {
			t.Fatalf("malformed patch %q failure = %+v, want invalid_patch", patch, observation.Failure)
		}
		if got := mustReadFile(t, workspace, "a.txt"); got != content {
			t.Fatalf("malformed patch modified the file: %q", got)
		}
	}
}

func TestApplyPatchRejectsContentMismatchAsStale(t *testing.T) {
	workspace := t.TempDir()
	content := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	// A well-formed patch whose content does not match the file.
	patch := "--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n-nope\n+yep\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if observation.Success {
		t.Fatal("mismatched hunk must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state (content precondition failed)", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != content {
		t.Fatalf("mismatched patch modified the file: %q", got)
	}
}

func TestApplyPatchNoopIsDistinct(t *testing.T) {
	workspace := t.TempDir()
	content := "keep\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	// A patch with only a context line applies but changes nothing.
	patch := "--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n keep\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if !observation.Success {
		t.Fatalf("noop patch failed: %+v", observation.Failure)
	}
	evidence := observation.Data.(tools.WriteEvidence)
	if evidence.Outcome != tools.WriteNoop || evidence.ChangeKind != "unchanged" {
		t.Fatalf("outcome/change = %q/%q, want noop/unchanged", evidence.Outcome, evidence.ChangeKind)
	}
	if evidence.BeforeHash != evidence.AfterHash {
		t.Fatal("noop patch must keep hashes equal")
	}
}

func TestApplyPatchRejectsMissingTarget(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	patch := "--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n-x\n+y\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("patch on a missing file must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailurePathNotFound {
		t.Fatalf("failure = %+v, want path_not_found", observation.Failure)
	}
}

func TestApplyPatchRejectsPathEscapes(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	patch := "--- a.txt\n+++ a.txt\n@@ -1,1 +1,1 @@\n-x\n+y\n"
	observation := registry.Execute(context.Background(), patchAction("../escape.txt", patch, tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("traversal patch must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailurePathTraversal {
		t.Fatalf("failure = %+v, want path_traversal", observation.Failure)
	}
}

func TestApplyPatchWithPrefixedHeaders(t *testing.T) {
	workspace := t.TempDir()
	content := "x\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,1 @@\n-x\n+y\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if !observation.Success {
		t.Fatalf("patch with a/ b/ prefixed headers failed: %+v", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "y\n" {
		t.Fatalf("patched content = %q", got)
	}
}

func TestApplyPatchMultipleHunks(t *testing.T) {
	workspace := t.TempDir()
	content := "a\nb\nc\nd\ne\n"
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte(content))

	patch := "--- a.txt\n+++ a.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+b2\n@@ -4,2 +4,2 @@\n d\n-e\n+e2\n"
	observation := registry.Execute(context.Background(), patchAction("a.txt", patch, before))
	if !observation.Success {
		t.Fatalf("multi-hunk patch failed: %+v", observation.Failure)
	}
	want := "a\nb2\nc\nd\ne2\n"
	if got := mustReadFile(t, workspace, "a.txt"); got != want {
		t.Fatalf("patched content = %q, want %q", got, want)
	}
}
