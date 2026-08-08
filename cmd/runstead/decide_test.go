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

// seedPendingWrite creates a task, records a pending write action with an
// approval_required policy decision, and marks the task paused for approval.
func seedPendingWrite(t *testing.T, store *state.Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: taskID, Objective: "o", Workspace: "/ws"}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if _, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: taskID, Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-decide",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordWritePolicyDecision(ctx, state.WritePolicyDecision{
		TaskID: taskID, ActionID: "action-000001", Tool: "write_file",
		Decision: "approval_required", Reason: "approval_required",
	}); err != nil {
		t.Fatalf("RecordWritePolicyDecision() error = %v", err)
	}
	if err := store.MarkTaskApprovalRequired(ctx, taskID, "action-000001", "waiting"); err != nil {
		t.Fatalf("MarkTaskApprovalRequired() error = %v", err)
	}
}

func TestDecideCommandRecordsApprovalAndInspectRendersIt(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	seedPendingWrite(t, store, "task-decide")
	store.Close()

	code, output := runDecide(t, stateDir, "task-decide", "action-000001", "approved", "operator reviewed the diff")
	if code != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", code, output)
	}
	if !strings.Contains(output, "decision=approved") {
		t.Fatalf("decide output = %q", output)
	}

	// inspect renders the approval record and no longer lists the action as
	// pending.
	var inspectOut, inspectErr bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "task-decide", "--state-dir", stateDir}, &inspectOut, &inspectErr); code != exitSuccess {
		t.Fatalf("inspect exit = %d\n%s", code, inspectErr.String())
	}
	rendered := inspectOut.String()
	if !strings.Contains(rendered, "Approvals:") || !strings.Contains(rendered, "decision=approved") || !strings.Contains(rendered, "action=action-000001") {
		t.Fatalf("inspect must render the approval:\n%s", rendered)
	}
	if strings.Contains(rendered, "action=action-000001 tool=write_file awaiting operator decision") {
		t.Fatalf("an approved action must not remain pending:\n%s", rendered)
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

func TestDecideCommandRejectsNonPendingActions(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	seedPendingWrite(t, store, "task-1")
	// A read action that is not part of the approval flow.
	if _, err := store.RecordAction(context.Background(), state.ActionRecord{
		TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`), Fingerprint: "fp-read",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	store.Close()

	for _, actionID := range []string{"action-000002", "does-not-exist"} {
		code, output := runDecide(t, stateDir, "task-1", actionID, "approved", "x")
		if code == exitSuccess {
			t.Fatalf("decide for non-pending action %q must fail", actionID)
		}
		if !strings.Contains(output, "not pending approval") {
			t.Fatalf("decide error = %q, want a not-pending diagnostic", output)
		}
	}
	// The pending action itself is accepted.
	if code, _ := runDecide(t, stateDir, "task-1", "action-000001", "approved", "ok"); code != exitSuccess {
		t.Fatal("decide for the pending write action must succeed")
	}
}

// TestRunDecideApprovalFlowEndToEnd proves the complete issue #10 review
// approval UX WITHOUT any artificial crash: run pauses with approval_required,
// the process returns normally, the operator approves the pending action with
// `runstead decide`, a normal `runstead resume` executes the approved write
// and completes.
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

	// Run 1 pauses normally with approval_required; the process returns its
	// typed exit code (no crash involved).
	var out, errOut strings.Builder
	code := run(context.Background(), args, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete\nstdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "outcome: approval_required") {
		t.Fatalf("run output must show approval_required:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	pendingAction := pendingActionFromOutput(t, out.String())
	// The write must NOT have executed without approval.
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("write must not execute without approval: %v", err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "Pending approvals:") || !strings.Contains(rendered, "action="+pendingAction+" tool=write_file awaiting operator decision") {
		t.Fatalf("inspect must show the pending write action:\n%s", rendered)
	}

	// The operator approves the pending action reported by the CLI.
	decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "approved", "operator reviewed")
	if decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}

	// A normal resume re-proposes the approved write and completes.
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
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", resumeOut.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
	if err != nil || string(content) != "created\n" {
		t.Fatalf("approved write must execute on resume; content = %q err = %v", content, err)
	}
}

// TestRunDecideRejectedFlowEndToEnd proves that after `runstead decide
// rejected`, a normal resume preserves the rejection and the write never
// executes; the task can still complete on other evidence.
func TestRunDecideRejectedFlowEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Create a file with approval.",
		"--workspace", workspace,
		"--scripted", script,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	var out, errOut strings.Builder
	code := run(context.Background(), args, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete")
	}
	taskID := taskIDFromOutput(t, errOut.String())
	pendingAction := pendingActionFromOutput(t, out.String())
	if decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "rejected", "operator rejected"); decideCode != exitSuccess {
		t.Fatalf("decide rejected exit = %d\n%s", decideCode, decideOut)
	}

	// Resume re-proposes the rejected write (the run-1 read is already seeded
	// as citable evidence, so the final grounds on it). The write never
	// executes.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
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
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected write must never execute: %v", err)
	}
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("resume must complete on other evidence:\n%s", resumeOut.String())
	}
}

// TestRunPendingApprovalBlocksCompleted proves the invariant at the CLI level:
// a resumed run that goes straight to a grounded final while a write is still
// awaiting approval pauses again instead of completing.
func TestRunPendingApprovalBlocksCompleted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Create a file with approval.",
		"--workspace", workspace,
		"--scripted", script,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	var out, errOut strings.Builder
	code := run(context.Background(), args, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete")
	}
	taskID := taskIDFromOutput(t, errOut.String())

	// Resume without any operator decision, going straight to a grounded
	// final on the seeded read evidence: the pending approval must block
	// completion.
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode == exitSuccess {
		t.Fatalf("resume must not complete while a write is pending approval:\n%s", resumeOut.String())
	}
	if !strings.Contains(resumeOut.String(), "outcome: approval_required") {
		t.Fatalf("resume must pause with approval_required:\n%s", resumeOut.String())
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if strings.Contains(rendered, "Status: completed") {
		t.Fatalf("a task with a pending write approval must never be completed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Pending approvals:") {
		t.Fatalf("inspect must show the pending approval:\n%s", rendered)
	}
}

// pendingActionFromOutput extracts the action id from the CLI hint line
// "pending approval: action <id>".
func pendingActionFromOutput(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "pending approval: action ") {
			action := strings.TrimSpace(strings.TrimPrefix(line, "pending approval: action "))
			if action != "" {
				return action
			}
		}
	}
	t.Fatalf("pending action id missing from output:\n%s", output)
	return ""
}

// TestRunResumeWithoutDecisionRepauses proves that approval_required survives
// restart: resuming without an operator decision re-proposes the write, the
// policy still gates it, and the run pauses again with approval_required.
func TestRunResumeWithoutDecisionRepauses(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	args := []string{
		"run", "--task", "Create a file with approval.",
		"--workspace", workspace,
		"--scripted", script,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	var out, errOut strings.Builder
	code := run(context.Background(), args, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete")
	}
	taskID := taskIDFromOutput(t, errOut.String())

	// Resume without any operator decision, re-proposing the same write: the
	// persisted approval_required policy still gates it and the run pauses
	// again.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"out.txt","content":"x\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode == exitSuccess {
		t.Fatalf("resume must pause, not complete")
	}
	if !strings.Contains(resumeOut.String(), "outcome: approval_required") {
		t.Fatalf("resume must pause with approval_required:\n%s", resumeOut.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("write must not execute without an operator decision: %v", err)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "Pending approvals:") {
		t.Fatalf("inspect must still show the pending approval:\n%s", rendered)
	}
}
