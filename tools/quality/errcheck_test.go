package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule creates a temporary Go module with the given files
// (relative path -> content) and returns its root.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.com/synth\n\ngo 1.22\n"
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const swallowModule = `package synth

func failing() error { return nil }

func swallow() {
	_ = failing()
}
`

func TestErrcheckDetectsSwallowedError(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": swallowModule})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1", report.Findings)
	}
	f := report.Findings[0]
	if f.File != "synth/synth.go" || f.Line != 6 {
		t.Fatalf("finding = %#v, want synth/synth.go:6", f)
	}
	if !strings.Contains(f.Text, "_ = failing()") {
		t.Fatalf("finding text = %q", f.Text)
	}
	if len(report.Stale) != 0 {
		t.Fatalf("stale = %v, want none", report.Stale)
	}
}

func TestErrcheckDetectsMultiValueSwallow(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func pair() (int, error) { return 0, nil }

func swallow() {
	n, _ := pair()
	_ = n
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (the discarded error in n, _ := pair())", report.Findings)
	}
	if report.Findings[0].Line != 6 {
		t.Fatalf("finding = %v, want line 6", report.Findings[0])
	}
}

func TestErrcheckAllowlistSuppressesFinding(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": swallowModule})
	allowlist := []AllowlistEntry{{File: "synth/synth.go", Line: 6, Text: "_ = failing()"}}
	report, err := RunErrcheck(root, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (allowlisted)", report.Findings)
	}
	if len(report.Stale) != 0 {
		t.Fatalf("stale = %v, want none", report.Stale)
	}
}

func TestErrcheckStaleAllowlistEntryFails(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": swallowModule})
	// The line is a finding, but the recorded text no longer matches the
	// source: the entry is stale and the finding is unprotected.
	allowlist := []AllowlistEntry{{File: "synth/synth.go", Line: 6, Text: "_ = differentCall()"}}
	report, err := RunErrcheck(root, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1", report.Findings)
	}
	if len(report.Stale) != 1 || report.Stale[0].Line != 6 {
		t.Fatalf("stale = %v, want the mismatched entry", report.Stale)
	}
}

func TestErrcheckIgnoresTypeAssertionAndMapLookup(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func assertions(anyValue any, m map[string]int) {
	s, _ := anyValue.(string)
	_ = s
	v, _ := m["missing"]
	_ = v
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (discarded values are bool, not error)", report.Findings)
	}
}

func TestErrcheckIgnoresNonErrorDiscard(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func swallow() {
	x := 42
	_ = x
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (int is not an error)", report.Findings)
	}
}

func TestErrcheckIgnoresTestFiles(t *testing.T) {
	root := writeModule(t, map[string]string{
		"synth/synth.go": `package synth

func failing() error { return nil }
`,
		"synth/synth_test.go": `package synth

import "testing"

func TestSwallow(t *testing.T) {
	_ = failing()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (test files are out of scope)", report.Findings)
	}
}

func TestErrcheckDetectsCustomErrorType(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

type boom struct{}

func (boom) Error() string { return "boom" }

func swallow() {
	_ = boom{}
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (boom implements error)", report.Findings)
	}
}

func TestErrcheckSingleBlankLineDeduped(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func both() (error, error) { return nil, nil }

func swallow() {
	_, _ = both()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (two blanks on one line dedupe)", report.Findings)
	}
}

func TestErrcheckRunOnRealRepoBaselineGreen(t *testing.T) {
	root := repoRoot(t)
	if root == "" {
		t.Skip("current tree does not look like the Runstead repo")
	}
	allowlist, err := DefaultAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunErrcheck(root, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 || len(report.Stale) != 0 {
		var sb strings.Builder
		for _, f := range report.Findings {
			sb.WriteString("\n  swallowed error: " + f.String())
		}
		for _, s := range report.Stale {
			sb.WriteString("\n  stale allowlist entry: " + s.String())
		}
		t.Fatalf("current tree is not errcheck-clean%s", sb.String())
	}
}

func TestParseAllowlistRejectsMalformedLines(t *testing.T) {
	for _, input := range []string{
		"not-an-entry",
		"file.go:abc  text",
		"file.go:7",
		":7  text",
	} {
		if _, err := parseAllowlist([]byte(input)); err == nil {
			t.Errorf("parseAllowlist(%q) succeeded, want error", input)
		}
	}
	if entries, err := parseAllowlist([]byte("# comment\n\nfile.go:7  _ = x()\n")); err != nil || len(entries) != 1 {
		t.Fatalf("parseAllowlist = %v, %v", entries, err)
	}
}

func ExampleAllowlistEntry() {
	e := AllowlistEntry{File: "pkg/a.go", Line: 3, Text: "_ = f()"}
	fmt.Println(e)
	// Output: pkg/a.go:3  _ = f()
}
