package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed limits.json
var defaultLimitsJSON []byte

// Limits is the explicit, reviewable growth configuration. Raising a limit
// requires a visible change to tools/quality/limits.json in the PR.
type Limits struct {
	MaxSourceFileLines   int      `json:"max_source_file_lines"`
	MaxTestFileLines     int      `json:"max_test_file_lines"`
	MaxSourceFilesPerPkg int      `json:"max_source_files_per_package"`
	MaxGoFilesPerPkg     int      `json:"max_go_files_per_package"`
	ExcludeDirs          []string `json:"exclude_dirs"`
}

// DefaultLimits loads the embedded limits.json.
func DefaultLimits() (Limits, error) {
	var l Limits
	if err := json.Unmarshal(defaultLimitsJSON, &l); err != nil {
		return Limits{}, fmt.Errorf("parse embedded limits.json: %w", err)
	}
	return l, nil
}

// GrowthViolation is one bounded-growth failure. String renders the
// file/package, the observed value and the limit so the CI log is
// actionable without extra tooling.
type GrowthViolation struct {
	Target    string
	Observed  int
	Limit     int
	LimitName string
}

func (v GrowthViolation) String() string {
	return fmt.Sprintf("%s: %d exceeds %s limit of %d", v.Target, v.Observed, v.LimitName, v.Limit)
}

// RunGrowth walks the Go tree under root (excluding the configured
// directories) and reports violations of the file-size and per-package
// file-count limits. It is a pure filesystem scan: no network, no
// credentials, no subprocesses.
func RunGrowth(root string, limits Limits) ([]GrowthViolation, error) {
	excluded := make(map[string]bool, len(limits.ExcludeDirs))
	for _, d := range limits.ExcludeDirs {
		excluded[d] = true
	}

	fileLines := make(map[string]int)   // relative path -> line count
	perDirFiles := make(map[string]int) // directory -> total .go files
	perDirSource := make(map[string]int)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && excluded[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileLines[rel] = countLines(data)
		dir := filepath.ToSlash(filepath.Dir(rel))
		perDirFiles[dir]++
		if !strings.HasSuffix(name, "_test.go") {
			perDirSource[dir]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	var violations []GrowthViolation
	for _, rel := range sortedKeys(fileLines) {
		lines := fileLines[rel]
		limit := limits.MaxSourceFileLines
		if strings.HasSuffix(rel, "_test.go") {
			limit = limits.MaxTestFileLines
		}
		if lines > limit {
			violations = append(violations, GrowthViolation{
				Target:    rel,
				Observed:  lines,
				Limit:     limit,
				LimitName: fileLimitName(rel),
			})
		}
	}
	for _, dir := range sortedKeys(perDirFiles) {
		total := perDirFiles[dir]
		if total > limits.MaxGoFilesPerPkg {
			violations = append(violations, GrowthViolation{
				Target:    dir,
				Observed:  total,
				Limit:     limits.MaxGoFilesPerPkg,
				LimitName: "per-package Go file",
			})
		}
		source := perDirSource[dir]
		if source > limits.MaxSourceFilesPerPkg {
			violations = append(violations, GrowthViolation{
				Target:    dir,
				Observed:  source,
				Limit:     limits.MaxSourceFilesPerPkg,
				LimitName: "per-package source file",
			})
		}
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Target != violations[j].Target {
			return violations[i].Target < violations[j].Target
		}
		return violations[i].LimitName < violations[j].LimitName
	})
	return violations, nil
}

func fileLimitName(rel string) string {
	if strings.HasSuffix(rel, "_test.go") {
		return "test file line"
	}
	return "source file line"
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
