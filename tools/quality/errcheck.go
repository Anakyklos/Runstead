package main

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed errcheck.allowlist
var defaultAllowlistData []byte

// AllowlistEntry is one explicit, reviewed exception in
// errcheck.allowlist. Text is the trimmed source line that the entry
// covers; the gate fails when an entry no longer matches the tree
// (stale entry), so exceptions cannot silently go dead.
type AllowlistEntry struct {
	File string
	Line int
	Text string
}

func (e AllowlistEntry) String() string {
	return fmt.Sprintf("%s:%d  %s", e.File, e.Line, e.Text)
}

// SwallowFinding is one discarded error-typed value in a non-test Go
// file: either a blank identifier bound to an error-typed value or a
// function call whose result (any component, for multi-value results) is
// error-typed and is discarded as a bare statement, defer or go. File is
// relative to the analyzed root.
type SwallowFinding struct {
	File string
	Line int
	Text string
}

func (f SwallowFinding) String() string {
	return fmt.Sprintf("%s:%d  %s", f.File, f.Line, f.Text)
}

// ErrcheckReport is the full result of one errcheck run.
type ErrcheckReport struct {
	Findings []SwallowFinding
	Stale    []AllowlistEntry
}

// DefaultAllowlist loads the embedded errcheck.allowlist.
func DefaultAllowlist() ([]AllowlistEntry, error) {
	return parseAllowlist(defaultAllowlistData)
}

// parseAllowlist parses the allowlist format:
//
//	<relative-path>:<line>  <trimmed source line>
//
// Blank lines and lines starting with '#' are comments. A malformed line
// is an error: the allowlist is small and must stay parseable.
func parseAllowlist(data []byte) ([]AllowlistEntry, error) {
	var entries []AllowlistEntry
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sep := strings.Index(line, ":")
		if sep <= 0 {
			return nil, fmt.Errorf("errcheck.allowlist line %d: expected %q", i+1, "path:line  source text")
		}
		file := line[:sep]
		rest := strings.TrimSpace(line[sep+1:])
		sp := strings.Index(rest, " ")
		numPart := rest
		var text string
		if sp > 0 {
			numPart = rest[:sp]
			text = strings.TrimSpace(rest[sp+1:])
		}
		n, err := strconv.Atoi(numPart)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("errcheck.allowlist line %d: invalid line number %q", i+1, numPart)
		}
		if text == "" {
			return nil, fmt.Errorf("errcheck.allowlist line %d: missing source text", i+1)
		}
		entries = append(entries, AllowlistEntry{File: file, Line: n, Text: text})
	}
	return entries, nil
}

// RunErrcheck type-checks every non-test Go file in the module rooted at
// root and reports discarded error-typed values:
//
//   - blank identifiers bound to an error-typed value: `_ = f()`,
//     `x, _ := f()`, `_, _ = f()`;
//   - bare call statements whose call has at least one error-typed
//     result: `f()` (including `os.Remove(path)` style single-result
//     calls and multi-value calls such as `fmt.Println(...)`, where the
//     error component is discarded);
//   - `defer f()` and `go f()` where the call has at least one
//     error-typed result. Policy: a deferred or goroutine-spawned call
//     discards its results exactly like a bare call statement, so it is a
//     swallowed error; there is no silent exclusion. Best-effort cleanup
//     sites are reviewed through the allowlist, never skipped by the
//     analysis.
//
// Findings covered by the allowlist are accepted; findings not in the
// allowlist and allowlist entries that no longer match are failures.
//
// The analysis is type-accurate: go/types resolves the static type of
// every discarded value, so type assertions (`x, _ := v.(T)`), map
// lookups and channel receives (which discard bool, not error) are never
// reported, and concrete types implementing error are reported. Channel
// receives and select cases are out of scope even when the received
// element type implements error: the policy covers function/method call
// results, not value consumption through channels.
func RunErrcheck(root string, allowlist []AllowlistEntry) (*ErrcheckReport, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root = absRoot
	pkgs, exports, err := loadPackages(root)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		return openExport(exports, path)
	})

	var findings []SwallowFinding
	for _, p := range pkgs {
		fs, err := analyzePackage(fset, imp, p, root)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fs...)
	}
	findings = dedupeFindings(findings)
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})

	report := &ErrcheckReport{}
	allowlistByKey := make(map[string]AllowlistEntry, len(allowlist))
	for _, entry := range allowlist {
		allowlistByKey[fmt.Sprintf("%s:%d", entry.File, entry.Line)] = entry
	}
	for _, f := range findings {
		entry, ok := allowlistByKey[fmt.Sprintf("%s:%d", f.File, f.Line)]
		if ok && entry.Text == f.Text {
			continue
		}
		report.Findings = append(report.Findings, f)
	}
	for _, entry := range allowlist {
		if !allowlistMatches(entry, findings) {
			report.Stale = append(report.Stale, entry)
		}
	}
	return report, nil
}

// allowlistMatches reports whether an allowlist entry is still live: the
// same location is still a finding and the recorded text still matches
// the current source line.
func allowlistMatches(entry AllowlistEntry, findings []SwallowFinding) bool {
	for _, f := range findings {
		if f.File == entry.File && f.Line == entry.Line {
			return f.Text == entry.Text
		}
	}
	return false
}

func dedupeFindings(findings []SwallowFinding) []SwallowFinding {
	seen := make(map[string]bool, len(findings))
	var out []SwallowFinding
	for _, f := range findings {
		key := fmt.Sprintf("%s:%d", f.File, f.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// excludedSymbols mirrors the default excluded set of the reference
// errcheck tool: functions whose error result is conventionally ignored.
// The set is explicit and reviewed, not a heuristic: detection above stays
// type-based, and this list only exempts calls whose identity resolves to
// one of these documented symbols. The error is ignored because the
// operation cannot fail meaningfully (in-memory buffers), the result is
// deliberately not actionable (CLI/output printing), or the standard
// library documents the convention (io.Copy*, math/rand.Read,
// os.Stdout/Stderr.Write). Adding or removing an entry is a deliberate
// change to the gate's policy, reviewed like any other gate change.
var excludedSymbols = map[string]bool{
	// bytes: in-memory buffers cannot fail; the error exists only for
	// io.Writer conformance.
	"(*bytes.Buffer).Write":       true,
	"(*bytes.Buffer).WriteByte":   true,
	"(*bytes.Buffer).WriteRune":   true,
	"(*bytes.Buffer).WriteString": true,
	// fmt: printing to a chosen writer is best-effort output; the error
	// is not actionable at the call site.
	"fmt.Print":    true,
	"fmt.Printf":   true,
	"fmt.Println":  true,
	"fmt.Fprint":   true,
	"fmt.Fprintf":  true,
	"fmt.Fprintln": true,
	// io: copying between streams is best-effort by convention.
	"io.Copy":       true,
	"io.CopyBuffer": true,
	"io.CopyN":      true,
	// math/rand and os: stdlib-documented best-effort helpers.
	"math/rand.Read":  true,
	"os.Stdout.Write": true,
	"os.Stderr.Write": true,
	// strings: in-memory builder cannot fail; the error exists only for
	// io.Writer conformance.
	"(*strings.Builder).Write":       true,
	"(*strings.Builder).WriteByte":   true,
	"(*strings.Builder).WriteRune":   true,
	"(*strings.Builder).WriteString": true,
}

// callFullName resolves the called function's identity to its fully
// qualified name (for example "os.Remove", "fmt.Println" or
// "(*strings.Builder).WriteString") using go/types objects. It returns ""
// when the callee is not a statically known function (for example a
// function value), in which case the call is never treated as excluded.
func callFullName(call *ast.CallExpr, info *types.Info) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		if id, ok := call.Fun.(*ast.Ident); ok {
			if fn, ok := info.Uses[id].(*types.Func); ok {
				return fn.FullName()
			}
		}
		return ""
	}
	if fn, ok := info.Uses[sel.Sel].(*types.Func); ok {
		return fn.FullName()
	}
	if s, ok := info.Selections[sel]; ok {
		if fn, ok := s.Obj().(*types.Func); ok {
			return fn.FullName()
		}
	}
	return ""
}

// isExcludedCall reports whether the call's statically resolved function
// is in the documented excluded set.
func isExcludedCall(call *ast.CallExpr, info *types.Info) bool {
	return call != nil && excludedSymbols[callFullName(call, info)]
}

// blankRHS returns the expression that produced the value bound to the
// i-th blank identifier of an assignment, or nil when the assignment
// shape is not one of the covered forms. It mirrors blankType's handling
// of `x, _ := f()` (one multi-valued RHS) and `_ = f()` (one RHS per
// blank).
func blankRHS(assign *ast.AssignStmt, i int) ast.Expr {
	if len(assign.Rhs) == len(assign.Lhs) {
		return assign.Rhs[i]
	}
	if len(assign.Rhs) == 1 {
		return assign.Rhs[0]
	}
	return nil
}

// analyzePackage type-checks one package's non-test files and scans for
// discarded error-typed values: blank identifiers in assignments and
// error-typed results of bare calls, defers and go statements.
func analyzePackage(fset *token.FileSet, imp types.Importer, p goPackage, root string) ([]SwallowFinding, error) {
	files := append(append([]string{}, p.GoFiles...), p.CgoFiles...)
	if len(files) == 0 {
		return nil, nil
	}
	var afs []*ast.File
	contents := make(map[string][]byte)
	for _, f := range files {
		full := filepath.Join(p.Dir, f)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", full, err)
		}
		contents[full] = data
		af, err := parser.ParseFile(fset, full, data, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", full, err)
		}
		afs = append(afs, af)
	}

	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: imp, Error: func(error) {}}
	if _, err := conf.Check(p.ImportPath, fset, afs, info); err != nil {
		return nil, fmt.Errorf("type-check %s: %w", p.ImportPath, err)
	}

	var findings []SwallowFinding
	report := func(pos token.Pos) {
		position := fset.Position(pos)
		rel, err := filepath.Rel(root, position.Filename)
		if err != nil {
			rel = position.Filename
		}
		findings = append(findings, SwallowFinding{
			File: filepath.ToSlash(rel),
			Line: position.Line,
			Text: sourceLine(contents[position.Filename], position.Line),
		})
	}
	for _, af := range afs {
		ast.Inspect(af, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || id.Name != "_" {
						continue
					}
					if t := blankType(node, i, info); t != nil && isErrorType(t) {
						if call, ok := blankRHS(node, i).(*ast.CallExpr); ok && isExcludedCall(call, info) {
							continue
						}
						report(id.Pos())
					}
				}
			case *ast.ExprStmt:
				// A bare call statement discards every result of the
				// call, so an error-typed result is swallowed.
				if call, ok := node.X.(*ast.CallExpr); ok && callDiscardsError(call, info) && !isExcludedCall(call, info) {
					report(node.Pos())
				}
			case *ast.DeferStmt:
				// Policy: defer discards the deferred call's results
				// exactly like a bare call, so an error-typed result is
				// a swallowed error. There is no silent exclusion for
				// best-effort cleanup; such sites are reviewed through
				// the explicit allowlist.
				if callDiscardsError(node.Call, info) && !isExcludedCall(node.Call, info) {
					report(node.Pos())
				}
			case *ast.GoStmt:
				// Policy: go discards the spawned call's results exactly
				// like a bare call, so an error-typed result is a
				// swallowed error (see the DeferStmt case).
				if callDiscardsError(node.Call, info) && !isExcludedCall(node.Call, info) {
					report(node.Pos())
				}
			}
			return true
		})
	}
	return findings, nil
}

// callDiscardsError reports whether a function or method call has at
// least one result whose static type implements error. The call's result
// type is resolved by go/types, so calls without results (for example
// wg.Done() or t.Skip()), calls returning only non-error values and
// concrete types implementing error are all handled uniformly. A call
// with no results has an empty tuple and never matches.
func callDiscardsError(call *ast.CallExpr, info *types.Info) bool {
	tv, ok := info.Types[call]
	if !ok || tv.Type == nil {
		return false
	}
	if tup, ok := tv.Type.(*types.Tuple); ok {
		for i := 0; i < tup.Len(); i++ {
			if isErrorType(tup.At(i).Type()) {
				return true
			}
		}
		return false
	}
	return isErrorType(tv.Type)
}

// blankType resolves the static type bound to the i-th blank identifier
// of an assignment, handling both `x, _ := f()` (one multi-valued RHS)
// and `_ = f()` (one RHS per blank).
func blankType(assign *ast.AssignStmt, i int, info *types.Info) types.Type {
	if len(assign.Rhs) == len(assign.Lhs) {
		if tv, ok := info.Types[assign.Rhs[i]]; ok {
			return tv.Type
		}
		return nil
	}
	if len(assign.Rhs) == 1 {
		tv, ok := info.Types[assign.Rhs[0]]
		if !ok || tv.Type == nil {
			return nil
		}
		if tup, ok := tv.Type.(*types.Tuple); ok && i < tup.Len() {
			return tup.At(i).Type()
		}
		return tv.Type
	}
	return nil
}

// isErrorType reports whether t implements the predeclared error
// interface, using go/types method-set resolution. Untyped nil is not an
// error value.
func isErrorType(t types.Type) bool {
	if t == nil {
		return false
	}
	if basic, ok := t.(*types.Basic); ok && basic.Kind() == types.UntypedNil {
		return false
	}
	errIface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return types.Implements(t, errIface)
}

// sourceLine returns the trimmed text of the given 1-based line, capped
// at 160 characters so allowlist entries stay compact.
func sourceLine(contents []byte, line int) string {
	lines := strings.Split(string(contents), "\n")
	if line-1 >= len(lines) {
		return ""
	}
	text := strings.TrimSpace(lines[line-1])
	if len(text) > 160 {
		text = text[:160]
	}
	return text
}
