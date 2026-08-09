package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Full-runtime write crash tests (issue #10): the real `runstead run`
// composition dies between write intent (TX 1) and the write result (TX 2),
// and the parent reconciles the interrupted write through `runstead resume`.

func writeCrashRunArgs(t *testing.T, scriptPath, workspace, stateDir string) []string {
	t.Helper()
	return []string{
		"run", "--task", "Modify the workspace.",
		"--workspace", workspace,
		"--scripted", scriptPath,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--state-dir", stateDir,
		"--write-policy", "write_file=allow,apply_patch=allow",
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
}

// TestRuntimeWriteCrashHelper is the subprocess entry point for write crash
// windows: it installs both the store crash seam and the write crash seam so
// the process can die at the exact effect boundary.
func TestRuntimeWriteCrashHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_RUNTIME_WRITE_CRASH_HELPER") == "" {
		t.Skip("runtime write crash helper")
	}
	point := os.Getenv("RUNSTEAD_RUNTIME_WRITE_CRASH_POINT")
	state.SetCrashPoint(func(name string) {
		if name == point {
			os.Exit(42)
		}
	})
	tools.SetWriteCrashPoint(func(name string) {
		if name == point {
			os.Exit(42)
		}
	})
	args := strings.Split(os.Getenv("RUNSTEAD_RUNTIME_WRITE_CRASH_ARGS"), "\x1f")
	code := run(context.Background(), args, os.Stdout, os.Stderr)
	os.Exit(code)
}

func runCrashedWriteRun(t *testing.T, args []string, point string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRuntimeWriteCrashHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_RUNTIME_WRITE_CRASH_HELPER=1",
		"RUNSTEAD_RUNTIME_WRITE_CRASH_POINT="+point,
		"RUNSTEAD_RUNTIME_WRITE_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("crash helper failed to run: %v\n%s", err, output)
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	return code, string(output)
}

func TestRuntimeCrashBeforeWriteEffectKeepsFileUnchanged(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedWriteRun(t, writeCrashRunArgs(t, script, workspace, stateDir), "write_before_effect")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The effect never ran: the file is unchanged and the attempt is prepared.
	content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil || string(content) != "old\n" {
		t.Fatalf("file content = %q, want old\\n (err %v)", content, err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("inspect must show the prepared write attempt:\n%s", rendered)
	}
	if !strings.Contains(rendered, "effect_after_hash=") {
		t.Fatalf("inspect must show the persisted effect_after_hash:\n%s", rendered)
	}

	// Resume reconciles the write as never-started and the run completes.
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--write-policy", "write_file=allow,apply_patch=allow",
		"--log-level", "error",
	}, &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "write_effect_not_started") {
		t.Fatalf("inspect must show the not-started reconciliation:\n%s", rendered)
	}
}

func TestRuntimeCrashAfterWriteEffectReconcilesFromFilesystem(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedWriteRun(t, writeCrashRunArgs(t, script, workspace, stateDir), "write_after_effect")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The effect DID run (the crash fired after the rename): the file is the
	// new content, but TX 2 never committed, so the attempt stays prepared
	// with no citable evidence.
	content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil || string(content) != "new\n" {
		t.Fatalf("file content = %q, want new\\n (err %v)", content, err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("inspect must show the prepared write attempt:\n%s", rendered)
	}
	if strings.Contains(rendered, "evidence=obs-000002") {
		t.Fatalf("no citable write evidence may exist before TX 2:\n%s", rendered)
	}

	// Resume reconciles the write as completed from the current filesystem
	// state, persists the observed evidence, and the run completes citing it.
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--write-policy", "write_file=allow,apply_patch=allow",
		"--log-level", "error",
	}, &out, &errOut)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "write_effect_completed") {
		t.Fatalf("inspect must show the completed reconciliation:\n%s", rendered)
	}
	if !strings.Contains(rendered, "evidence=obs-000002") {
		t.Fatalf("reconciled write evidence must be citable:\n%s", rendered)
	}

	// The crash-reconciled evidence must carry the planned diff (bounded,
	// sanitized) alongside the before/after hashes: the TX 1 intent persisted
	// it, and only the current filesystem state matching the expected
	// after-state hash promoted it to reconciled completed evidence.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	defer store.Close()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	var reconciledEvidence tools.WriteEvidence
	found := false
	for _, item := range snapshot.Evidence {
		if item.EvidenceID == "obs-000002" {
			if err := json.Unmarshal([]byte(item.DataJSON), &reconciledEvidence); err != nil {
				t.Fatalf("decode reconciled evidence: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("reconciled evidence obs-000002 not persisted")
	}
	if reconciledEvidence.BeforeHash != before {
		t.Fatalf("reconciled before hash = %q, want %q", reconciledEvidence.BeforeHash, before)
	}
	if reconciledEvidence.AfterHash != tools.HashBytes([]byte("new\n")) {
		t.Fatalf("reconciled after hash = %q", reconciledEvidence.AfterHash)
	}
	if reconciledEvidence.Diff == "" {
		t.Fatal("reconciled write evidence must carry the bounded planned diff")
	}
	if !strings.Contains(reconciledEvidence.Diff, "+new") {
		t.Fatalf("reconciled diff must describe the planned change:\n%s", reconciledEvidence.Diff)
	}
}

func TestRuntimeUnreconcilableWriteStopsWithHumanReview(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedWriteRun(t, writeCrashRunArgs(t, script, workspace, stateDir), "write_after_effect")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// After the crash an unrelated change rewrites the file to a third state:
	// the current state matches neither the precondition nor the expected
	// after-state, so the write is unreconcilable.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("third-party\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	var out, errOut strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--write-policy", "write_file=allow,apply_patch=allow",
		"--log-level", "error",
	}, &out, &errOut)
	if resumeCode != exitHumanReview {
		t.Fatalf("resume exit = %d, want %d (human review)\nstderr:\n%s", resumeCode, exitHumanReview, errOut.String())
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "human_review_required") {
		t.Fatalf("inspect must show the human review requirement:\n%s", rendered)
	}
	if !strings.Contains(rendered, "write_effect_unreconcilable") {
		t.Fatalf("inspect must show the unreconcilable reason:\n%s", rendered)
	}
}
