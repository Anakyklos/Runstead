package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

// Write-tool infrastructure (issue #10).
//
// The write tools extend the read-only registry with the first controlled
// filesystem mutations. They share the canonical workspace resolver and path
// security model of the read tools: writes fail closed for absolute paths,
// traversal, symlink escapes and any path whose containment cannot be proven.
//
// Two ordering invariants are enforced by the callers (the agent loop) and
// verified by the recovery pipeline:
//
//  1. write intent (TX 1) is persisted BEFORE the filesystem effect, and the
//     observed result (TX 2) is persisted AFTER the effect, with no SQLite
//     transaction open while the effect runs;
//  2. every write carries an explicit stale-state precondition
//     (expected_before_hash): the sha256 of the file the model observed, or
//     the literal "absent" when the file must not exist yet. A mismatch
//     refuses the write.
//
// The filesystem effect itself is a temp-file-plus-rename in the target
// directory, so a crash during the effect leaves either the old or the new
// file, never a torn partial write. Recovery (internal/recovery) classifies
// the current state with the persisted expected-after hash (effect_after_hash
// persisted at TX 1) instead of trusting an action fingerprint.

// AbsentBeforeHash is the stale-state precondition marker for a target that
// must not exist yet.
const AbsentBeforeHash = "absent"

// WriteOutcome is the typed result classification of one write effect. It is
// stable structured evidence for the model, the journal and the later
// verifier milestone; generic error strings are never the main API.
type WriteOutcome string

const (
	WriteSuccess           WriteOutcome = "success"
	WriteNoop              WriteOutcome = "noop"
	WriteDenied            WriteOutcome = "denied"
	WriteInvalidArguments  WriteOutcome = "invalid_arguments"
	WritePathViolation     WriteOutcome = "path_violation"
	WriteStale             WriteOutcome = "stale"
	WriteInvalidPatch      WriteOutcome = "invalid_patch"
	WriteFailed            WriteOutcome = "failed"
	WriteUncertain         WriteOutcome = "uncertain"
	WriteReconciliationReq WriteOutcome = "reconciliation_required"
	WriteHumanReviewReq    WriteOutcome = "human_review_required"
)

// WriteEvidence is the structured, bounded evidence of one write effect. It
// carries enough information to verify the real effect: normalized path,
// before/after hashes, byte count, change classification, bounded diff and
// the action/execution/evidence identities when known.
type WriteEvidence struct {
	// Path is the normalized slash-separated path relative to the workspace.
	Path string `json:"path"`
	// BeforeHash is the sha256 of the file before the effect, or "absent" for
	// a new file.
	BeforeHash string `json:"before_hash"`
	// AfterHash is the sha256 of the file after the effect.
	AfterHash string `json:"after_hash"`
	// ByteCount is the resulting file size in bytes.
	ByteCount int64 `json:"byte_count"`
	// ChangeKind is "created", "modified" or "unchanged".
	ChangeKind string `json:"change_kind"`
	// Outcome is the typed write outcome (success, noop, ...).
	Outcome WriteOutcome `json:"outcome"`
	// Diff is a bounded structured change description (full-replacement
	// unified diff for write_file, the applied patch for apply_patch).
	Diff string `json:"diff,omitempty"`
	// DiffTruncated reports that Diff was truncated to the evidence bound.
	DiffTruncated bool `json:"diff_truncated,omitempty"`
	// DiffBytes is the number of diff bytes available before bounding.
	DiffBytes int64 `json:"diff_bytes,omitempty"`
	// ActionID, ExecutionID and EvidenceID are filled by the loop after the
	// execution identity is known; the registry does not choose them.
	ActionID    string `json:"action_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	EvidenceID  string `json:"evidence_id,omitempty"`
}

const (
	changeCreated   = "created"
	changeModified  = "modified"
	changeUnchanged = "unchanged"
)

// writePath is the validated target of one write. The canonical path is
// guaranteed to live inside the workspace root; symlink final components are
// refused (writes never follow or replace symlinks).
type writePath struct {
	relative  string
	canonical string
	exists    bool
	mode      os.FileMode
}

// resolveWrite resolves a write target with the same canonical security model
// as reads (normalizeRelativePath + directory boundary + EvalSymlinks), and
// additionally supports targets that do not exist yet. Failures are typed:
// absolute path, traversal, symlink escape, wrong type, missing parent.
func (w workspace) resolveWrite(input string) (writePath, *Failure) {
	relative, failure := normalizeRelativePath(input)
	if failure != nil {
		return writePath{}, failure
	}
	candidate := filepath.Join(w.root, filepath.FromSlash(relative))
	if !within(w.root, candidate) {
		return writePath{}, newFailure(FailurePathTraversal)
	}
	parent := filepath.Dir(candidate)
	parentCanonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return writePath{}, newFailure(FailurePathNotFound)
		}
		return writePath{}, newFailure(FailureReadFailure)
	}
	parentCanonical, err = filepath.Abs(parentCanonical)
	if err != nil {
		return writePath{}, newFailure(FailureReadFailure)
	}
	if !within(w.root, parentCanonical) {
		return writePath{}, newFailure(FailureSymlinkEscape)
	}
	target := filepath.Join(parentCanonical, filepath.Base(candidate))
	info, statErr := os.Lstat(target)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return writePath{}, newFailure(FailureReadFailure)
	}
	exists := statErr == nil
	var mode os.FileMode
	if exists {
		if info.Mode()&os.ModeSymlink != 0 {
			// Never write through or replace a symlink, even one that points
			// inside the workspace: the effect of a write on a symlink target
			// is ambiguous and stale-state protection cannot verify it.
			return writePath{}, newFailure(FailureSymlinkEscape)
		}
		if !info.Mode().IsRegular() {
			return writePath{}, newFailure(FailureWrongType)
		}
		mode = info.Mode()
	}
	return writePath{
		relative:  filepath.ToSlash(relative),
		canonical: filepath.Clean(target),
		exists:    exists,
		mode:      mode,
	}, nil
}

// HashBytes returns the lowercase hex sha256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashFile returns the sha256 of the complete file content at path. The
// second result reports whether the file exists.
func hashFile(path string) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", true, err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", true, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

// fileSize returns the size of the file at path, or -1 when it cannot be
// determined.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

// atomicWrite writes content to path through a temp file plus rename in the
// same directory. The effect is all-or-nothing at the filesystem level: a
// crash mid-write leaves either the old file or the new file. Existing file
// permissions are preserved; new files get 0644. The context is checked
// before the rename so a canceled run never performs the effect.
func atomicWrite(ctx context.Context, path string, content []byte, existingMode os.FileMode) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if existingMode != 0 {
		mode = existingMode.Perm()
	}
	tmp, err := os.CreateTemp(dir, ".runstead-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	// Sync the content before the rename so a completed effect is durable
	// (fsync boundary of the effect, not of any SQLite transaction).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if failure := contextFailure(ctx); failure != nil {
		return errors.New(string(failure.Code))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// writeIntentArguments is the decoded persisted argument set of one write
// attempt.
type writeIntentArguments struct {
	Path               string
	Content            string
	Patch              string
	ExpectedBeforeHash string
}

// WriteIntent is the persisted intent of one interrupted write attempt used
// by the recovery pipeline. Arguments is the persisted sanitized arguments
// JSON document; ExpectedAfterHash is the deterministic effect expectation
// persisted at TX 1 (never derived from redacted persisted content).
type WriteIntent struct {
	Tool string
	// Arguments is the persisted arguments JSON document of the attempt.
	Arguments []byte
	// ExpectedAfterHash is the deterministic after-state hash computed at
	// intent time from the real (unredacted) arguments.
	ExpectedAfterHash string
}

// ReconcileStatus is the typed result of reconciling one interrupted write
// attempt against the current filesystem state.
type ReconcileStatus string

const (
	// ReconcileNotStarted means the current file state still matches the
	// recorded precondition, so the effect provably never ran.
	ReconcileNotStarted ReconcileStatus = "effect_not_started"
	// ReconcileCompleted means the current file state matches the recorded
	// expected after-state, so the effect completed (or a no-op write ran).
	ReconcileCompleted ReconcileStatus = "effect_completed"
	// ReconcileHumanReview means the current state matches neither the
	// precondition nor the expected after-state: the outcome cannot be
	// determined safely and a human must decide.
	ReconcileHumanReview ReconcileStatus = "human_review_required"
)

// ReconcileResult is the outcome of write reconciliation.
type ReconcileResult struct {
	Status   ReconcileStatus
	Evidence WriteEvidence
	Failure  *Failure
}

// ReconcileWrite reconciles one interrupted write attempt (ADR recovery
// class 2) against the current filesystem state. It never performs the
// effect and never repeats it; it classifies observable state only.
func ReconcileWrite(ctx context.Context, workspaceRoot string, intent WriteIntent) ReconcileResult {
	workspace, err := newWorkspace(workspaceRoot)
	if err != nil {
		return ReconcileResult{Status: ReconcileHumanReview, Failure: newFailure(FailureReadFailure)}
	}
	decoded, failure := decodeWriteIntent(intent.Tool, intent.Arguments)
	if failure != nil {
		// A persisted intent that cannot be decoded cannot be reconciled
		// safely: the effect may have started.
		return ReconcileResult{Status: ReconcileHumanReview, Failure: failure}
	}
	resolved, failure := workspace.resolveWrite(decoded.Path)
	if failure != nil {
		return ReconcileResult{Status: ReconcileHumanReview, Failure: failure}
	}
	before := AbsentBeforeHash
	size := int64(-1)
	if resolved.exists {
		hash, exists, hashErr := hashFile(resolved.canonical)
		if hashErr != nil || !exists {
			return ReconcileResult{Status: ReconcileHumanReview, Failure: newFailure(FailureReadFailure)}
		}
		before = hash
		size = fileSize(resolved.canonical)
	}
	if before == decoded.ExpectedBeforeHash {
		// The file is exactly in the state the model observed at intent time:
		// the effect never started (or a no-op write ran, which is
		// indistinguishable and safe to reconsider).
		return ReconcileResult{Status: ReconcileNotStarted}
	}
	if intent.ExpectedAfterHash != "" && before == intent.ExpectedAfterHash {
		kind := changeModified
		if decoded.ExpectedBeforeHash == AbsentBeforeHash {
			kind = changeCreated
		}
		return ReconcileResult{
			Status: ReconcileCompleted,
			Evidence: WriteEvidence{
				Path:       resolved.relative,
				BeforeHash: decoded.ExpectedBeforeHash,
				AfterHash:  before,
				ByteCount:  size,
				ChangeKind: kind,
				Outcome:    WriteSuccess,
			},
		}
	}
	// The current state matches neither the precondition nor the expected
	// after-state: the write may have been partial or an unrelated change
	// happened. Never classify this as success or retry it automatically.
	return ReconcileResult{Status: ReconcileHumanReview}
}

// decodeWriteIntent reads the persisted sanitized arguments JSON of a write
// attempt. It deliberately does not trust redacted persisted content for
// effect planning: the expected-after hash is carried separately.
func decodeWriteIntent(tool string, arguments []byte) (writeIntentArguments, *Failure) {
	var decoded writeIntentArguments
	var raw protocol.Arguments
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return decoded, newFailure(FailureInvalidArguments)
	}
	path, failure := stringArgument(raw, "path")
	if failure != nil {
		return decoded, failure
	}
	expected, failure := stringArgument(raw, "expected_before_hash")
	if failure != nil {
		return decoded, failure
	}
	decoded.Path = path
	decoded.ExpectedBeforeHash = expected
	switch tool {
	case ToolWriteFile:
		content, contentFailure := stringArgumentAllowEmpty(raw, "content")
		if contentFailure != nil {
			return decoded, contentFailure
		}
		decoded.Content = content
	case ToolApplyPatch:
		patch, patchFailure := stringArgumentAllowEmpty(raw, "patch")
		if patchFailure != nil || strings.TrimSpace(patch) == "" {
			return decoded, newFailure(FailureInvalidArguments)
		}
		decoded.Patch = patch
	default:
		return decoded, newFailure(FailureInvalidArguments)
	}
	return decoded, nil
}

// validBeforeHash reports whether value is a legal stale-state precondition:
// the literal "absent" or a 64-character lowercase hex sha256.
func validBeforeHash(value string) bool {
	if value == AbsentBeforeHash {
		return true
	}
	if len(value) != 64 {
		return false
	}
	for _, digit := range value {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}

// stringArgumentAllowEmpty reads a string argument that may legitimately be
// empty (file content). It still rejects absent keys and non-string values.
func stringArgumentAllowEmpty(arguments protocol.Arguments, name string) (string, *Failure) {
	raw, ok := arguments[name]
	if !ok {
		return "", newFailure(FailureInvalidArguments)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", newFailure(FailureInvalidArguments)
	}
	return value, nil
}

// PlanWriteEffect computes the deterministic expected after-state hash of a
// proposed write WITHOUT performing any filesystem mutation. It returns ""
// when the effect cannot be planned (the authoritative typed failure comes
// from Execute), and "" for read-only tools.
func (r *Registry) PlanWriteEffect(action protocol.Action) string {
	switch action.Tool {
	case ToolWriteFile:
		content, failure := stringArgumentAllowEmpty(action.Arguments, "content")
		if failure != nil {
			return ""
		}
		return HashBytes([]byte(content))
	case ToolApplyPatch:
		path, failure := stringArgument(action.Arguments, "path")
		if failure != nil {
			return ""
		}
		patch, patchFailure := stringArgumentAllowEmpty(action.Arguments, "patch")
		if patchFailure != nil || strings.TrimSpace(patch) == "" {
			return ""
		}
		resolved, writeFailure := r.workspace.resolveWrite(path)
		if writeFailure != nil || !resolved.exists {
			return ""
		}
		current, readErr := os.ReadFile(resolved.canonical)
		if readErr != nil {
			return ""
		}
		patched, applyErr := applyPatch(current, patch, resolved.relative, r.limits.MaxPatchTargetBytes)
		if applyErr != nil {
			return ""
		}
		return HashBytes(patched)
	}
	return ""
}

// AnnotateWriteEvidence fills the execution identities of a successful write
// observation. The loop calls it after the execution id is allocated and
// before TX 2, so the persisted evidence carries action/execution/evidence ids.
func (r *Registry) AnnotateWriteEvidence(observation *Observation, actionID, executionID string) {
	if observation == nil || !observation.Success {
		return
	}
	evidence, ok := observation.Data.(WriteEvidence)
	if !ok {
		return
	}
	evidence.ActionID = actionID
	evidence.ExecutionID = executionID
	evidence.EvidenceID = observation.ID
	observation.Data = evidence
}

// IsWriteTool reports whether the tool is a policy-gated write tool.
func (r *Registry) IsWriteTool(tool string) bool {
	return tool == ToolWriteFile || tool == ToolApplyPatch
}

// writeCrashPoint is the deterministic test seam at write-effect boundaries.
// Production code leaves it nil; subprocess crash tests install it to die
// between TX 1 and the effect ("write_before_effect") or between the effect
// and TX 2 ("write_after_effect").
var writeCrashPoint func(string)

// SetWriteCrashPoint installs the write crash test seam. Only tests call it.
func SetWriteCrashPoint(fn func(string)) { writeCrashPoint = fn }

func hitWriteCrashPoint(name string) {
	if writeCrashPoint != nil {
		writeCrashPoint(name)
	}
}
