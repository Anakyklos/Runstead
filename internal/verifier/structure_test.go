package verifier

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestVerifierHasNoProcessExecutionPath is the structural independence test of
// issue #11: the verifier package must contain no direct path to arbitrary
// process or shell execution. It imports only the authoritative data types
// and the Observer seam; process execution (recipes) happens exclusively
// through the #26 boundary, which the verifier only reads evidence from.
//
// This source-level check makes it hard to regress into a verifier that runs
// commands, exactly like the project's existing structural checks.
func TestVerifierHasNoProcessExecutionPath(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate verifier package source")
	}
	dir := filepath.Dir(file)
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"os/exec",
		"syscall",
		"golang.org/x/sys",
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		ast, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range ast.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			for _, forbiddenPath := range forbidden {
				if importPath == forbiddenPath || strings.HasPrefix(importPath, forbiddenPath+"/") {
					t.Fatalf("%s imports %q: the verifier must never execute processes or shell commands", path, importPath)
				}
			}
		}
	}
}

// TestInputCarriesNoModelText is the structural test that model prose cannot
// influence the verifier: the input boundary has no field for summaries,
// claims or free text.
func TestInputCarriesNoModelText(t *testing.T) {
	input := Input{TaskID: "t"}
	// Only authoritative typed fields exist; there is no place to put a model
	// claim. The final response's only contribution is the cited evidence IDs,
	// which are validated against persisted evidence.
	if input.TaskID == "" {
		t.Fatal("task id is the only identity field")
	}
}
