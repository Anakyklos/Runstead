package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Strict unified-diff patch application (issue #10).
//
// apply_patch accepts a deliberately narrow, deterministic subset of unified
// diff and applies it entirely in memory BEFORE any filesystem mutation, so a
// patch either applies cleanly (all hunks match, all-or-nothing) or is
// rejected as a typed failure without touching the file. No shell `patch`
// command and no arbitrary subprocess is involved.
//
// Accepted format:
//
//	--- <path>
//	+++ <path>
//	@@ -S,C +S,C @@
//	<context | removal | addition lines>
//	...
//
// The two header paths must match the tool's target path (optionally with a/
// or b/ prefixes or quotes). Hunk header counts must match the body. Only
// context (' '), removal ('-') and addition ('+') lines are accepted; no
// "\ No newline" markers, no index/mode/rename headers. A hunk whose content
// does not match the current file fails the whole application.

type patchOpKind byte

const (
	patchContext patchOpKind = ' '
	patchRemove  patchOpKind = '-'
	patchAdd     patchOpKind = '+'
)

type patchOp struct {
	kind patchOpKind
	text string
}

type patchHunk struct {
	oldStart int
	ops      []patchOp
}

type parsedPatch struct {
	oldPath string
	newPath string
	hunks   []patchHunk
}

var hunkHeaderPattern = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@$`)

// parsePatch parses a strict unified diff. It returns a typed parse error for
// any deviation from the accepted subset.
func parsePatch(patch, targetPath string) (parsedPatch, error) {
	var result parsedPatch
	lines := strings.Split(patch, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	index := 0
	for index < len(lines) && strings.TrimSpace(lines[index]) == "" {
		index++
	}
	if index >= len(lines) || !strings.HasPrefix(lines[index], "--- ") {
		return result, fmt.Errorf("missing --- header")
	}
	result.oldPath = normalizePatchHeader(lines[index])
	index++
	if index >= len(lines) || !strings.HasPrefix(lines[index], "+++ ") {
		return result, fmt.Errorf("missing +++ header")
	}
	result.newPath = normalizePatchHeader(lines[index])
	index++
	if !patchPathMatches(result.oldPath, targetPath) || !patchPathMatches(result.newPath, targetPath) {
		return result, fmt.Errorf("patch header paths %q %q do not match target %q", result.oldPath, result.newPath, targetPath)
	}
	for index < len(lines) {
		line := lines[index]
		if strings.TrimSpace(line) == "" {
			index++
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			hunk, next, err := parseHunk(lines, index)
			if err != nil {
				return result, err
			}
			result.hunks = append(result.hunks, hunk)
			index = next
			continue
		}
		return result, fmt.Errorf("unexpected patch line %q", line)
	}
	return result, nil
}

func parseHunk(lines []string, start int) (patchHunk, int, error) {
	var hunk patchHunk
	match := hunkHeaderPattern.FindStringSubmatch(lines[start])
	if match == nil {
		return hunk, 0, fmt.Errorf("invalid hunk header %q", lines[start])
	}
	hunk.oldStart = atoi(match[1])
	oldCount := 1
	if match[2] != "" {
		oldCount = atoi(match[2])
	}
	newCount := 1
	if match[4] != "" {
		newCount = atoi(match[4])
	}
	index := start + 1
	contextCount, removed, added := 0, 0, 0
	for index < len(lines) {
		line := lines[index]
		if line == "" || strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			break
		}
		if len(line) < 1 || (line[0] != ' ' && line[0] != '-' && line[0] != '+') {
			return hunk, 0, fmt.Errorf("invalid hunk line %q", line)
		}
		switch patchOpKind(line[0]) {
		case patchContext:
			hunk.ops = append(hunk.ops, patchOp{kind: patchContext, text: line[1:]})
			contextCount++
		case patchRemove:
			hunk.ops = append(hunk.ops, patchOp{kind: patchRemove, text: line[1:]})
			removed++
		default:
			hunk.ops = append(hunk.ops, patchOp{kind: patchAdd, text: line[1:]})
			added++
		}
		index++
	}
	// The hunk header counts include context lines: old side is
	// context+removals, new side is context+additions.
	if contextCount+removed != oldCount || contextCount+added != newCount {
		return hunk, 0, fmt.Errorf("hunk counts (%d context, %d removed, %d added) do not match header (%d, %d)", contextCount, removed, added, oldCount, newCount)
	}
	return hunk, index, nil
}

// errPatchContentMismatch is returned when a well-formed hunk's content does
// not match the current file. It is distinct from parse errors: the patch was
// structurally valid but its preconditions failed, which is a stale-state
// failure after the before-hash check already passed (the file changed
// between the hash read and the patch application, or the model's patch did
// not describe the file it observed).
var errPatchContentMismatch = errors.New("patch content does not match the current file")

// applyParsedPatch applies the parsed hunks to the original file lines. It is
// all-or-nothing: any hunk whose context or removals do not match the current
// content aborts the whole application.
func applyParsedPatch(old []string, patch parsedPatch) ([]string, error) {
	var result []string
	cursor := 0
	for _, hunk := range patch.hunks {
		start := hunk.oldStart - 1
		if start < cursor || start > len(old) {
			return nil, fmt.Errorf("hunk start %d is out of order", hunk.oldStart)
		}
		result = append(result, old[cursor:start]...)
		pos := start
		for _, op := range hunk.ops {
			switch op.kind {
			case patchContext, patchRemove:
				if pos >= len(old) || old[pos] != op.text {
					return nil, errPatchContentMismatch
				}
				if op.kind == patchContext {
					result = append(result, op.text)
				}
				pos++
			case patchAdd:
				result = append(result, op.text)
			}
		}
		cursor = pos
	}
	result = append(result, old[cursor:]...)
	return result, nil
}

// applyPatch parses and applies a strict unified diff to content. The target
// path bound is the same bound used for reading the patch target.
func applyPatch(content []byte, patch, targetPath string, targetLimit int) ([]byte, error) {
	if len(content) > targetLimit {
		return nil, fmt.Errorf("patch target exceeds the configured bound")
	}
	parsed, err := parsePatch(patch, targetPath)
	if err != nil {
		return nil, err
	}
	original := contentLines(content)
	patched, err := applyParsedPatch(original, parsed)
	if err != nil {
		return nil, err
	}
	return joinLines(patched, endsWithNewline(content)), nil
}

func normalizePatchHeader(line string) string {
	value := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(value, "--- "):
		value = strings.TrimPrefix(value, "--- ")
	case strings.HasPrefix(value, "+++ "):
		value = strings.TrimPrefix(value, "+++ ")
	}
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	value = strings.TrimPrefix(value, "a/")
	value = strings.TrimPrefix(value, "b/")
	return filepath.ToSlash(value)
}

func patchPathMatches(header, target string) bool {
	return header == target
}

func contentLines(content []byte) []string {
	text := string(content)
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func endsWithNewline(content []byte) bool {
	return len(content) > 0 && content[len(content)-1] == '\n'
}

func joinLines(lines []string, trailingNewline bool) []byte {
	joined := strings.Join(lines, "\n")
	if trailingNewline {
		joined += "\n"
	}
	return []byte(joined)
}

func atoi(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
