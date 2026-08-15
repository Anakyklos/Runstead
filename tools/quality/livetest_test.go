package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLiveConventionSkipInOtherTestDoesNotProtect(t *testing.T) {
	// The maintainer's blocker: TestOther's t.Skip must not protect
	// TestLive, which reads RUNSTEAD_LIVE_* without skipping.
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

func TestOther(t *testing.T) {
	t.Skip("irrelevant")
}

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
		t.Fatalf("violations = %v, want exactly 1 (TestLive)", violations)
	}
	if !strings.Contains(violations[0], "TestLive") {
		t.Fatalf("violation = %q, want it to name TestLive", violations[0])
	}
}

func TestLiveConventionNonTestingSkipDoesNotCount(t *testing.T) {
	// A Skip method on an object that is not the function's testing
	// object does not satisfy the convention, even when it appears in the
	// same function.
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

type fake struct{}

func (fake) Skip(string) {}

func TestLive(t *testing.T) {
	var f fake
	f.Skip("not a testing skip")
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
		t.Fatalf("violations = %v, want exactly 1 (f.Skip is not on the testing object)", violations)
	}
}

func TestLiveConventionAcceptsSkipfAndSkipNow(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import "testing"

func TestLive(t *testing.T) {
	if os.Getenv("RUNSTEAD_LIVE_PKG") != "1" {
		t.Skipf("opt-in live test %s", "pkg")
	}
}

func BenchmarkLive(b *testing.B) {
	if os.Getenv("RUNSTEAD_LIVE_BENCH") != "1" {
		b.SkipNow()
	}
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (Skipf on t and SkipNow on b are accepted)", violations)
	}
}

func TestLiveConventionPackageScopeReadFails(t *testing.T) {
	// A package-scope read cannot be guarded by any test skip, so it is
	// always a violation.
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import (
	"os"
	"testing"
)

var liveEnv = os.Getenv("RUNSTEAD_LIVE_PKG")

func TestLive(t *testing.T) {
	t.Skip("opt-in")
	_ = liveEnv
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly 1 (package-scope read)", violations)
	}
	if !strings.Contains(violations[0], "package scope") {
		t.Fatalf("violation = %q, want it to mention package scope", violations[0])
	}
}

func TestLiveConventionHelperWithoutTestingObjectFails(t *testing.T) {
	// A helper that reads RUNSTEAD_LIVE_* and has no testing object
	// cannot demonstrate the skip in its own scope, so it fails
	// closed even when a caller skips.
	root := t.TempDir()
	writeTestFile(t, root, "pkg/live_test.go", `package pkg

import (
	"os"
	"testing"
)

func isLive() bool {
	return os.Getenv("RUNSTEAD_LIVE_PKG") == "1"
}

func TestLive(t *testing.T) {
	if !isLive() {
		t.Skip("opt-in")
	}
}
`)
	violations, err := RunLiveConvention(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want exactly 1 (isLive has no testing object)", violations)
	}
	if !strings.Contains(violations[0], "isLive") {
		t.Fatalf("violation = %q, want it to name isLive", violations[0])
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
