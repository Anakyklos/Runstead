package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestListFilesReturnsSortedOneLevelEntriesAndTypes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a-dir", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "inside.txt"), filepath.Join(root, "inside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolListFiles,
		Arguments: arguments(`{"path":"."}`),
	})
	data, ok := observation.Data.(ListData)
	if !ok || observation.Failure != nil || !observation.Success {
		t.Fatalf("observation = %#v, want successful ListData", observation)
	}
	if data.Path != "." || len(data.Entries) != 4 {
		t.Fatalf("list data = %#v", data)
	}
	want := []FileEntry{
		{Path: "a-dir", Type: EntryDirectory},
		{Path: "b.txt", Type: EntryFile, Size: 1},
		{Path: "inside-link", Type: EntrySymlink},
		{Path: "inside.txt", Type: EntryFile, Size: 6},
	}
	for index := range want {
		if data.Entries[index] != want[index] {
			t.Fatalf("entry %d = %#v, want %#v", index, data.Entries[index], want[index])
		}
	}
	if observation.Metadata.Untrusted != true || observation.Metadata.EntriesOriginal != 4 || observation.Metadata.EntriesReturned != 4 {
		t.Fatalf("list metadata = %#v", observation.Metadata)
	}
}

func TestListFilesTruncatesWithoutDescendingIntoDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		writeFixture(t, root, name, name)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "nested"), "hidden", "hidden")
	registry, err := NewRegistry(Options{Workspace: root, Limits: Limits{MaxListEntries: 2}})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolListFiles,
		Arguments: arguments(`{"path":"."}`),
	})
	data, ok := observation.Data.(ListData)
	if !ok || !observation.Success || observation.Failure != nil {
		t.Fatalf("observation = %#v", observation)
	}
	if len(data.Entries) != 2 || data.Entries[0].Path != "nested" || data.Entries[1].Path != "one" {
		t.Fatalf("truncated entries = %#v", data.Entries)
	}
	if !observation.Truncated || observation.Metadata.EntriesOriginal != 4 || observation.Metadata.EntriesReturned != 2 {
		t.Fatalf("truncation metadata = %#v", observation.Metadata)
	}
	for _, entry := range data.Entries {
		if entry.Path == "nested/hidden" {
			t.Fatal("list_files walked recursively")
		}
	}
}

func TestListFilesRejectsFinalInternalDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "target-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "target-dir"), "nested.txt", "nested")
	if err := os.Symlink(filepath.Join(root, "target-dir"), filepath.Join(root, "dir-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolListFiles,
		Arguments: arguments(`{"path":"dir-link"}`),
	})
	if observation.Success || observation.Failure == nil || observation.Failure.Code != FailureWrongType {
		t.Fatalf("observation = %#v, want %q", observation, FailureWrongType)
	}
}

func TestListFilesRejectsExternalIntermediateSymlinkBeforeLstat(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "external-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolListFiles,
		Arguments: arguments(`{"path":"external-link/subdir"}`),
	})
	if observation.Success || observation.Failure == nil || observation.Failure.Code != FailureSymlinkEscape {
		t.Fatalf("observation = %#v, want %q", observation, FailureSymlinkEscape)
	}
}

func TestListFilesRejectsWrongAndEscapingTargets(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	writeFixture(t, root, "file.txt", "file")
	outside := filepath.Join(t.TempDir(), "outside")
	writeFixture(t, filepath.Dir(outside), filepath.Base(outside), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		code FailureCode
	}{
		{path: "file.txt", code: FailureWrongType},
		{path: "outside-link", code: FailureSymlinkEscape},
		{path: "missing", code: FailurePathNotFound},
		{path: file, code: FailureAbsolutePath},
	}
	for _, testCase := range cases {
		t.Run(testCase.path, func(t *testing.T) {
			observation := registry.Execute(context.Background(), protocol.Action{
				Tool:      ToolListFiles,
				Arguments: arguments(`{"path":` + mustJSON(testCase.path) + `}`),
			})
			if observation.Success || observation.Failure == nil || observation.Failure.Code != testCase.code {
				t.Fatalf("observation = %#v, want %q", observation, testCase.code)
			}
		})
	}
}

func mustJSON(value string) string {
	return `"` + value + `"`
}
