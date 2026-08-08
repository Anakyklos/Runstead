package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

func jsonString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func jsonInt(value int) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func writeAction(path, content, expected string) protocol.Action {
	return protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolWriteFile,
		Arguments: protocol.Arguments{
			"path":                 jsonString(path),
			"content":              jsonString(content),
			"expected_before_hash": jsonString(expected),
		},
	}
}

func mustRegistry(t *testing.T, workspace string) *tools.Registry {
	t.Helper()
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	return registry
}

func mustWrite(t *testing.T, registry *tools.Registry, action protocol.Action) tools.Observation {
	t.Helper()
	observation := registry.Execute(context.Background(), action)
	if !observation.Success {
		t.Fatalf("write failed: %+v", observation.Failure)
	}
	return observation
}

func mustReadFile(t *testing.T, root, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func TestWriteFileCreatesNewFileInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	observation := mustWrite(t, registry, writeAction("notes.txt", "hello\n", tools.AbsentBeforeHash))

	evidence, ok := observation.Data.(tools.WriteEvidence)
	if !ok {
		t.Fatalf("observation data = %T, want WriteEvidence", observation.Data)
	}
	if evidence.Path != "notes.txt" {
		t.Fatalf("evidence path = %q, want notes.txt", evidence.Path)
	}
	if evidence.BeforeHash != tools.AbsentBeforeHash {
		t.Fatalf("before hash = %q, want absent", evidence.BeforeHash)
	}
	if evidence.AfterHash == "" || evidence.AfterHash == tools.AbsentBeforeHash {
		t.Fatalf("after hash missing: %q", evidence.AfterHash)
	}
	if evidence.ChangeKind != "created" {
		t.Fatalf("change kind = %q, want created", evidence.ChangeKind)
	}
	if evidence.Outcome != tools.WriteSuccess {
		t.Fatalf("outcome = %q, want success", evidence.Outcome)
	}
	if evidence.ByteCount != int64(len("hello\n")) {
		t.Fatalf("byte count = %d, want %d", evidence.ByteCount, len("hello\n"))
	}
	if evidence.AfterHash != tools.HashBytes([]byte("hello\n")) {
		t.Fatal("after hash must equal the sha256 of the written content")
	}
	if got := mustReadFile(t, workspace, "notes.txt"); got != "hello\n" {
		t.Fatalf("file content = %q, want hello\\n", got)
	}
	if !containsSubstring(evidence.Diff, "-") || !containsSubstring(evidence.Diff, "+") {
		t.Fatalf("diff evidence missing change lines:\n%s", evidence.Diff)
	}
}

func TestWriteFileOverwritesExistingFileWithMatchingPrecondition(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	before := tools.HashBytes([]byte("old\n"))

	observation := mustWrite(t, registry, writeAction("a.txt", "new\n", before))
	evidence := observation.Data.(tools.WriteEvidence)
	if evidence.ChangeKind != "modified" || evidence.Outcome != tools.WriteSuccess {
		t.Fatalf("change kind/outcome = %q/%q, want modified/success", evidence.ChangeKind, evidence.Outcome)
	}
	if evidence.BeforeHash != before {
		t.Fatalf("before hash = %q, want %q", evidence.BeforeHash, before)
	}
	if evidence.AfterHash != tools.HashBytes([]byte("new\n")) {
		t.Fatalf("after hash = %q, want %q", evidence.AfterHash, tools.HashBytes([]byte("new\n")))
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "new\n" {
		t.Fatalf("file content = %q, want new\\n", got)
	}
}

func TestWriteFileRejectsAbsolutePathEscape(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	outside := filepath.Join(t.TempDir(), "escape.txt")

	observation := registry.Execute(context.Background(), writeAction(outside, "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("absolute path write must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureAbsolutePath {
		t.Fatalf("failure = %+v, want absolute_path", observation.Failure)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("absolute escape must not create the file: %v", err)
	}
}

func TestWriteFileRejectsTraversal(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	for _, path := range []string{"../escape.txt", "sub/../../escape.txt"} {
		observation := registry.Execute(context.Background(), writeAction(path, "x", tools.AbsentBeforeHash))
		if observation.Success {
			t.Fatalf("traversal write %q must fail", path)
		}
		if observation.Failure == nil || observation.Failure.Code != tools.FailurePathTraversal {
			t.Fatalf("traversal %q failure = %+v, want path_traversal", path, observation.Failure)
		}
	}
}

func TestWriteFileRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the workspace pointing outside.
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)

	// Writing through the symlinked directory must fail closed.
	observation := registry.Execute(context.Background(), writeAction("link/target.txt", "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("symlink escape write must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureSymlinkEscape {
		t.Fatalf("failure = %+v, want symlink_escape", observation.Failure)
	}
	if got := mustReadFile(t, outside, "target.txt"); got != "keep" {
		t.Fatalf("outside file was modified: %q", got)
	}

	// A symlink as the final component is refused even when it points inside.
	if err := os.Symlink("a.txt", filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("orig"))
	observation = registry.Execute(context.Background(), writeAction("alias", "x", before))
	if observation.Success {
		t.Fatal("writing through a final-component symlink must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureSymlinkEscape {
		t.Fatalf("failure = %+v, want symlink_escape", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "orig" {
		t.Fatalf("symlink target was modified: %q", got)
	}
}

func TestWriteFileRejectsMissingParentDirectory(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	observation := registry.Execute(context.Background(), writeAction("nope/file.txt", "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("write into a missing parent must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailurePathNotFound {
		t.Fatalf("failure = %+v, want path_not_found", observation.Failure)
	}
}

func TestWriteFileRejectsDirectoryTarget(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)

	observation := registry.Execute(context.Background(), writeAction("dir", "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("write to a directory must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureWrongType {
		t.Fatalf("failure = %+v, want wrong_type", observation.Failure)
	}
}

func TestWriteFileStaleStateProtection(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	observed := tools.HashBytes([]byte("v1\n"))

	// External modification invalidates the observed precondition.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), writeAction("a.txt", "v3\n", observed))
	if observation.Success {
		t.Fatal("stale write must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
	if got := mustReadFile(t, workspace, "a.txt"); got != "v2\n" {
		t.Fatalf("stale write modified the file: %q", got)
	}
}

func TestWriteFileStaleAbsentPrecondition(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	// File exists but the model claimed it must be absent.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("exists"), 0o644); err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), writeAction("a.txt", "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("write with absent precondition on an existing file must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureStaleState {
		t.Fatalf("failure = %+v, want stale_state", observation.Failure)
	}
}

func TestWriteFileNoopIsDistinctFromChange(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)
	hash := tools.HashBytes([]byte("same\n"))

	observation := mustWrite(t, registry, writeAction("a.txt", "same\n", hash))
	evidence := observation.Data.(tools.WriteEvidence)
	if evidence.Outcome != tools.WriteNoop {
		t.Fatalf("outcome = %q, want noop", evidence.Outcome)
	}
	if evidence.ChangeKind != "unchanged" {
		t.Fatalf("change kind = %q, want unchanged", evidence.ChangeKind)
	}
	if evidence.BeforeHash != evidence.AfterHash {
		t.Fatalf("noop must keep before == after: %q != %q", evidence.BeforeHash, evidence.AfterHash)
	}
	if evidence.Diff != "" {
		t.Fatalf("noop must not produce diff evidence: %q", evidence.Diff)
	}
	// The file must not have been replaced (inode preserved is not required,
	// but content must be untouched).
	if got := mustReadFile(t, workspace, "a.txt"); got != "same\n" {
		t.Fatalf("file content = %q", got)
	}
}

func TestWriteFileRequiresValidBeforeHash(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	for _, expected := range []string{"", "short", "zzz"} {
		observation := registry.Execute(context.Background(), writeAction("a.txt", "x", expected))
		if observation.Success {
			t.Fatalf("write with expected_before_hash %q must fail validation", expected)
		}
		if observation.Failure == nil || observation.Failure.Code != tools.FailureInvalidArguments {
			t.Fatalf("failure = %+v, want invalid_arguments", observation.Failure)
		}
	}
}

func TestWriteFileReadFileExposesSha256Precondition(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := mustRegistry(t, workspace)

	read := registry.Execute(context.Background(), protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolReadFile,
		Arguments: protocol.Arguments{
			"path": jsonString("a.txt"),
		},
	})
	if !read.Success {
		t.Fatalf("read failed: %+v", read.Failure)
	}
	fileData, ok := read.Data.(tools.FileData)
	if !ok {
		t.Fatalf("read data = %T, want FileData", read.Data)
	}
	wantHash := tools.HashBytes([]byte("payload\n"))
	if fileData.SHA256 != wantHash {
		t.Fatalf("read sha256 = %q, want %q", fileData.SHA256, wantHash)
	}
	// The hash from read_file is a valid precondition for a write.
	observation := mustWrite(t, registry, writeAction("a.txt", "changed\n", fileData.SHA256))
	evidence := observation.Data.(tools.WriteEvidence)
	if evidence.ChangeKind != "modified" {
		t.Fatalf("change kind = %q, want modified", evidence.ChangeKind)
	}
}

func TestWriteFileRejectsUnknownFieldsAndWrongTypes(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)

	// Wrong JSON type for content.
	action := protocol.Action{
		Version: protocol.Current,
		Tool:    tools.ToolWriteFile,
		Arguments: protocol.Arguments{
			"path":                 jsonString("a.txt"),
			"content":              jsonInt(1),
			"expected_before_hash": jsonString(tools.AbsentBeforeHash),
		},
	}
	if registered, err := registry.ValidateArguments(action.Tool, action.Arguments); registered && err == nil {
		t.Fatal("wrong content type must be rejected")
	}
	// Unknown field.
	action.Arguments = protocol.Arguments{
		"path": jsonString("a.txt"), "content": jsonString("x"), "expected_before_hash": jsonString(tools.AbsentBeforeHash), "extra": jsonString("x"),
	}
	if registered, err := registry.ValidateArguments(action.Tool, action.Arguments); registered && err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}

func TestWriteFileCancelledContextNeverWrites(t *testing.T) {
	workspace := t.TempDir()
	registry := mustRegistry(t, workspace)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	observation := registry.Execute(ctx, writeAction("a.txt", "x", tools.AbsentBeforeHash))
	if observation.Success {
		t.Fatal("cancelled write must fail")
	}
	if observation.Failure == nil || observation.Failure.Code != tools.FailureCanceled {
		t.Fatalf("failure = %+v, want canceled", observation.Failure)
	}
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("cancelled write must not create the file: %v", err)
	}
}
