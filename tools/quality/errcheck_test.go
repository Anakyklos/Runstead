package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func assertions(anyValue any, m map[string]int, ch chan int) {
	s, _ := anyValue.(string)
	_ = s
	v, _ := m["missing"]
	_ = v
	got, _ := <-ch
	_ = got
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

func TestErrcheckDetectsBareCallReturningError(t *testing.T) {
	// The maintainer's blocker example: os.Remove returns error and the
	// bare call discards it. No blank identifier is involved.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

import "os"

func cleanup(path string) {
	os.Remove(path)
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (bare call discards the error)", report.Findings)
	}
	f := report.Findings[0]
	if f.File != "synth/synth.go" || f.Line != 6 {
		t.Fatalf("finding = %#v, want synth/synth.go:6", f)
	}
	if !strings.Contains(f.Text, "os.Remove(path)") {
		t.Fatalf("finding text = %q", f.Text)
	}
}

func TestErrcheckDetectsMultiValueBareCall(t *testing.T) {
	// A bare call discards every result, including an error component of
	// a multi-value result.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func pair() (int, error) { return 0, nil }

func swallow() {
	pair()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (the error component of pair() is discarded)", report.Findings)
	}
	if report.Findings[0].Line != 6 {
		t.Fatalf("finding = %v, want line 6", report.Findings[0])
	}
}

func TestErrcheckIgnoresBareCallsWithoutErrorResult(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func noop() {}

func value() int { return 42 }

func run() {
	noop()
	value()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (calls without an error result)", report.Findings)
	}
}

func TestErrcheckDetectsDeferAndGoSwallow(t *testing.T) {
	// Policy: defer and go discard the called function's results exactly
	// like a bare call, so an error-typed result is a swallowed error.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func failing() error { return nil }

func swallow() {
	defer failing()
	go failing()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 2 {
		t.Fatalf("findings = %v, want 2 (defer and go each swallow the error)", report.Findings)
	}
	lines := []int{report.Findings[0].Line, report.Findings[1].Line}
	sort.Ints(lines)
	if lines[0] != 6 || lines[1] != 7 {
		t.Fatalf("finding lines = %v, want 6 (defer) and 7 (go)", lines)
	}
	texts := report.Findings[0].Text + " " + report.Findings[1].Text
	if !strings.Contains(texts, "defer failing()") || !strings.Contains(texts, "go failing()") {
		t.Fatalf("finding texts = %q and %q, want defer failing() and go failing()",
			report.Findings[0].Text, report.Findings[1].Text)
	}
}

func TestErrcheckIgnoresDeferAndGoWithoutErrorResult(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func noop() {}

func value() int { return 42 }

func run() {
	defer noop()
	defer value()
	go noop()
	go value()
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (defer/go without an error result)", report.Findings)
	}
}

func TestErrcheckDetectsBareCallOfConcreteErrorType(t *testing.T) {
	// Concrete types implementing error are recognized in the bare-call
	// path, not just through blank identifiers.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

type boom struct{}

func (boom) Error() string { return "boom" }

func explode() boom { return boom{} }

func swallow() {
	explode()
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

func TestErrcheckAllowlistCoversBareCall(t *testing.T) {
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

func failing() error { return nil }

func swallow() {
	failing()
}
`})
	allowlist := []AllowlistEntry{{File: "synth/synth.go", Line: 6, Text: "failing()"}}
	report, err := RunErrcheck(root, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 || len(report.Stale) != 0 {
		t.Fatalf("findings = %v, stale = %v, want none (bare call allowlisted)", report.Findings, report.Stale)
	}
	// A stale entry (same line, different text) still fails.
	report, err = RunErrcheck(root, []AllowlistEntry{{File: "synth/synth.go", Line: 6, Text: "differentCall()"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || len(report.Stale) != 1 {
		t.Fatalf("findings = %v, stale = %v, want the bare call finding plus the stale entry",
			report.Findings, report.Stale)
	}
}

func TestErrcheckExcludedSymbolsAreNotReported(t *testing.T) {
	// The excluded set mirrors errcheck's documented defaults: in-memory
	// buffer writes, fmt output printing and io.Copy* are conventionally
	// ignored and never reported, even though they return error.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

func output(w io.Writer) {
	var sb strings.Builder
	sb.WriteString("x")
	sb.Write([]byte("y"))
	var bb bytes.Buffer
	bb.WriteRune('z')
	fmt.Fprintln(w, "a")
	fmt.Fprintf(w, "%s", "b")
	io.Copy(w, strings.NewReader("c"))
}
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %v, want none (all calls are in the excluded set)", report.Findings)
	}
}

func TestErrcheckExcludedSetDoesNotHideOtherSwallows(t *testing.T) {
	// The excluded set is narrow: a call in the same file that is not an
	// excluded symbol still fails.
	root := writeModule(t, map[string]string{"synth/synth.go": `package synth

import "fmt"

func output() {
	fmt.Fprintln(nil)
	swallow()
}

func swallow() error { return nil }
`})
	report, err := RunErrcheck(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %v, want 1 (the bare swallow() call; fmt.Fprintln is excluded)", report.Findings)
	}
	if report.Findings[0].Line != 7 {
		t.Fatalf("finding = %v, want line 7", report.Findings[0])
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
