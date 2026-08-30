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

// Workspace returns the canonical root the registry is bound to.
func (r *Registry) Workspace() string {
	if r == nil {
		return ""
	}
	return r.workspace.root
}

// ResolveWorkspacePath resolves a relative path with the same canonical
// security model as every other tool: relative only, no traversal, no symlink
// escapes. It is the control-plane observer seam for the verifier (issue
// #11): verification reuses this resolver instead of implementing a second
// containment logic. The returned relative path is normalized slash form and
// the canonical path is the absolute resolved path inside the workspace.
func (r *Registry) ResolveWorkspacePath(input string) (relative, canonical string, failure *Failure) {

	if r == nil {
		return "", "", newFailure(FailureInvalidArguments)
	}
	resolved, failure := r.workspace.resolve(input)
	if failure != nil {
		return "", "", failure
	}
	return resolved.relative, resolved.canonical, nil
}

// FileSHA256 returns the sha256 of the COMPLETE file at the relative path,
// using the canonical resolver. The second result reports whether the file
// exists. It is the control-plane observer seam for the verifier (issue #11);
// the verifier never opens paths itself.
func (r *Registry) FileSHA256(input string) (hash string, present bool, failure error) {

	if r == nil {
		return "", false, newFailure(FailureInvalidArguments)
	}
	// The resolver returns a typed *Failure; keep it typed locally so a nil
	// failure never becomes a non-nil error interface (typed-nil trap).
	_, canonical, resolveFailure := r.ResolveWorkspacePath(input)

	if resolveFailure != nil {
		// The resolver only reports path_not_found after proving the path is
		// inside the workspace; for verification, absent is a valid
		// observation, not a failure.
		if resolveFailure.Code == FailurePathNotFound {
			return "", false, nil
		}
		return "", false, resolveFailure
	}
	hash, present, err := hashFile(canonical)

	if err != nil {
		return "", present, newFailure(FailureReadFailure)
	}
	return hash, present, nil
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

// subRoot returns the workspace view rooted at a workspace-relative scope
// path (Work Unit WorkspaceScope). The scope must stay inside the parent
// root. The sub-root may not exist yet (a unit scope can be created by a
// permitted write inside it); containment is enforced lexically so no
// evaluation or symlink resolution of the not-yet-existing root happens.
func (w workspace) subRoot(scope string) (workspace, *Failure) {
	normalized, failure := normalizeRelativePath(scope)
	if failure != nil {
		return workspace{}, failure
	}
	candidate := filepath.Join(w.root, filepath.FromSlash(normalized))
	if !within(w.root, candidate) {
		return workspace{}, newFailure(FailurePathTraversal)
	}
	return workspace{root: filepath.Clean(candidate)}, nil
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
	return NormalizeWorkspacePath(input)
}

// NormalizeWorkspacePath is the canonical workspace-relative path check used
// by every tool and by Work Unit workspace-scope validation (issue #106):
// relative only (no absolute paths, no volume names), no ".." traversal, no
// embedded NUL. The returned value is the slash-form cleaned path. This is
// the SINGLE coordinate system for workspace scopes.
func NormalizeWorkspacePath(input string) (string, *Failure) {
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
