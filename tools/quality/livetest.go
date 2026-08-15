package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RunLiveConvention enforces the opt-in live test convention per test
// function. A function that reads a RUNSTEAD_LIVE_* environment variable
// (via os.Getenv or os.LookupEnv) must demonstrate within its own body
// that the live path is opt-in and skipped by default: it must call a
// skip method (Skip, Skipf or SkipNow) on its own testing object
// (*testing.T, *testing.B or *testing.F). A skip in another test or
// helper in the same file does not protect the function that reads the
// variable, and a skip on an arbitrary object named like the parameter is
// not accepted either. The check is structural, not a proof of network
// isolation and not a formal flow analysis: it makes the documented
// convention mechanically visible per function instead of per file.
func RunLiveConvention(root string) ([]string, error) {
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == ".git" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileViolations, err := inspectTestFile(data)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, v := range fileViolations {
			violations = append(violations, filepath.ToSlash(rel)+": "+v)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(violations)
	return violations, nil
}

// inspectTestFile reports the per-function live-convention violations in
// one test file. A violation is raised for:
//
//   - a RUNSTEAD_LIVE_* read at package scope (it runs before any test
//     function and cannot be guarded by a test skip);
//   - a function that reads RUNSTEAD_LIVE_* but has no testing-object
//     parameter (*testing.T, *testing.B or *testing.F) to skip on;
//   - a function that reads RUNSTEAD_LIVE_* and has a testing object but
//     never calls Skip/Skipf/SkipNow on that object within its own body.
//
// The testing object is identified by parameter name and declared type
// (the file's import of "testing" is resolved by import path, so an
// aliased import works). The skip is attributed by receiver name within
// the function body, including nested function literals; a closure that
// shadows the parameter name with a different object of the same name is
// a documented structural approximation, because this gate is not a full
// flow analyzer.
func inspectTestFile(data []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testfile.go", data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse test file: %w", err)
	}
	testingName := testingImportName(f)
	var violations []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			if readsLiveEnv(decl) {
				violations = append(violations,
					"reads RUNSTEAD_LIVE_* at package scope, which cannot be guarded by a test skip; move the read into a test function that skips on its testing object")
			}
			continue
		}
		if fn.Body == nil || !readsLiveEnv(fn.Body) {
			continue
		}
		obj := testingObject(fn, testingName)
		if obj == "" {
			violations = append(violations, fmt.Sprintf(
				"%s reads RUNSTEAD_LIVE_* but has no testing object (*testing.T, *testing.B or *testing.F) to skip on; live tests must be opt-in and skipped by default",
				fn.Name.Name))
			continue
		}
		if !skipsOnTestingObject(fn.Body, obj) {
			violations = append(violations, fmt.Sprintf(
				"%s reads RUNSTEAD_LIVE_* but never calls %s.Skip/Skipf/SkipNow in its own body; a skip in another function does not protect this test",
				fn.Name.Name, obj))
		}
	}
	return violations, nil
}

// testingImportName returns the local name of the imported "testing"
// package in the file (the package name itself when not aliased), or ""
// when the file does not import testing.
func testingImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "testing" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "testing"
	}
	return ""
}

// testingObject returns the name of the function's testing-object
// parameter: the first parameter whose declared type is *<testing>.T,
// *<testing>.B or *<testing>.F. It returns "" when the function has no
// testing object.
func testingObject(fn *ast.FuncDecl, testingName string) string {
	if testingName == "" || fn.Type == nil || fn.Type.Params == nil {
		return ""
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 || !isTestingObjectType(field.Type, testingName) {
			continue
		}
		return field.Names[0].Name
	}
	return ""
}

// isTestingObjectType reports whether the expression is a pointer to
// testing.T, testing.B or testing.F, resolved through the file's local
// name for the testing package.
func isTestingObjectType(t ast.Expr, testingName string) bool {
	star, ok := t.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != testingName {
		return false
	}
	switch sel.Sel.Name {
	case "T", "B", "F":
		return true
	}
	return false
}

// readsLiveEnv reports whether the node contains a call to a Getenv or
// LookupEnv method with a RUNSTEAD_LIVE_* string literal argument. Nested
// function literals are part of the node, so a read inside a t.Run
// closure counts as a read of the enclosing test function.
func readsLiveEnv(n ast.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if strings.HasPrefix(strings.Trim(lit.Value, `"`), "RUNSTEAD_LIVE") {
			found = true
			return false
		}
		return true
	})
	return found
}

// skipsOnTestingObject reports whether the function body contains a call
// to a skip method (Skip, Skipf or SkipNow) whose receiver identifier is
// the testing-object parameter name. Nested function literals are part of
// the body, so a skip on the same name inside a t.Run closure counts.
func skipsOnTestingObject(body *ast.BlockStmt, obj string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != obj {
			return true
		}
		switch sel.Sel.Name {
		case "Skip", "Skipf", "SkipNow":
			found = true
			return false
		}
		return true
	})
	return found
}
