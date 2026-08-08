package recovery_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Write-specific recovery tests (issue #10 extending #9): an interrupted
// write attempt (ADR recovery class 2) is reconciled from observable
// filesystem state, never re-executed.

func mustTaskInWorkspace(t *testing.T, store *state.Store, taskID, workspace string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: taskID, Objective: "modify the workspace", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"max_corrections":2,"max_repeated_actions":2,"time_budget_ns":600000000000,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
}

func mustWriteAction(t *testing.T, store *state.Store, taskID, tool, arguments string, afterHash string) (string, string) {
	t.Helper()
	actionID, err := store.RecordAction(context.Background(), state.ActionRecord{
		TaskID: taskID, Tool: tool, Arguments: []byte(arguments),
		Fingerprint: "fp-write", WorkspaceSignature: "sig",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(context.Background(), state.ToolAttemptPrepared{
		TaskID: taskID, ActionID: actionID, Tool: tool, Arguments: []byte(arguments),
		RecoveryClass: 2, EffectAfterHash: afterHash,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	return actionID, executionID
}

func attemptByExecution(t *testing.T, store *state.Store, taskID, executionID string) state.RecoveryToolAttempt {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.ExecutionID == executionID {
			return attempt
		}
	}
	t.Fatalf("attempt %s not found", executionID)
	return state.RecoveryToolAttempt{}
}

func TestResumeReconcilesWriteEffectNotStarted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	store := openStore(t)
	taskID := "task-write-notstarted"
	mustTaskInWorkspace(t, store, taskID, workspace)
	_, executionID := mustWriteAction(t, store, taskID, tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`, after)

	// The file is still in the recorded precondition state: the effect never
	// started, so the attempt reconciles without repeating it.
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: taskID})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %s, want continue", plan.Decision)
	}
	if plan.ReconciledToolAttempts != 1 {
		t.Fatalf("reconciled tool attempts = %d, want 1", plan.ReconciledToolAttempts)
	}
	attempt := attemptByExecution(t, store, taskID, executionID)
	if attempt.Status != "reconciled" || attempt.RecoveryReason != "write_effect_not_started" {
		t.Fatalf("attempt = status %q reason %q, want reconciled/write_effect_not_started", attempt.Status, attempt.RecoveryReason)
	}
	if plan.NextEvidenceSequence != 0 {
		t.Fatalf("next evidence sequence = %d, want 0 (no evidence produced)", plan.NextEvidenceSequence)
	}
}

func TestResumeReconcilesWriteEffectCompleted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	store := openStore(t)
	taskID := "task-write-completed"
	mustTaskInWorkspace(t, store, taskID, workspace)
	_, executionID := mustWriteAction(t, store, taskID, tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`, after)

	// The file is exactly the expected after-state: the effect completed
	// before the crash, so recovery verifies it from the filesystem.
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: taskID})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %s, want continue", plan.Decision)
	}
	attempt := attemptByExecution(t, store, taskID, executionID)
	if attempt.Status != "reconciled" || attempt.RecoveryReason != "write_effect_completed" {
		t.Fatalf("attempt = status %q reason %q, want reconciled/write_effect_completed", attempt.Status, attempt.RecoveryReason)
	}
	// The observed write evidence is citable and the resumed registry will
	// continue the evidence id space after it.
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].EvidenceID != "obs-000001" {
		t.Fatalf("reconciled evidence = %+v, want obs-000001", snapshot.Evidence)
	}
	if plan.NextEvidenceSequence != 1 {
		t.Fatalf("next evidence sequence = %d, want 1", plan.NextEvidenceSequence)
	}
	// A resumed run can ground a final on the reconciled evidence.
	if len(plan.Seed.Evidence) != 1 || plan.Seed.Evidence[0].ID != "obs-000001" {
		t.Fatalf("recovery seed evidence = %+v", plan.Seed.Evidence)
	}
}

func TestResumeWriteUnreconcilableStopsHumanReview(t *testing.T) {
	workspace := t.TempDir()
	// The file matches neither the recorded precondition nor the expected
	// after-state: an unrelated change happened after the crash.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("third-party\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	store := openStore(t)
	taskID := "task-write-unreconcilable"
	mustTaskInWorkspace(t, store, taskID, workspace)
	_, executionID := mustWriteAction(t, store, taskID, tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`, after)

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: taskID})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionHumanReview {
		t.Fatalf("decision = %s, want human_review_required", plan.Decision)
	}
	attempt := attemptByExecution(t, store, taskID, executionID)
	if attempt.Status != "human_review_required" || attempt.RecoveryReason != "write_effect_unreconcilable" {
		t.Fatalf("attempt = status %q reason %q, want human_review_required/write_effect_unreconcilable", attempt.Status, attempt.RecoveryReason)
	}
	if plan.Seed != nil {
		t.Fatal("no seed may be produced for a human-review decision")
	}
}

func TestResumeCompletedWriteSeedsRepeatGuard(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	store := openStore(t)
	taskID := "task-write-guard"
	mustTaskInWorkspace(t, store, taskID, workspace)
	mustWriteAction(t, store, taskID, tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`, after)

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: taskID})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %s, want continue", plan.Decision)
	}
	// The verified-completed write must seed the repeat guard so a resumed
	// run does not blindly re-propose the same write while the workspace is
	// unchanged.
	if len(plan.Seed.Guard) != 1 {
		t.Fatalf("repeat guard seed = %v, want the completed write fingerprint", plan.Seed.Guard)
	}
}

func TestResumeNotStartedWriteDoesNotSeedRepeatGuard(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tools.HashBytes([]byte("old\n"))
	after := tools.HashBytes([]byte("new\n"))

	store := openStore(t)
	taskID := "task-write-reconsider"
	mustTaskInWorkspace(t, store, taskID, workspace)
	mustWriteAction(t, store, taskID, tools.ToolWriteFile,
		`{"path":"a.txt","content":"new\n","expected_before_hash":"`+before+`"}`, after)

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: taskID})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %s, want continue", plan.Decision)
	}
	// A write that never started must NOT seed the guard: the model may
	// legitimately re-propose it after resume.
	if len(plan.Seed.Guard) != 0 {
		t.Fatalf("repeat guard seed = %v, want empty for a not-started write", plan.Seed.Guard)
	}
}
