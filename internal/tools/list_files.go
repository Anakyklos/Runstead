package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

func (r *Registry) executeListFiles(ctx context.Context, observation Observation, path string) Observation {
	relative, failure := normalizeRelativePath(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	pathValue := filepath.FromSlash(relative)
	var parent resolvedPath
	parent, failure = r.workspace.resolve(filepath.ToSlash(filepath.Dir(pathValue)))
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	if !parent.info.IsDir() {
		observation.Failure = newFailure(FailureWrongType)
		return observation
	}
	// Preserve the final path component type after validating its canonical parent.
	info, err := os.Lstat(filepath.Join(parent.canonical, filepath.Base(pathValue)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			observation.Failure = newFailure(FailurePathNotFound)
			return observation
		}
		observation.Failure = newFailure(FailureReadFailure)
		return observation
	}
	finalSymlink := info.Mode()&os.ModeSymlink != 0

	resolved, failure := r.workspace.resolve(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	if finalSymlink {
		observation.Failure = newFailure(FailureWrongType)
		return observation
	}
	if !resolved.info.IsDir() {
		observation.Failure = newFailure(FailureWrongType)
		return observation
	}
	entries, err := os.ReadDir(resolved.canonical)
	if err != nil {
		observation.Failure = newFailure(FailureListFailure)
		return observation
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if failure := contextFailure(ctx); failure != nil {
			observation.Failure = failure
			return observation
		}
		info, err := os.Lstat(filepath.Join(resolved.canonical, entry.Name()))
		if err != nil {
			observation.Failure = newFailure(FailureListFailure)
			return observation
		}
		typeValue := EntryOther
		size := int64(0)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			typeValue = EntrySymlink
		case info.IsDir():
			typeValue = EntryDirectory
		case info.Mode().IsRegular():
			typeValue = EntryFile
			size = info.Size()
		}
		result = append(result, FileEntry{
			Path: filepath.ToSlash(filepath.Join(resolved.relative, entry.Name())),
			Type: typeValue,
			Size: size,
		})
	}
	original := len(result)
	if original > r.limits.MaxListEntries {
		result = result[:r.limits.MaxListEntries]
	}
	observation.Success = true
	observation.Truncated = len(result) < original
	observation.Data = ListData{Path: resolved.relative, Entries: result}
	observation.Metadata = Metadata{
		Source:          ToolListFiles,
		Untrusted:       true,
		Path:            resolved.relative,
		EntriesOriginal: original,
		EntriesReturned: len(result),
		ExitCode:        -1,
	}
	return observation
}
