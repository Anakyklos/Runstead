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

// SwallowFinding is one blank identifier bound to an error-typed value in
// a non-test Go file. File is relative to the analyzed root.
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
// root and reports blank-identifier discards of error-typed values.
// Findings covered by the allowlist are accepted; findings not in the
// allowlist and allowlist entries that no longer match are failures.
//
// The analysis is type-accurate: go/types resolves the static type of
// every discarded value, so type assertions (`x, _ := v.(T)`), map
// lookups and channel receives (which discard bool, not error) are never
// reported, and concrete types implementing error are reported.
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

// analyzePackage type-checks one package's non-test files and scans for
// blank-identifier error discards.
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
	for _, af := range afs {
		ast.Inspect(af, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assign.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != "_" {
					continue
				}
				if t := blankType(assign, i, info); t != nil && isErrorType(t) {
					pos := fset.Position(id.Pos())
					rel, err := filepath.Rel(root, pos.Filename)
					if err != nil {
						rel = pos.Filename
					}
					findings = append(findings, SwallowFinding{
						File: filepath.ToSlash(rel),
						Line: pos.Line,
						Text: sourceLine(contents[pos.Filename], pos.Line),
					})
				}
			}
			return true
		})
	}
	return findings, nil
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
