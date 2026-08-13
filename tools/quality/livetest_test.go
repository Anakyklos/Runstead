package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLiveConventionPassesWithSkip(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") != "1" {
		t.Skip("opt-in live test")
	}
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

func TestLiveConventionFailsWithoutSkip(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") == "1" {
		t.Log("would dial the provider")
	}
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want 1", violations)
	}
}

func TestLiveConventionIgnoresNonLiveFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "pkg/plain_test.go", `package pkg

import "testing"

func TestPlain(t *testing.T) {}
`)
	writeTestFile(t, root, "pkg/other.go", `package pkg

var _ = "RUNSTEAD_LIVE mentioned in production code is not covered"
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

func TestLiveConventionSkipsVendorAndGit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "vendor/pkg/live_test.go", `package pkg

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") == "1" {
		t.Log("no skip")
	}
}
`)
	writeTestFile(t, root, ".git/x_test.go", `package x

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") == "1" {
		t.Log("no skip")
	}
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (vendor and .git excluded)", violations)
	}
}

func TestLiveConventionCurrentTreePasses(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("current tree does not look like the Runstead repo")
	}
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("current tree violates the live-test convention: %v", violations)
	}
}
