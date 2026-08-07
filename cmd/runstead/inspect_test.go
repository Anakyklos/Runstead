package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
)

// TestInspectAfterRunProcessExit is the acceptance scenario for
// `runstead inspect <task-id>`: the original run process exits, then inspect
// reconstructs the task from the SQLite database alone.
func TestInspectAfterRunProcessExit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Inspected.","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut bytes.Buffer

	code := run(context.Background(), withStateDirArgs(stateDir, []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}), &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, exitSuccess, errOut.String())
	}

	// The task id is printed to stderr so inspect is usable after the run.
	taskID := extractTaskID(t, errOut.String())
	if taskID == "" {
		t.Fatalf("run did not print a task id:\n%s", errOut.String())
	}

	// The run process is gone; inspect opens the database itself.
	var inspectOut, inspectErr bytes.Buffer
	code = run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &inspectOut, &inspectErr)
	if code != exitSuccess {
		t.Fatalf("inspect exit code = %d, want %d\nstderr:\n%s", code, exitSuccess, inspectErr.String())
	}
	rendered := inspectOut.String()
	for _, want := range []string{
		"Task: " + taskID,
		"Objective: Inspect the workspace.",
		"Status: completed",
		"Outcome: completed",
		"evidence=obs-000001",
		"outcome=success upstream_reached=true",
		"task_finalized",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect output missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "alpha") {
		t.Error("inspect output must not leak repository content")
	}
	if inspectErr.Len() != 0 {
		t.Fatalf("inspect wrote diagnostics: %s", inspectErr.String())
	}
}

// TestInspectUnknownTaskFails clearly and TestInspectUsage covers the CLI
// contract.
func TestInspectUnknownTaskFailsClearly(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"inspect", "missing-task", "--state-dir", t.TempDir()}, &out, &errOut)
	if code != exitNotFound {
		t.Fatalf("inspect exit code = %d, want %d", code, exitNotFound)
	}
	if !strings.Contains(errOut.String(), "not found") {
		t.Fatalf("inspect diagnostic = %q", errOut.String())
	}
}

func TestInspectRequiresExactlyOneTaskID(t *testing.T) {
	cases := [][]string{
		{"inspect"},                        // no task id
		{"inspect", "a", "b"},              // two task ids
		{"inspect", "--state-dir"},         // missing flag value
		{"inspect", "--unknown-flag", "a"}, // unknown flag
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("inspect %v exit code = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestInspectTaskIDMayPrecedeStateDirFlag(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two identical actions with zero repeated-action allowance end the run
	// with repeated_action, mapping the task to a typed failure.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(context.Background(), withStateDirArgs(stateDir, []string{
		"run", "--task", "t", "--workspace", workspace, "--scripted", script,
		"--min-start-interval", "1ms", "--max-repeated-actions", "0", "--log-level", "error",
	}), &out, &errOut)
	if code != agent.OutcomeRepeatedAction.ExitCode() {
		t.Fatalf("run exit code = %d\nstderr:\n%s", code, errOut.String())
	}
	taskID := extractTaskID(t, errOut.String())

	// task id first, then --state-dir: both orders must work.
	var inspectOut, inspectErr bytes.Buffer
	code = run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &inspectOut, &inspectErr)
	if code != exitSuccess {
		t.Fatalf("inspect exit code = %d\nstderr:\n%s", code, inspectErr.String())
	}
	if !strings.Contains(inspectOut.String(), "Status: failed") {
		t.Fatalf("repeated-action stop must map to a failed task status:\n%s", inspectOut.String())
	}
	if !strings.Contains(inspectOut.String(), "Outcome: repeated_action") {
		t.Fatalf("inspect must show the typed outcome:\n%s", inspectOut.String())
	}
}

func extractTaskID(t *testing.T, stderr string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "task: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "task: "))
		}
	}
	return ""
}

func withStateDirArgs(stateDir string, args []string) []string {
	return append(append([]string{}, args...), "--state-dir", stateDir)
}
