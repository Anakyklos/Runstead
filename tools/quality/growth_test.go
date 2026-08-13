package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLines writes a file containing exactly n lines.
func writeLines(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGrowthPassesBaselineTree(t *testing.T) {
	root := t.TempDir()
	// Files just under the limits: 100-line source, 100-line test,
	// 3 files in one package.
	writeLines(t, filepath.Join(root, "pkg", "a.go"), 100)
	writeLines(t, filepath.Join(root, "pkg", "b.go"), 50)
	writeLines(t, filepath.Join(root, "pkg", "a_test.go"), 100)

	limits := Limits{
		MaxSourceFileLines:   1800,
		MaxTestFileLines:     2400,
		MaxSourceFilesPerPkg: 40,
		MaxGoFilesPerPkg:     60,
		ExcludeDirs:          []string{".git", "vendor", "testdata"},
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

func TestGrowthFailsOversizedSourceFile(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "pkg", "big.go"), 1801)

	limits := Limits{
		MaxSourceFileLines:   1800,
		MaxTestFileLines:     2400,
		MaxSourceFilesPerPkg: 40,
		MaxGoFilesPerPkg:     60,
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want 1", violations)
	}
	v := violations[0]
	if v.Target != "pkg/big.go" || v.Observed != 1801 || v.Limit != 1800 {
		t.Fatalf("violation = %#v, want pkg/big.go 1801 vs 1800", v)
	}
	if !strings.Contains(v.String(), "pkg/big.go") || !strings.Contains(v.String(), "1801") || !strings.Contains(v.String(), "1800") {
		t.Fatalf("violation message %q lacks file/observed/limit", v.String())
	}
}

func TestGrowthTestFileLimitIsDistinct(t *testing.T) {
	root := t.TempDir()
	// 1900 lines is over the source limit but under the test limit; as a
	// _test.go file it must pass, and as a source file it must fail.
	writeLines(t, filepath.Join(root, "pkg", "large_test.go"), 1900)

	limits := Limits{
		MaxSourceFileLines:   1800,
		MaxTestFileLines:     2400,
		MaxSourceFilesPerPkg: 40,
		MaxGoFilesPerPkg:     60,
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("test file over source limit must pass: %v", violations)
	}

	writeLines(t, filepath.Join(root, "pkg", "large.go"), 1900)
	violations, err = RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || violations[0].Target != "pkg/large.go" {
		t.Fatalf("violations = %v, want only pkg/large.go", violations)
	}
}

func TestGrowthFailsPackageFileCount(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 61; i++ {
		writeLines(t, filepath.Join(root, "pkg", fmt.Sprintf("f%03d.go", i)), 5)
	}

	limits := Limits{
		MaxSourceFileLines:   1800,
		MaxTestFileLines:     2400,
		MaxSourceFilesPerPkg: 40,
		MaxGoFilesPerPkg:     60,
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %v, want 2 (total and source file count)", violations)
	}
	for _, v := range violations {
		if v.Target != "pkg" || v.Observed != 61 {
			t.Fatalf("violation = %#v, want pkg with 61 files", v)
		}
	}
}

func TestGrowthExcludesConfiguredDirectories(t *testing.T) {
	root := t.TempDir()
	// Oversized files inside excluded directories must be ignored.
	writeLines(t, filepath.Join(root, "testdata", "huge.go"), 50000)
	writeLines(t, filepath.Join(root, "vendor", "huge.go"), 50000)
	writeLines(t, filepath.Join(root, "fixtures", "app", "huge.go"), 50000)
	writeLines(t, filepath.Join(root, "pkg", "ok.go"), 10)

	limits := Limits{
		MaxSourceFileLines:   1800,
		MaxTestFileLines:     2400,
		MaxSourceFilesPerPkg: 40,
		MaxGoFilesPerPkg:     60,
		ExcludeDirs:          []string{"testdata", "vendor", "fixtures"},
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (excluded dirs ignored)", violations)
	}
}

func TestGrowthCurrentTreePasses(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("current tree does not look like the Runstead repo")
	}
	limits, err := DefaultLimits()
	if err != nil {
		t.Fatal(err)
	}
	violations, err := RunGrowth(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		var sb strings.Builder
		for _, v := range violations {
			sb.WriteString("\n  " + v.String())
		}
		t.Fatalf("current tree violates growth limits; raise limits or fix growth:%s", sb.String())
	}
}

// repoRoot returns the Runstead repository root when the tests run from
// tools/quality, or "" when the layout does not match. The main module's
// go.mod declares exactly "module github.com/RenyEnnos/Runstead".
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(line) == "module github.com/RenyEnnos/Runstead" {
					return dir
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
