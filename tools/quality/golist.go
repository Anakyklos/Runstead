package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// goPackage mirrors the subset of `go list -export -deps -json` that the
// errcheck gate needs.
type goPackage struct {
	ImportPath string
	Name       string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
}

// loadPackages runs `go list -export -deps -json ./...` inside root and
// returns the packages that belong to the module rooted at root plus the
// import path -> compiled export data map for every dependency. The
// checker requires the local Go toolchain and the module's dependencies
// in the local module cache (the CI job runs `go test ./...` first), but
// never the network: GOPROXY=off makes any missing dependency fail fast
// instead of fetching, and `go list -export` compiles export data on
// demand from the local cache and GOROOT, so a cold build cache works.
func loadPackages(root string) ([]goPackage, map[string]string, error) {
	cmd := exec.Command("go", "list", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, nil, fmt.Errorf("go list -export -deps -json ./...: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, nil, fmt.Errorf("go list -export -deps -json ./...: %w", err)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	exports := make(map[string]string)
	var modulePkgs []goPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p goPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, fmt.Errorf("decode go list output: %w", err)
		}
		exports[p.ImportPath] = p.Export
		if p.Dir == "" {
			continue
		}
		absDir, err := filepath.Abs(p.Dir)
		if err != nil {
			return nil, nil, err
		}
		if absDir == absRoot || strings.HasPrefix(absDir, absRoot+string(filepath.Separator)) {
			modulePkgs = append(modulePkgs, p)
		}
	}
	if len(modulePkgs) == 0 {
		return nil, nil, fmt.Errorf("go list returned no packages under %s", root)
	}
	return modulePkgs, exports, nil
}

// openExport returns a read closer for a package's compiled export data.
// The gc importer consumes it to resolve dependency types during
// type-checking. "unsafe" has no export data and is handled by the
// importer itself.
func openExport(exports map[string]string, importPath string) (io.ReadCloser, error) {
	if importPath == "unsafe" {
		return nil, nil
	}
	path := exports[importPath]
	if path == "" {
		return nil, fmt.Errorf("no export data for %q", importPath)
	}
	return os.Open(path)
}
