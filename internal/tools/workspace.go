package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type workspace struct {
	root string
}

type resolvedPath struct {
	relative  string
	canonical string
	info      os.FileInfo
}

func newWorkspace(configured string) (workspace, error) {
	if strings.TrimSpace(configured) == "" {
		configured = "."
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return workspace{}, errors.New("workspace path cannot be made absolute")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return workspace{}, errors.New("workspace path cannot be resolved")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return workspace{}, errors.New("workspace path cannot be canonicalized")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return workspace{}, errors.New("workspace must be an existing directory")
	}
	return workspace{root: filepath.Clean(canonical)}, nil
}

func (w workspace) resolve(input string) (resolvedPath, *Failure) {
	relative, failure := normalizeRelativePath(input)
	if failure != nil {
		return resolvedPath{}, failure
	}
	candidate := filepath.Join(w.root, filepath.FromSlash(relative))
	if !within(w.root, candidate) {
		return resolvedPath{}, newFailure(FailurePathTraversal)
	}

	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return resolvedPath{}, newFailure(FailurePathNotFound)
		}
		return resolvedPath{}, newFailure(FailureReadFailure)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return resolvedPath{}, newFailure(FailureReadFailure)
	}
	if !within(w.root, canonical) {
		return resolvedPath{}, newFailure(FailureSymlinkEscape)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return resolvedPath{}, newFailure(FailurePathNotFound)
		}
		return resolvedPath{}, newFailure(FailureReadFailure)
	}
	return resolvedPath{relative: filepath.ToSlash(relative), canonical: filepath.Clean(canonical), info: info}, nil
}

func normalizeRelativePath(input string) (string, *Failure) {
	if input == "" || strings.IndexByte(input, 0) >= 0 {
		return "", newFailure(FailureInvalidArguments)
	}
	path := filepath.FromSlash(input)
	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return "", newFailure(FailureAbsolutePath)
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return "", newFailure(FailurePathTraversal)
		}
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", newFailure(FailurePathTraversal)
	}
	return filepath.ToSlash(clean), nil
}

func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
