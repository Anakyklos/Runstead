package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

func (r *Registry) executeSearchText(ctx context.Context, observation Observation, query, path string) Observation {
	resolved, failure := r.workspace.resolve(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	if !resolved.info.IsDir() && !resolved.info.Mode().IsRegular() {
		observation.Failure = newFailure(FailureWrongType)
		return observation
	}
	if r.rgPath != "" {
		return r.searchWithRG(ctx, observation, query, resolved)
	}
	commandContext, cancel := context.WithTimeout(ctx, r.limits.SearchTimeout)
	defer cancel()
	return r.searchWithFallback(commandContext, observation, query, resolved)
}

func (r *Registry) searchWithRG(ctx context.Context, observation Observation, query string, resolved resolvedPath) Observation {
	commandContext, cancel := context.WithTimeout(ctx, r.limits.SearchTimeout)
	defer cancel()
	args := []string{
		"--no-config",
		"--json",
		"--fixed-strings",
		"--line-number",
		"--color=never",
		"--no-heading",
		"-I",
		"--",
		query,
		resolved.relative,
	}
	result := r.runRG(commandContext, r.rgPath, args, r.workspace.root)
	if failure := commandResultContextFailure(ctx, commandContext); failure != nil {
		observation.Failure = failure
		return observation
	}
	stdoutOriginal := result.StdoutBytes
	if stdoutOriginal < int64(len(result.Stdout)) {
		stdoutOriginal = int64(len(result.Stdout))
	}
	stderrOriginal := result.StderrBytes
	if stderrOriginal < int64(len(result.Stderr)) {
		stderrOriginal = int64(len(result.Stderr))
	}
	stdout := result.Stdout
	rawTruncated := stdoutOriginal > int64(r.limits.MaxSearchBytes)
	if len(stdout) > r.limits.MaxSearchBytes {
		stdout = stdout[:r.limits.MaxSearchBytes]
	}
	stderr := boundedBytes(result.Stderr, r.limits.MaxSearchBytes)
	matches, originalMatches, returnedBytes, payloadTruncated, err := parseRGOutput(stdout, r.workspace.root, r.limits.MaxSearchMatches, r.limits.MaxSearchBytes, rawTruncated)
	if err != nil {
		observation.Failure = newFailure(FailureSearchFailure)
		return observation
	}
	if result.ExitCode != 0 && result.ExitCode != 1 {
		observation.Failure = newFailure(FailureSearchFailure)
		return observation
	}
	if result.Err != nil && result.ExitCode != 1 {
		observation.Failure = newFailure(FailureSearchFailure)
		return observation
	}
	if rawTruncated {
		originalMatches = -1
	}
	truncated := rawTruncated || payloadTruncated || (originalMatches >= 0 && originalMatches > r.limits.MaxSearchMatches)
	observation.Success = true
	observation.Truncated = truncated
	observation.Data = SearchData{Matches: matches}
	observation.Metadata = Metadata{
		Source:              ToolSearchText,
		Backend:             "rg",
		Untrusted:           true,
		Path:                resolved.relative,
		BytesOriginal:       stdoutOriginal,
		BytesReturned:       int64(returnedBytes),
		MatchesOriginal:     originalMatches,
		MatchesReturned:     len(matches),
		StdoutBytesOriginal: stdoutOriginal,
		StdoutBytesReturned: int64(len(stdout)),
		StderrBytesOriginal: stderrOriginal,
		StderrBytesReturned: int64(len(stderr)),
		ExitCode:            result.ExitCode,
		Signal:              result.Signal,
	}
	return observation
}

func parseRGOutput(output []byte, root string, maxMatches, maxBytes int, rawTruncated bool) ([]SearchMatch, int, int, bool, error) {
	lines := bytes.Split(output, []byte{'\n'})
	matches := make([]SearchMatch, 0, maxMatches)
	originalMatches := 0
	returnedBytes := 0
	payloadTruncated := false
	for index, line := range lines {
		if len(line) == 0 {
			continue
		}
		if rawTruncated && index == len(lines)-1 {
			break
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, 0, 0, false, err
		}
		if event.Type != "match" {
			continue
		}
		path, err := relativeSearchPath(root, event.Data.Path.Text)
		if err != nil || event.Data.LineNumber < 1 {
			return nil, 0, 0, false, errors.New("invalid rg match")
		}
		text := strings.TrimSuffix(event.Data.Lines.Text, "\n")
		text = strings.TrimSuffix(text, "\r")
		match := SearchMatch{Path: path, Line: event.Data.LineNumber, Text: text}
		originalMatches++
		payloadBytes := len(match.Path) + len(match.Text)
		if len(matches) >= maxMatches || returnedBytes+payloadBytes > maxBytes {
			payloadTruncated = true
			continue
		}
		matches = append(matches, match)
		returnedBytes += payloadBytes
	}
	return matches, originalMatches, returnedBytes, payloadTruncated, nil
}

func relativeSearchPath(root, value string) (string, error) {
	if value == "" {
		return "", errors.New("empty search result path")
	}
	path := filepath.FromSlash(value)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if !within(root, path) {
		return "", errors.New("search result escaped workspace")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func (r *Registry) searchWithFallback(ctx context.Context, observation Observation, query string, resolved resolvedPath) Observation {
	paths, err := searchFiles(ctx, resolved.canonical)
	if err != nil {
		if failure := contextFailure(ctx); failure != nil {
			observation.Failure = failure
		} else {
			observation.Failure = newFailure(FailureSearchFailure)
		}
		return observation
	}
	matches := make([]SearchMatch, 0, r.limits.MaxSearchMatches)
	originalMatches := 0
	originalBytes := 0
	returnedBytes := 0
	skippedBinary := 0
	skippedInvalid := 0
	truncated := false
	for _, path := range paths {
		fileMatches, binary, invalid, scanErr := scanSearchFile(ctx, path, r.workspace.root, query)
		if scanErr != nil {
			if failure := contextFailure(ctx); failure != nil {
				observation.Failure = failure
			} else {
				observation.Failure = newFailure(FailureSearchFailure)
			}
			return observation
		}
		if binary {
			skippedBinary++
			continue
		}
		if invalid {
			skippedInvalid++
			continue
		}
		originalMatches += len(fileMatches)
		for _, match := range fileMatches {
			payloadBytes := len(match.Path) + len(match.Text)
			originalBytes += payloadBytes
			if len(matches) >= r.limits.MaxSearchMatches || returnedBytes+payloadBytes > r.limits.MaxSearchBytes {
				truncated = true
				continue
			}
			matches = append(matches, match)
			returnedBytes += payloadBytes
		}
	}
	if originalMatches > r.limits.MaxSearchMatches {
		truncated = true
	}
	observation.Success = true
	observation.Truncated = truncated
	observation.Data = SearchData{
		Matches:            matches,
		SkippedBinaryFiles: skippedBinary,
		SkippedInvalidUTF8: skippedInvalid,
	}
	observation.Metadata = Metadata{
		Source:          ToolSearchText,
		Backend:         "fallback",
		Untrusted:       true,
		Path:            resolved.relative,
		BytesOriginal:   int64(originalBytes),
		BytesReturned:   int64(returnedBytes),
		MatchesOriginal: originalMatches,
		MatchesReturned: len(matches),
		ExitCode:        -1,
	}
	return observation
}

func searchFiles(ctx context.Context, root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		return []string{root}, nil
	}
	paths := make([]string, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if failure := contextFailure(ctx); failure != nil {
			return errors.New(string(failure.Code))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(paths, func(left, right int) bool {
		return filepath.ToSlash(paths[left]) < filepath.ToSlash(paths[right])
	})
	return paths, nil
}

func scanSearchFile(ctx context.Context, path, root, query string) ([]SearchMatch, bool, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, false, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	matches := make([]SearchMatch, 0)
	lineNumber := 0
	for {
		if failure := contextFailure(ctx); failure != nil {
			return nil, false, false, errors.New(string(failure.Code))
		}
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			if bytes.IndexByte([]byte(line), 0) >= 0 {
				return nil, true, false, nil
			}
			if !utf8.ValidString(line) {
				return nil, false, true, nil
			}
			lineNumber++
			text := strings.TrimSuffix(line, "\n")
			text = strings.TrimSuffix(text, "\r")
			if strings.Contains(text, query) {
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return nil, false, false, err
				}
				matches = append(matches, SearchMatch{Path: filepath.ToSlash(relative), Line: lineNumber, Text: text})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false, false, readErr
		}
	}
	return matches, false, false, nil
}

func commandResultContextFailure(parent, command context.Context) *Failure {
	if failure := contextFailure(parent); failure != nil {
		return failure
	}
	return contextFailure(command)
}
