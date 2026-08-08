package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

func runDecide(t *testing.T, stateDir, taskID, actionID, decision, reason string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	args := []string{"decide", taskID, actionID, decision, "--state-dir", stateDir}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	code := run(context.Background(), args, &out, &errOut)
	if errOut.Len() > 0 {
		return code, errOut.String()
	}
	return code, out.String()
}

func TestDecideCommandRecordsApprovalAndInspectRendersIt(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: "task-decide", Objective: "o", Workspace: "/ws"}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-decide"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if _, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: "task-decide", Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-decide",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	store.Close()

	code, output := runDecide(t, stateDir, "task-decide", "action-000001", "approved", "operator reviewed the diff")
	if code != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", code, output)
	}
	if !strings.Contains(output, "decision=approved") {
		t.Fatalf("decide output = %q", output)
	}

	// inspect renders the approval record.
	var inspectOut, inspectErr bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "task-decide", "--state-dir", stateDir}, &inspectOut, &inspectErr); code != exitSuccess {
		t.Fatalf("inspect exit = %d\n%s", code, inspectErr.String())
	}
	rendered := inspectOut.String()
	if !strings.Contains(rendered, "Approvals:") || !strings.Contains(rendered, "decision=approved") || !strings.Contains(rendered, "action=action-000001") {
		t.Fatalf("inspect must render the approval:\n%s", rendered)
	}
}

func TestDecideCommandRejectsBadUsage(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), state.TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "/ws"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	cases := [][]string{
		{},                               // missing args
		{"task-1"},                       // missing action/decision
		{"task-1", "action-1"},           // missing decision
		{"task-1", "action-1", "maybe"},  // invalid decision
		{"nope", "action-1", "approved"}, // missing task
		{"task-1", "action-1", "approved", "extra"}, // too many positionals
	}
	for _, args := range cases {
		var out, errOut strings.Builder
		full := append([]string{"decide"}, args...)
		code := run(context.Background(), full, &out, &errOut)
		if code == exitSuccess {
			t.Fatalf("decide %v must fail", args)
		}
	}
}

// TestRunDecideApprovalFlowEndToEnd proves the complete issue #10 approval
// flow: an approval-required write does not execute during the run, the
// operator approves it with `runstead decide`, and a resumed run executes it.
func TestRunDecideApprovalFlowEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"created\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001","obs-000002"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Create a file with approval.",
		"--workspace", workspace,
		"--scripted", script,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
		// Default policy: approval_required for every write tool.
	}
	// The run crashes right after persisting the approval_required decision:
	// the task stays running, the write action stays planned and resumable.
	code, output := runCrashedWriteRun(t, args, "write_policy_decision_after")
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The write must NOT have executed without approval.
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("write must not execute without approval: %v", err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=approval_required") {
		t.Fatalf("inspect must show the approval_required decision:\n%s", rendered)
	}

	// The operator approves the write action. Provider attempts share the
	// store identity sequence, so in this scripted task the write proposal is
	// action-000005 (exec-000001 provider, action-000002 read, exec-000003
	// read execution, exec-000004 provider, action-000005 write).
	decideCode, decideOut := runDecide(t, stateDir, taskID, "action-000005", "approved", "operator reviewed")
	if decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}
	afterApprove := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(afterApprove, "decision=approved") || !strings.Contains(afterApprove, "Approvals:") {
		t.Fatalf("inspect must show the approval after decide:\n%s", afterApprove)
	}
	// Debug: dump the persisted approval fingerprint and the write action's
	// fingerprint from the crashed run.
	{
		store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range snapshot.Actions {
			t.Logf("action %s tool=%s fingerprint=%s", action.ActionID, action.Tool, action.Fingerprint)
		}
	}

	// A resumed run with the approval re-proposes the write (same fingerprint,
	// new action id) and executes it.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"created\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"created","evidence":["obs-000001","obs-000002"]}</runstead_final>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, resumeErr.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
	if err != nil || string(content) != "created\n" {
		t.Fatalf("approved write must execute on resume; content = %q err = %v", content, err)
	}
}
