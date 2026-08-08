package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// executeWriteFile performs one write_file effect. The contract (issue #10):
//
//  1. validate the target path (canonical workspace resolver, fail closed);
//  2. validate the stale-state precondition (expected_before_hash);
//  3. refuse when the current file state differs from the precondition;
//  4. the caller (agent loop) persisted the intent (TX 1) before calling;
//  5. perform the filesystem effect outside any SQLite transaction;
//  6. observe the resulting state and persist the result (TX 2) afterward;
//  7. capture structured evidence (before/after hash, byte count, diff).
func (r *Registry) executeWriteFile(ctx context.Context, observation Observation, path, content, expectedBefore string) Observation {
	resolved, failure := r.workspace.resolveWrite(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	before := AbsentBeforeHash
	if resolved.exists {
		hash, exists, hashErr := hashFile(resolved.canonical)
		if hashErr != nil || !exists {
			observation.Failure = newFailure(FailureReadFailure)
			return observation
		}
		before = hash
	}
	if before != expectedBefore {
		observation.Failure = newFailure(FailureStaleState)
		return observation
	}

	contentBytes := []byte(content)
	after := HashBytes(contentBytes)
	changed := before != after

	// The diff evidence describes the real before -> after transition; the
	// before content is read from the current file (bounded, so the evidence
	// stays bounded even for huge targets). The hashes remain authoritative.
	var beforeContent string
	if resolved.exists {
		beforeContent = string(readBoundedPrefix(resolved.canonical, r.limits.MaxDiffBytes*2))
	}

	// The effect: temp-file plus rename. A canceled context aborts before the
	// rename, so the effect never starts after cancellation.
	hitWriteCrashPoint("write_before_effect")
	if failure := contextFailure(ctx); failure != nil {
		observation.Failure = failure
		return observation
	}
	// TOCTOU boundary: after the initial before-state validation above, an
	// external process could modify the target, create an originally-absent
	// target, or swap a path component to a symlink. The race hook lets tests
	// inject exactly those mutations deterministically; the revalidation below
	// re-checks canonical containment and the before-state against the current
	// filesystem state, and is repeated immediately before the rename.
	hitWriteRaceHook("write_effect_revalidate")
	if failure := r.revalidateWriteTarget(path, expectedBefore, resolved); failure != nil {
		observation.Failure = failure
		return observation
	}
	if changed {
		mode := os.FileMode(0)
		if resolved.exists {
			mode = resolved.mode
		}
		if err := atomicWrite(ctx, resolved.canonical, contentBytes, mode, func() *Failure {
			return r.revalidateWriteTarget(path, expectedBefore, resolved)
		}); err != nil {
			if failure, ok := err.(*Failure); ok {
				observation.Failure = failure
			} else {
				observation.Failure = writeEffectFailure(err)
			}
			return observation
		}
	} else {
		// No-op write: the target already contains exactly the proposed
		// content. Revalidate that the precondition still holds so an external
		// change racing the no-op is reported as stale, never as success; the
		// file is left untouched.
		if failure := r.revalidateWriteTarget(path, expectedBefore, resolved); failure != nil {
			observation.Failure = failure
			return observation
		}
		after = before
	}
	hitWriteCrashPoint("write_after_effect")

	observedAfter, exists, hashErr := hashFile(resolved.canonical)
	if hashErr != nil || !exists {
		observation.Failure = newFailure(FailureWriteFailure)
		return observation
	}
	if changed && observedAfter != after {
		// The observed state contradicts the intended effect: the write may
		// have been partial or an unrelated change raced it. Fail closed.
		observation.Failure = newFailure(FailureWriteFailure)
		return observation
	}
	diff, diffBytes, diffTruncated := "", int64(0), false
	if changed {
		diff, diffBytes, diffTruncated = boundedDiff(replacementDiffText(resolved.relative, beforeContent, string(contentBytes)), r.limits.MaxDiffBytes)
	}
	observation.Success = true
	observation.Data = WriteEvidence{
		Path:          resolved.relative,
		BeforeHash:    before,
		AfterHash:     observedAfter,
		ByteCount:     int64(len(contentBytes)),
		ChangeKind:    changeKindFor(before, observedAfter),
		Outcome:       outcomeFor(before, observedAfter),
		Diff:          diff,
		DiffBytes:     diffBytes,
		DiffTruncated: diffTruncated,
	}
	observation.Metadata = Metadata{
		Source:        ToolWriteFile,
		Untrusted:     true,
		Path:          resolved.relative,
		SizeBytes:     int64(len(contentBytes)),
		BytesOriginal: int64(len(contentBytes)),
		BytesReturned: int64(len(contentBytes)),
		ExitCode:      -1,
	}
	return observation
}

// executeApplyPatch performs one apply_patch effect. The patch is parsed and
// applied in memory first (all-or-nothing), so an invalid or partially
// applicable patch is rejected as a typed failure without touching the file.
func (r *Registry) executeApplyPatch(ctx context.Context, observation Observation, path, patch, expectedBefore string) Observation {
	resolved, failure := r.workspace.resolveWrite(path)
	if failure != nil {
		observation.Failure = failure
		return observation
	}
	if !resolved.exists {
		observation.Failure = newFailure(FailurePathNotFound)
		return observation
	}
	before, exists, hashErr := hashFile(resolved.canonical)
	if hashErr != nil || !exists {
		observation.Failure = newFailure(FailureReadFailure)
		return observation
	}
	if before != expectedBefore {
		observation.Failure = newFailure(FailureStaleState)
		return observation
	}
	if size := fileSize(resolved.canonical); size > int64(r.limits.MaxPatchTargetBytes) {
		observation.Failure = newFailure(FailureWriteTooLarge)
		return observation
	}
	current, readErr := os.ReadFile(resolved.canonical)
	if readErr != nil {
		observation.Failure = newFailure(FailureReadFailure)
		return observation
	}
	patched, applyErr := applyPatch(current, patch, resolved.relative, r.limits.MaxPatchTargetBytes)
	if applyErr != nil {
		// A parse error is an invalid patch; a content mismatch against a
		// verified before-hash is a precondition failure. Both are typed and
		// neither touches the file.
		if errors.Is(applyErr, errPatchContentMismatch) {
			observation.Failure = newFailure(FailureStaleState)
		} else {
			observation.Failure = newFailure(FailureInvalidPatch)
		}
		return observation
	}

	beforeContent := string(current)
	afterContent := string(patched)
	after := HashBytes(patched)
	changed := beforeContent != afterContent

	hitWriteCrashPoint("write_before_effect")
	if failure := contextFailure(ctx); failure != nil {
		observation.Failure = failure
		return observation
	}
	// TOCTOU boundary: revalidate canonical containment and the before-state
	// against the current filesystem state, then again immediately before the
	// rename (see executeWriteFile for the full rationale).
	hitWriteRaceHook("write_effect_revalidate")
	if failure := r.revalidateWriteTarget(path, expectedBefore, resolved); failure != nil {
		observation.Failure = failure
		return observation
	}
	if changed {
		if err := atomicWrite(ctx, resolved.canonical, patched, resolved.mode, func() *Failure {
			return r.revalidateWriteTarget(path, expectedBefore, resolved)
		}); err != nil {
			if failure, ok := err.(*Failure); ok {
				observation.Failure = failure
			} else {
				observation.Failure = writeEffectFailure(err)
			}
			return observation
		}
	} else {
		if failure := r.revalidateWriteTarget(path, expectedBefore, resolved); failure != nil {
			observation.Failure = failure
			return observation
		}
		after = before
	}
	hitWriteCrashPoint("write_after_effect")

	observedAfter, exists, hashErr := hashFile(resolved.canonical)
	if hashErr != nil || !exists {
		observation.Failure = newFailure(FailureWriteFailure)
		return observation
	}
	if changed && observedAfter != after {
		observation.Failure = newFailure(FailureWriteFailure)
		return observation
	}
	diff, diffBytes, diffTruncated := boundedDiff(patch, r.limits.MaxDiffBytes)
	observation.Success = true
	observation.Data = WriteEvidence{
		Path:          resolved.relative,
		BeforeHash:    before,
		AfterHash:     observedAfter,
		ByteCount:     int64(len(patched)),
		ChangeKind:    changeKindFor(before, observedAfter),
		Outcome:       outcomeFor(before, observedAfter),
		Diff:          diff,
		DiffBytes:     diffBytes,
		DiffTruncated: diffTruncated,
	}
	observation.Metadata = Metadata{
		Source:        ToolApplyPatch,
		Untrusted:     true,
		Path:          resolved.relative,
		SizeBytes:     int64(len(patched)),
		BytesOriginal: int64(len(patched)),
		BytesReturned: int64(len(patched)),
		ExitCode:      -1,
	}
	return observation
}

func writeEffectFailure(err error) *Failure {
	if errors.Is(err, os.ErrNotExist) {
		return newFailure(FailurePathNotFound)
	}
	return &Failure{Code: FailureWriteFailure, Message: fmt.Sprintf("%s: %v", failureMessages[FailureWriteFailure], err)}
}

func changeKindFor(before, after string) string {
	switch {
	case before == AbsentBeforeHash:
		return changeCreated
	case before == after:
		return changeUnchanged
	default:
		return changeModified
	}
}

func outcomeFor(before, after string) WriteOutcome {
	if before == after {
		return WriteNoop
	}
	return WriteSuccess
}

// replacementDiffText produces a full-replacement unified diff of one file
// (valid diff form: the whole file as one removal/addition hunk). This is
// structured, deterministic change evidence; it is not a general-purpose diff
// engine.
func replacementDiffText(path, before, after string) string {
	beforeLines := contentLines([]byte(before))
	afterLines := contentLines([]byte(after))
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n+++ %s\n@@ -1,%d +1,%d @@\n", path, path, len(beforeLines), len(afterLines))
	for _, line := range beforeLines {
		builder.WriteString("-")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	for _, line := range afterLines {
		builder.WriteString("+")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return builder.String()
}

// boundedDiff bounds a diff string to limit bytes and reports the original
// byte count and whether truncation occurred.
func boundedDiff(diff string, limit int) (string, int64, bool) {
	if len(diff) <= limit {
		return diff, int64(len(diff)), false
	}
	return diff[:limit], int64(len(diff)), true
}

// readBoundedPrefix reads at most limit bytes of a file. A read failure
// returns an empty prefix (the evidence hashes remain authoritative).
func readBoundedPrefix(path string, limit int) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	if limit <= 0 {
		limit = 64 << 10
	}
	content := make([]byte, 0, limit)
	buffer := make([]byte, 32<<10)
	for len(content) < limit {
		count, readErr := file.Read(buffer)
		if count > 0 {
			take := limit - len(content)
			if take > count {
				take = count
			}
			content = append(content, buffer[:take]...)
		}
		if readErr != nil {
			break
		}
	}
	return content
}
