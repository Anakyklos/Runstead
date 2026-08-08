package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// seedRunningTaskWithConfig creates a durable resumable task (status running)
// with the given configuration snapshot, mirroring a task that `run` created
// and left resumable.
func seedRunningTaskWithConfig(t *testing.T, stateDir, taskID, workspace, configJSON string) {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: taskID, Objective: "o", Workspace: workspace, Model: "scripted", ConfigJSON: []byte(configJSON)}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
}

// seedCompletedRead seeds a completed read_file attempt with citable evidence
// obs-000001, as a prior run would have left it for the recovery pipeline.
func seedCompletedRead(t *testing.T, store *state.Store, taskID, path string) {
	t.Helper()
	ctx := context.Background()
	actionID, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: taskID, Tool: "read_file", Arguments: []byte(`{"path":"` + path + `"}`), Fingerprint: "fp-read-" + path,
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: taskID, ActionID: actionID, Tool: "read_file",
		Arguments: []byte(`{"path":"` + path + `"}`), RecoveryClass: 1,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	observation := tools.Observation{
		ID: "obs-000001", Tool: "read_file", Success: true,
		Data:     map[string]any{"path": path, "content": "info\n"},
		Metadata: tools.Metadata{Source: "read_file", Untrusted: true, Path: path, ExitCode: 0},
	}
	if err := store.CompleteToolAttempt(ctx, state.ToolAttemptCompleted{
		TaskID: taskID, ExecutionID: executionID, Status: "completed",
		EvidenceID: observation.ID, DurationNanos: 1000, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
}

func TestResolveResumeWritePolicyUsesPersistedByDefaultAndRejectsDivergence(t *testing.T) {
	denySpec := `{"write_policy":"apply_patch=deny,write_file=deny","max_steps":24}`
	persisted, err := writePolicyFromConfig(denySpec)
	if err != nil {
		t.Fatalf("writePolicyFromConfig() error = %v", err)
	}
	if string(persisted.Modes["write_file"]) != "deny" || string(persisted.Modes["apply_patch"]) != "deny" {
		t.Fatalf("persisted policy = %+v, want deny/deny", persisted.Modes)
	}

	// No override: the persisted policy is used.
	config, err := resolveResumeWritePolicy(denySpec, "", false)
	if err != nil {
		t.Fatalf("resolveResumeWritePolicy(no flag) error = %v", err)
	}
	if config.Spec() != persisted.Spec() {
		t.Fatalf("spec = %q, want %q", config.Spec(), persisted.Spec())
	}

	// A divergent override is rejected fail-closed (never silently widened).
	if _, err := resolveResumeWritePolicy(denySpec, "write_file=allow,apply_patch=deny", true); err == nil {
		t.Fatal("a divergent --write-policy must be rejected")
	}
	// A matching override is accepted.
	if _, err := resolveResumeWritePolicy(denySpec, "write_file=deny,apply_patch=deny", true); err != nil {
		t.Fatalf("a matching override must be accepted: %v", err)
	}
	// A legacy task without a persisted policy falls back to the fail-closed
	// default.
	config, err = resolveResumeWritePolicy("{}", "", false)
	if err != nil {
		t.Fatalf("resolveResumeWritePolicy(empty) error = %v", err)
	}
	if string(config.Modes["write_file"]) != "approval_required" || string(config.Modes["apply_patch"]) != "approval_required" {
		t.Fatalf("legacy fallback = %+v, want approval_required defaults", config.Modes)
	}
}

func TestResumeRejectsDivergentWritePolicyOverride(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	seedRunningTaskWithConfig(t, stateDir, "task-policy", workspace, `{"write_policy":"apply_patch=deny,write_file=deny","max_steps":24}`)

	// A more permissive override must fail closed before any recovery or
	// execution side effect.
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"resume", "task-policy",
		"--state-dir", stateDir,
		"--write-policy", "write_file=allow,apply_patch=deny",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("resume exit = %d, want %d (divergent policy)\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "diverges from the task's persisted policy") {
		t.Fatalf("resume diagnostic = %q, want a divergence diagnostic", errOut.String())
	}
	// The task must still be resumable: nothing was finalized or corrupted.
	rendered := inspectRendered(t, stateDir, "task-policy")
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("task must remain running after the rejected resume:\n%s", rendered)
	}
}

// TestResumeUsesPersistedDenyPolicy proves that a deny write policy survives
// restart: resume with no override continues under the persisted deny policy,
// so the re-proposed write is still denied and never executes.
func TestResumeUsesPersistedDenyPolicy(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a resumable task under the deny policy, as `run` would have
	// persisted it, with a completed read observation as citable evidence.
	stateDir := t.TempDir()
	seedRunningTaskWithConfig(t, stateDir, "task-deny", workspace, `{"write_policy":"apply_patch=deny,write_file=deny","max_steps":24}`)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedRead(t, store, "task-deny", "readme.txt")
	store.Close()

	// Resume with no write-policy override: the persisted deny policy is used,
	// the write proposal is denied, and the final grounds on the read evidence.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		"task-deny", "--state-dir", stateDir, "--scripted", resumeScript, "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, resumeErr)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("deny policy must survive resume; write executed: %v", err)
	}
	rendered := inspectRendered(t, stateDir, "task-deny")
	if !strings.Contains(rendered, "decision=denied") {
		t.Fatalf("the re-proposed write must be denied under the persisted policy:\n%s", rendered)
	}
	if !strings.Contains(rendered, "write_policy: apply_patch=deny,write_file=deny") {
		t.Fatalf("inspect must render the persisted write policy sanitized:\n%s", rendered)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume must complete on the grounded read evidence:\n%s", resumeOut)
	}
}
