package state

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mustAction(t *testing.T, store *Store, taskID, tool, arguments, fingerprint, signature string) string {
	t.Helper()
	actionID, err := store.RecordAction(context.Background(), ActionRecord{
		TaskID: taskID, Tool: tool, Arguments: []byte(arguments),
		Fingerprint: fingerprint, WorkspaceSignature: signature,
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	return actionID
}

// mustProviderAttemptPreparedOnly records a provider attempt intent without
// finishing it (the interrupted window).
func mustProviderAttemptPreparedOnly(t *testing.T, store *Store, taskID, requestID string, sequence int) {
	t.Helper()
	now := newFixedClock().Now()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: sequence + 1,
		Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:      governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		RollingEvents: []governor.LedgerEvent{{At: now, TaskID: taskID}},
		TaskStates:    []governor.TaskStateRecord{{TaskID: taskID, Attempts: sequence, Retries: 0, LastTouched: now}},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: taskID, ClientRequestID: requestID, ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: sequence,
		StartedAt: now, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
}

// mustToolAttemptPreparedOnly records a tool attempt intent (TX 1) without
// completing it: the interrupted window that recovery reconciles.
func mustToolAttemptPreparedOnly(t *testing.T, store *Store, taskID, actionID, tool string) string {
	t.Helper()
	executionID, err := store.PrepareToolAttempt(context.Background(), ToolAttemptPrepared{
		TaskID: taskID, ActionID: actionID, Tool: tool,
		Arguments: []byte(`{"path":"a.txt"}`), RecoveryClass: 1,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	return executionID
}

func loadToolAttemptsForTest(t *testing.T, store *Store, taskID string) []RecoveryToolAttempt {
	t.Helper()
	attempts, err := store.loadRecoveryToolAttempts(context.Background(), taskID)
	if err != nil {
		t.Fatalf("loadRecoveryToolAttempts() error = %v", err)
	}
	return attempts
}

func renderForTest(t *testing.T, store *Store, taskID string) string {
	t.Helper()
	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, taskID); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	return out.String()
}

func TestMarkRecoveryStartedIncrementsResumeCount(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	if err := store.MarkRecoveryStarted(context.Background(), "task-1", "task interrupted"); err != nil {
		t.Fatalf("MarkRecoveryStarted() error = %v", err)
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if snapshot.Task.ResumeCount != 1 {
		t.Errorf("resume count = %d, want 1", snapshot.Task.ResumeCount)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsString(kinds, "recovery_started") {
		t.Errorf("journal must contain recovery_started: %v", kinds)
	}
}

func TestMarkRecoveryStartedOnPlannedTaskMovesToRunning(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-planned", Objective: "o", Workspace: "/ws", Model: "scripted"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRecoveryStarted(ctx, "task-planned", "interrupted before start"); err != nil {
		t.Fatalf("MarkRecoveryStarted() error = %v", err)
	}
	status, err := store.TaskStatus(ctx, "task-planned")
	if err != nil || status != "running" {
		t.Fatalf("task status = %q, err %v; want running", status, err)
	}
}

func TestMarkRecoveryStartedOnTerminalTaskFails(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustFinalize(t, store, "task-1")
	err := store.MarkRecoveryStarted(context.Background(), "task-1", "interrupted")
	if !errors.Is(err, ErrNotResumable) {
		t.Fatalf("MarkRecoveryStarted() error = %v, want ErrNotResumable", err)
	}
}

func TestReconcileToolAttemptPersistsDecisionAndEvent(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	actionID := mustAction(t, store, "task-1", "read_file", `{"path":"a.txt"}`, "fp", "sig")
	executionID := mustToolAttemptPreparedOnly(t, store, "task-1", actionID, "read_file")

	if err := store.ReconcileToolAttempt(context.Background(), ReconcileToolAttempt{
		TaskID: "task-1", ExecutionID: executionID, Status: "reconciled", Reason: "replay_safe_observation",
	}); err != nil {
		t.Fatalf("ReconcileToolAttempt() error = %v", err)
	}
	attempts := loadToolAttemptsForTest(t, store, "task-1")
	if len(attempts) != 1 || attempts[0].Status != "reconciled" || attempts[0].RecoveryReason != "replay_safe_observation" {
		t.Fatalf("attempt after reconcile = %+v", attempts)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsString(kinds, "tool_attempt_reconciled") {
		t.Errorf("journal must contain tool_attempt_reconciled: %v", kinds)
	}
}

func TestReconcileToolAttemptRefusesTerminalAttempt(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	actionID := mustAction(t, store, "task-1", "read_file", `{"path":"a.txt"}`, "fp", "sig")
	// mustToolAttempt completes the attempt with evidence obs-000001: a
	// terminal attempt must not be reconcilable.
	executionID := mustToolAttempt(t, store, "task-1", actionID)

	err := store.ReconcileToolAttempt(context.Background(), ReconcileToolAttempt{
		TaskID: "task-1", ExecutionID: executionID, Status: "reconciled", Reason: "replay_safe_observation",
	})
	if !errors.Is(err, ErrNotReconcilable) {
		t.Fatalf("ReconcileToolAttempt() on a completed attempt error = %v, want ErrNotReconcilable", err)
	}
}

func TestReconcileToolAttemptHumanReviewMarksAction(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	actionID := mustAction(t, store, "task-1", "write_file", `{"path":"x"}`, "fp", "sig")
	executionID := mustToolAttemptPreparedOnly(t, store, "task-1", actionID, "write_file")

	if err := store.ReconcileToolAttempt(context.Background(), ReconcileToolAttempt{
		TaskID: "task-1", ExecutionID: executionID, Status: "human_review_required", Reason: "unreconcilable_effect",
	}); err != nil {
		t.Fatalf("ReconcileToolAttempt() error = %v", err)
	}
	var actionStatus string
	if err := store.db.QueryRow(`SELECT status FROM actions WHERE action_id = ?`, actionID).Scan(&actionStatus); err != nil {
		t.Fatal(err)
	}
	if actionStatus != "human_review_required" {
		t.Errorf("action status = %q, want human_review_required", actionStatus)
	}
}

func TestReconcileProviderAttemptPreservesDebit(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttemptPreparedOnly(t, store, "task-1", "task-1-0001", 1)
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.ProviderAttempts) != 1 {
		t.Fatalf("fixture provider attempts = %d, want 1", len(snapshot.ProviderAttempts))
	}
	executionID := snapshot.ProviderAttempts[0].ExecutionID

	if err := store.ReconcileProviderAttempt(context.Background(), ReconcileProviderAttempt{
		TaskID: "task-1", ExecutionID: executionID, ClientRequestID: "task-1-0001",
		Status: "reconciled", Reason: "upstream_may_have_been_reached", Uncertain: true, AttemptDebited: 1,
	}); err != nil {
		t.Fatalf("ReconcileProviderAttempt() error = %v", err)
	}
	var status, reason string
	var uncertain, debited int
	if err := store.db.QueryRow(
		`SELECT status, recovery_reason, uncertain, attempt_debited FROM provider_attempts WHERE task_id = ?`,
		"task-1").Scan(&status, &reason, &uncertain, &debited); err != nil {
		t.Fatal(err)
	}
	if status != "reconciled" || reason != "upstream_may_have_been_reached" || uncertain != 1 || debited != 1 {
		t.Errorf("reconciled provider attempt = %s/%s uncertain=%d debited=%d", status, reason, uncertain, debited)
	}
}

func TestMarkHumanReviewRequiredPersistsState(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	if err := store.MarkHumanReviewRequired(context.Background(), "task-1", "cannot reconcile effect", []string{"exec-1", "exec-2"}); err != nil {
		t.Fatalf("MarkHumanReviewRequired() error = %v", err)
	}
	status, err := store.TaskStatus(context.Background(), "task-1")
	if err != nil || status != "human_review_required" {
		t.Fatalf("task status = %q, err %v; want human_review_required", status, err)
	}
	rendered := renderForTest(t, store, "task-1")
	if !strings.Contains(rendered, "task_human_review_required") {
		t.Errorf("inspect must show the human-review event:\n%s", rendered)
	}
	if !strings.Contains(rendered, "cannot reconcile effect") {
		t.Errorf("inspect must show the stop reason:\n%s", rendered)
	}
}

func TestAppendRecoveryEventJournalsOnly(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	if err := store.AppendRecoveryEvent(context.Background(), "task-1", "recovery_continued", map[string]any{"turns": 1}); err != nil {
		t.Fatalf("AppendRecoveryEvent() error = %v", err)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsString(kinds, "recovery_continued") {
		t.Errorf("journal must contain recovery_continued: %v", kinds)
	}
}

func TestRecoveryReasonsAreRedacted(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	actionID := mustAction(t, store, "task-1", "read_file", `{"path":"a.txt"}`, "fp", "sig")
	executionID := mustToolAttemptPreparedOnly(t, store, "task-1", actionID, "read_file")
	secretReason := "recovery saw Bearer sk-0123456789abcdef"
	if err := store.ReconcileToolAttempt(context.Background(), ReconcileToolAttempt{
		TaskID: "task-1", ExecutionID: executionID, Status: "reconciled", Reason: secretReason,
	}); err != nil {
		t.Fatalf("ReconcileToolAttempt() error = %v", err)
	}
	var stored string
	if err := store.db.QueryRow(`SELECT recovery_reason FROM tool_attempts WHERE execution_id = ?`, executionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "sk-0123456789abcdef") {
		t.Fatalf("recovery reason leaked the secret: %q", stored)
	}
	if !strings.Contains(stored, "<redacted>") {
		t.Errorf("recovery reason must carry the redaction marker: %q", stored)
	}
}

func TestLoadRecoverySnapshotIncludesWorkspaceSignature(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustAction(t, store, "task-1", "read_file", `{"path":"a.txt"}`, "fp", "sig-workspace-1")
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.Actions) != 1 || snapshot.Actions[0].WorkspaceSignature != "sig-workspace-1" {
		t.Fatalf("recovery actions = %+v, want workspace signature", snapshot.Actions)
	}
}
