package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceResolverNormalizesAndConstrainsPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "dir", "file.txt")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	resolved, failure := workspace.resolve("./dir//file.txt")
	if failure != nil {
		t.Fatalf("resolve() failure = %#v", failure)
	}
	if resolved.relative != "dir/file.txt" || resolved.canonical != file {
		t.Fatalf("resolved = %#v, want normalized path and canonical file", resolved)
	}

	cases := []struct {
		name string
		path string
		code FailureCode
	}{
		{name: "absolute", path: file, code: FailureAbsolutePath},
		{name: "parent", path: "../outside", code: FailurePathTraversal},
		{name: "nested parent", path: "dir/../file.txt", code: FailurePathTraversal},
		{name: "missing", path: "missing.txt", code: FailurePathNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, failure := workspace.resolve(testCase.path)
			if failure == nil || failure.Code != testCase.code {
				t.Fatalf("resolve(%q) = %#v, want %q", testCase.path, failure, testCase.code)
			}
		})
	}
}

func TestWorkspaceResolverAllowsInternalAndRejectsExternalSymlinks(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "inside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspace, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	resolved, failure := workspace.resolve("inside-link")
	if failure != nil || resolved.canonical != inside {
		t.Fatalf("internal symlink resolve = %#v, %#v", resolved, failure)
	}
	_, failure = workspace.resolve("outside-link")
	if failure == nil || failure.Code != FailureSymlinkEscape {
		t.Fatalf("external symlink failure = %#v, want %q", failure, FailureSymlinkEscape)
	}
}
