package state

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
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

// mustProviderAttemptReceiptAwarePreparedOnly records a receipt-aware provider
// attempt intent whose TX 1 governor projection has NO debit, mirroring
// StartReceiptAware: all debits are deferred to the receipt finish path, so a
// crash between TX 1 and TX 2 leaves the account protection not debited.
func mustProviderAttemptReceiptAwarePreparedOnly(t *testing.T, store *Store, taskID, requestID string, sequence int) {
	t.Helper()
	now := newFixedClock().Now()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: sequence + 1,
		Circuit:  governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		// No RollingEvents and no TaskStates: StartReceiptAware defers debits.
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: taskID, ClientRequestID: requestID, ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: sequence,
		StartedAt: now, ReceiptAware: true, State: persisted,
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

// TestReconcileReceiptAwareAttemptAppliesConservativeDebit covers the reviewer
// blocker: a receipt-aware attempt interrupted before TX 2 was never debited by
// StartReceiptAware, so recovery must apply the #29 conservative debit to the
// persisted governor projection atomically with the attempt reconciliation.
func TestReconcileReceiptAwareAttemptAppliesConservativeDebit(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttemptReceiptAwarePreparedOnly(t, store, "task-1", "task-1-0001", 1)

	// Before reconciliation the persisted governor projection has NO debit for
	// the task and telemetry is safe.
	before, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = %v, %v", ok, err)
	}
	if len(before.RollingEvents) != 0 {
		t.Fatalf("receipt-aware TX1 must not have debited the ledger: %v", before.RollingEvents)
	}
	if len(before.TaskStates) != 0 {
		t.Fatalf("receipt-aware TX1 must not have debited task attempts: %v", before.TaskStates)
	}
	if before.Telemetry.Unsafe {
		t.Fatal("receipt-aware TX1 must not mark telemetry unsafe")
	}

	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.ProviderAttempts) != 1 || !snapshot.ProviderAttempts[0].ReceiptAware {
		t.Fatalf("fixture must be a receipt-aware attempt: %+v", snapshot.ProviderAttempts)
	}
	attempt := snapshot.ProviderAttempts[0]

	if err := store.ReconcileProviderAttempt(context.Background(), ReconcileProviderAttempt{
		TaskID: "task-1", ExecutionID: attempt.ExecutionID, ClientRequestID: attempt.ClientRequestID,
		Status: "reconciled", Reason: "upstream_may_have_been_reached", Uncertain: true,
		AttemptDebited: 1, ApplyConservativeDebit: true,
	}); err != nil {
		t.Fatalf("ReconcileProviderAttempt() error = %v", err)
	}

	// The persisted governor projection now carries the conservative debit:
	// one task attempt, one rolling ledger event and telemetry unsafe.
	after, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = %v, %v", ok, err)
	}
	if len(after.RollingEvents) != 1 || after.RollingEvents[0].TaskID != "task-1" {
		t.Errorf("ledger after reconciliation = %v, want one task-1 event", after.RollingEvents)
	}
	if len(after.TaskStates) != 1 || after.TaskStates[0].TaskID != "task-1" || after.TaskStates[0].Attempts != 1 {
		t.Errorf("task states after reconciliation = %v, want task-1 attempts=1", after.TaskStates)
	}
	if !after.Telemetry.Unsafe {
		t.Error("telemetry must be unsafe after the conservative debit")
	}

	// Re-reconciling the already-terminal attempt fails: the debit is never
	// applied twice.
	err = store.ReconcileProviderAttempt(context.Background(), ReconcileProviderAttempt{
		TaskID: "task-1", ExecutionID: attempt.ExecutionID, ClientRequestID: attempt.ClientRequestID,
		Status: "reconciled", Reason: "upstream_may_have_been_reached", Uncertain: true,
		AttemptDebited: 1, ApplyConservativeDebit: true,
	})
	if !errors.Is(err, ErrNotReconcilable) {
		t.Fatalf("second reconcile error = %v, want ErrNotReconcilable", err)
	}
	// The attempt and journal are consistent: exactly one reconciliation event.
	kinds := taskEventKinds(t, store, "task-1")
	count := 0
	for _, kind := range kinds {
		if kind == "provider_attempt_reconciled" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("provider_attempt_reconciled events = %d, want 1", count)
	}
}

// TestReconcileReceiptAwareDebitKeepsOriginalAttemptTime covers the temporal
// accounting blocker: the conservative rolling-ledger debit must be dated with
// the ORIGINAL permit start (provider_attempts.prepared_at), not with the
// resume/reconciliation time, so the 10m/1h/3h windows represent when the
// upstream attempt possibly happened. A task interrupted 3h1m before the
// resume must NOT get a fresh debit at resume time that keeps it inside the 3h
// window.
func TestReconcileReceiptAwareDebitKeepsOriginalAttemptTime(t *testing.T) {
	clock := newFixedClock() // store clock: 2026-01-01T12:00:00Z
	store, err := Open(Options{
		Path:  filepath.Join(t.TempDir(), "runstead.db"),
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mustTask(t, store, "task-1")

	now := clock.Now()
	past := now.Add(-3*time.Hour - time.Minute) // outside the 3h rolling window
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:  governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-0001", ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: 1,
		StartedAt: past, ReceiptAware: true, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if !snapshot.ProviderAttempts[0].PreparedAt.Equal(past) {
		t.Fatalf("recovery snapshot PreparedAt = %v, want %v", snapshot.ProviderAttempts[0].PreparedAt, past)
	}
	attempt := snapshot.ProviderAttempts[0]

	if err := store.ReconcileProviderAttempt(context.Background(), ReconcileProviderAttempt{
		TaskID: "task-1", ExecutionID: attempt.ExecutionID, ClientRequestID: attempt.ClientRequestID,
		Status: "reconciled", Reason: "upstream_may_have_been_reached", Uncertain: true,
		AttemptDebited: 1, DebitAt: attempt.PreparedAt, ApplyConservativeDebit: true,
	}); err != nil {
		t.Fatalf("ReconcileProviderAttempt() error = %v", err)
	}

	after, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = %v, %v", ok, err)
	}
	// The ledger event keeps the ORIGINAL attempt time, not the resume time,
	// and lastStart moved to it exactly like finishReceiptFailureLocked.
	if len(after.RollingEvents) != 1 || !after.RollingEvents[0].At.Equal(past) {
		t.Fatalf("ledger event = %v, want At=%v (original attempt time)", after.RollingEvents, past)
	}
	if !after.LastStart.Equal(past) {
		t.Errorf("lastStart = %v, want %v", after.LastStart, past)
	}

	// A governor restored at the current time must NOT count the expired event
	// in the 3h window (it happened 3h1m ago). The persisted projection is
	// authoritative and retains the task attempt count; the runtime governor
	// prunes an untouched task state after its 3h retention window (existing
	// #8 behavior), so the in-memory task map is empty for a 3h1m-old attempt.
	config := governor.DefaultInstantConfig("runstead-cli", "scripted", "instant", provider.SafeRouteSafety())
	accountGovernor, err := governor.New(config, governor.Options{Clock: &governorFakeClock{now: now}, Restore: &after})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	budgets := accountGovernor.Snapshot().Budgets
	if budgets.Rolling3hUsed != 0 {
		t.Errorf("rolling 3h used = %d, want 0 (event expired from the window)", budgets.Rolling3hUsed)
	}
	if len(after.TaskStates) != 1 || after.TaskStates[0].Attempts != 1 {
		t.Errorf("persisted task projection = %+v, want attempts=1 (conservative debit retained)", after.TaskStates)
	}
}

// TestReconcileReceiptAwareDebitStaysInsideWindow is the in-window complement:
// an attempt started 2h50m before the resume keeps its ledger event inside the
// 3h window at the ORIGINAL time, exactly as the normal receipt-failure path
// would have dated it.
func TestReconcileReceiptAwareDebitStaysInsideWindow(t *testing.T) {
	clock := newFixedClock()
	store, err := Open(Options{
		Path:  filepath.Join(t.TempDir(), "runstead.db"),
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mustTask(t, store, "task-1")

	now := clock.Now()
	inWindow := now.Add(-2*time.Hour - 50*time.Minute)
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:  governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-0001", ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: 1,
		StartedAt: inWindow, ReceiptAware: true, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	attempt := snapshot.ProviderAttempts[0]
	if err := store.ReconcileProviderAttempt(context.Background(), ReconcileProviderAttempt{
		TaskID: "task-1", ExecutionID: attempt.ExecutionID, ClientRequestID: attempt.ClientRequestID,
		Status: "reconciled", Reason: "upstream_may_have_been_reached", Uncertain: true,
		AttemptDebited: 1, DebitAt: attempt.PreparedAt, ApplyConservativeDebit: true,
	}); err != nil {
		t.Fatalf("ReconcileProviderAttempt() error = %v", err)
	}
	after, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = %v, %v", ok, err)
	}
	if len(after.RollingEvents) != 1 || !after.RollingEvents[0].At.Equal(inWindow) {
		t.Fatalf("ledger event = %v, want At=%v", after.RollingEvents, inWindow)
	}
	config := governor.DefaultInstantConfig("runstead-cli", "scripted", "instant", provider.SafeRouteSafety())
	accountGovernor, err := governor.New(config, governor.Options{Clock: &governorFakeClock{now: now}, Restore: &after})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	if used := accountGovernor.Snapshot().Budgets.Rolling3hUsed; used != 1 {
		t.Errorf("rolling 3h used = %d, want 1 (event still inside the window)", used)
	}
	if used := accountGovernor.Snapshot().Budgets.Rolling1hUsed; used != 0 {
		t.Errorf("rolling 1h used = %d, want 0 (event outside the 1h window)", used)
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
