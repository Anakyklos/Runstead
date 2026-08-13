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
	"strings"
)

// RunLiveConvention enforces the opt-in live test convention: any test
// file that reads a RUNSTEAD_LIVE_* environment variable (via
// os.Getenv or os.LookupEnv) must also contain a call to t.Skip (or any
// testing .Skip method), so the default `go test ./...` path stays
// hermetic and live tests are skipped unless the operator explicitly
// opts in. The check is structural, not a proof of network isolation: it
// makes the documented convention mechanically visible instead of relying
// on review alone.
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
		readsLiveEnv, skips, err := inspectTestFile(data)
		if err != nil {
			return err
		}
		if readsLiveEnv && !skips {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}
			violations = append(violations, fmt.Sprintf(
				"%s: reads RUNSTEAD_LIVE_* but has no t.Skip call; live tests must be opt-in and skipped by default",
				filepath.ToSlash(rel)))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Strings(violations)
	return violations, nil
}

// inspectTestFile reports whether the source reads a RUNSTEAD_LIVE_* env
// variable and whether it contains any .Skip call.
func inspectTestFile(data []byte) (readsLiveEnv, skips bool, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testfile.go", data, parser.SkipObjectResolution)
	if err != nil {
		return false, false, fmt.Errorf("parse test file: %w", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Getenv", "LookupEnv":
				if len(node.Args) == 1 {
					if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						name := strings.Trim(lit.Value, `"`)
						if strings.HasPrefix(name, "RUNSTEAD_LIVE") {
							readsLiveEnv = true
						}
					}
				}
			case "Skip":
				skips = true
			}
		}
		return true
	})
	return readsLiveEnv, skips, nil
}
