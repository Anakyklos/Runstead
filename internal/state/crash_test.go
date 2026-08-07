package state

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// crashExitCode is the deterministic exit code the crash seam uses.
const crashExitCode = 42

// Store crash-window tests.
//
// The ADR crash table (docs/adr/0001-durable-execution.md) defines the
// failure boundaries around one external effect. These tests exercise the
// store boundaries with a deterministic test seam (SetCrashPoint) in a real
// subprocess: the process dies at the named boundary and the parent reopens
// the database to prove which state survived.

// TestCrashStoreHelper is the subprocess entry point. It is skipped unless
// RUNSTEAD_CRASH_STORE_HELPER is set; the parent tests spawn it through
// os.Args[0] -test.run=TestCrashStoreHelper.
func TestCrashStoreHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_CRASH_STORE_HELPER") == "" {
		t.Skip("store crash helper")
	}
	dbPath := os.Getenv("RUNSTEAD_CRASH_STORE_DB")
	point := os.Getenv("RUNSTEAD_CRASH_STORE_POINT")
	scenario := os.Getenv("RUNSTEAD_CRASH_STORE_SCENARIO")
	SetCrashPoint(func(name string) {
		if name == point {
			os.Exit(crashExitCode)
		}
	})
	store, err := Open(Options{Path: dbPath, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	switch scenario {
	case "task":
		if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "w"}); err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if err := store.StartTask(ctx, "task-1"); err != nil {
			t.Fatalf("StartTask() error = %v", err)
		}
	case "tool":
		if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "w"}); err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if err := store.StartTask(ctx, "task-1"); err != nil {
			t.Fatalf("StartTask() error = %v", err)
		}
		actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`)})
		if err != nil {
			t.Fatalf("RecordAction() error = %v", err)
		}
		executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
			TaskID: "task-1", ActionID: actionID, Tool: "read_file", Arguments: []byte(`{}`), RecoveryClass: 1,
		})
		if err != nil {
			t.Fatalf("PrepareToolAttempt() error = %v", err)
		}
		obs := tools.Observation{
			ID: "obs-000001", Tool: "read_file", Success: true,
			Data: map[string]any{"content": "alpha"}, Metadata: tools.Metadata{Source: "read_file", Untrusted: true},
		}
		if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
			TaskID: "task-1", ExecutionID: executionID, Status: "completed",
			EvidenceID: obs.ID, DurationNanos: 1, Observation: obs,
		}); err != nil {
			t.Fatalf("CompleteToolAttempt() error = %v", err)
		}
	case "provider":
		if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "w"}); err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if err := store.StartTask(ctx, "task-1"); err != nil {
			t.Fatalf("StartTask() error = %v", err)
		}
		mustProviderAttempt(t, store, "task-1", "task-1-0001", 1)
	case "full":
		if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "w"}); err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if err := store.StartTask(ctx, "task-1"); err != nil {
			t.Fatalf("StartTask() error = %v", err)
		}
		actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`)})
		if err != nil {
			t.Fatalf("RecordAction() error = %v", err)
		}
		executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
			TaskID: "task-1", ActionID: actionID, Tool: "read_file", Arguments: []byte(`{}`), RecoveryClass: 1,
		})
		if err != nil {
			t.Fatalf("PrepareToolAttempt() error = %v", err)
		}
		obs := tools.Observation{
			ID: "obs-000001", Tool: "read_file", Success: true,
			Data: map[string]any{"content": "alpha"}, Metadata: tools.Metadata{Source: "read_file", Untrusted: true},
		}
		if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
			TaskID: "task-1", ExecutionID: executionID, Status: "completed",
			EvidenceID: obs.ID, DurationNanos: 1, Observation: obs,
		}); err != nil {
			t.Fatalf("CompleteToolAttempt() error = %v", err)
		}
		mustProviderAttempt(t, store, "task-1", "task-1-0001", 1)
		if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"}); err != nil {
			t.Fatalf("FinalizeTask() error = %v", err)
		}
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
}

// reopenCrashedStore reopens the database a crashed helper left behind.
func reopenCrashedStore(t *testing.T, dbPath string) *Store {
	t.Helper()
	store, err := Open(Options{Path: dbPath, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("reopen crashed database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func crashDBPath(t *testing.T, scenario, point string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "runstead.db")
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashStoreHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_CRASH_STORE_HELPER=1",
		"RUNSTEAD_CRASH_STORE_DB="+dbPath,
		"RUNSTEAD_CRASH_STORE_SCENARIO="+scenario,
		"RUNSTEAD_CRASH_STORE_POINT="+point,
	)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != crashExitCode {
			t.Fatalf("crash helper exit error = %v", err)
		}
	}
	return dbPath
}

func TestCrashAfterTaskCreatedKeepsTaskPlanned(t *testing.T) {
	dbPath := crashDBPath(t, "task", "task_created_after")
	store := reopenCrashedStore(t, dbPath)

	status, err := store.TaskStatus(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("TaskStatus() error = %v", err)
	}
	if status != "planned" {
		t.Fatalf("status after crash at task_created_after = %q, want planned", status)
	}
	want := []string{"task_created"}
	if got := taskEventKinds(t, store, "task-1"); !equalKinds(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
}

func TestCrashAfterTaskStartedKeepsTaskRunning(t *testing.T) {
	dbPath := crashDBPath(t, "task", "task_started_after")
	store := reopenCrashedStore(t, dbPath)

	status, _ := store.TaskStatus(context.Background(), "task-1")
	if status != "running" {
		t.Fatalf("status after crash at task_started_after = %q, want running", status)
	}
}

// TestCrashAfterProviderTX1 is the core ADR window: after TX 1 commits and
// before the provider effect, the durable intent must survive.
func TestCrashAfterProviderTX1LeavesPreparedIntent(t *testing.T) {
	dbPath := crashDBPath(t, "provider", "provider_tx1_after")
	store := reopenCrashedStore(t, dbPath)

	var status string
	var executionID string
	if err := store.db.QueryRow(
		`SELECT execution_id, status FROM provider_attempts WHERE task_id = 'task-1' AND client_request_id = 'task-1-0001'`).Scan(&executionID, &status); err != nil {
		t.Fatalf("provider attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("provider attempt status = %q, want prepared (intent survives)", status)
	}
	if executionID == "" {
		t.Fatal("provider attempt must have a Runstead execution id")
	}
	// The governor protection projection must contain the debit.
	ledger, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatalf("GovernorState() error = %v", err)
	}
	if len(ledger.RollingEvents) != 1 {
		t.Fatalf("rolling ledger after TX1 = %d entries, want 1 (debit persisted)", len(ledger.RollingEvents))
	}
}

// TestCrashAfterToolTX1LeavesPreparedIntent is the tool-side TX 1 window.
func TestCrashAfterToolTX1LeavesPreparedIntent(t *testing.T) {
	dbPath := crashDBPath(t, "tool", "tool_tx1_after")
	store := reopenCrashedStore(t, dbPath)

	var status string
	if err := store.db.QueryRow(
		`SELECT status FROM tool_attempts WHERE task_id = 'task-1'`).Scan(&status); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("tool attempt status = %q, want prepared", status)
	}
	if countRows(t, store, "tool_results") != 0 {
		t.Fatal("no evidence may exist before TX 2")
	}
}

// TestCrashBeforeToolTX2 proves the "effect completed, result not persisted"
// window: the attempt stays prepared and no citable evidence exists.
func TestCrashBeforeToolTX2LeavesPrepared(t *testing.T) {
	dbPath := crashDBPath(t, "tool", "tool_tx2_before")
	store := reopenCrashedStore(t, dbPath)

	var status, evidenceID string
	if err := store.db.QueryRow(
		`SELECT status, evidence_id FROM tool_attempts WHERE task_id = 'task-1'`).Scan(&status, &evidenceID); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("tool attempt status = %q, want prepared", status)
	}
	if evidenceID != "" {
		t.Fatalf("evidence_id = %q, want empty before TX 2", evidenceID)
	}
	if countRows(t, store, "tool_results") != 0 {
		t.Fatal("TX 2 crash must not leave citable evidence")
	}
}

// TestCrashBeforeProviderTX2LeavesPrepared is the provider-side
// "effect completed, result not persisted" window.
func TestCrashBeforeProviderTX2LeavesPrepared(t *testing.T) {
	dbPath := crashDBPath(t, "provider", "provider_tx2_before")
	store := reopenCrashedStore(t, dbPath)

	var status string
	if err := store.db.QueryRow(
		`SELECT status FROM provider_attempts WHERE task_id = 'task-1' AND client_request_id = 'task-1-0001'`).Scan(&status); err != nil {
		t.Fatalf("provider attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("provider attempt status = %q, want prepared", status)
	}
}

// TestCrashBeforeFinalizeKeepsAttemptsButNoTerminalOutcome proves the window
// between the last TX 2 and the task finalize: everything except the
// terminal outcome is reconstructable.
func TestCrashBeforeFinalizeKeepsAttemptsButNoTerminalOutcome(t *testing.T) {
	dbPath := crashDBPath(t, "full", "task_finalized_before")
	store := reopenCrashedStore(t, dbPath)

	status, err := store.TaskStatus(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("TaskStatus() error = %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running (finalize not committed)", status)
	}
	if countRows(t, store, "tool_attempts") != 1 || countRows(t, store, "provider_attempts") != 1 {
		t.Fatal("attempts must survive the pre-finalize crash")
	}
	var attemptStatus string
	if err := store.db.QueryRow(`SELECT status FROM tool_attempts WHERE task_id = 'task-1'`).Scan(&attemptStatus); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if attemptStatus != "completed" {
		t.Fatalf("tool attempt status = %q, want completed", attemptStatus)
	}
	kinds := taskEventKinds(t, store, "task-1")
	for _, kind := range kinds {
		if kind == "task_finalized" {
			t.Fatal("task_finalized must not exist before the finalize crash window")
		}
	}
}

// TestCrashFullLifecycleWithoutCrashCommitsEverything is the control case:
// no crash point fires and the whole lifecycle commits.
func TestCrashFullLifecycleWithoutCrashCommitsEverything(t *testing.T) {
	dbPath := crashDBPath(t, "full", "never-fires")
	store := reopenCrashedStore(t, dbPath)

	status, _ := store.TaskStatus(context.Background(), "task-1")
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}
	if countRows(t, store, "tool_attempts") != 1 || countRows(t, store, "tool_results") != 1 ||
		countRows(t, store, "provider_attempts") != 1 {
		t.Fatal("full lifecycle must persist attempts and evidence")
	}
	want := []string{"task_created", "task_started", "action_planned", "tool_attempt_prepared",
		"tool_attempt_completed", "provider_attempt_prepared", "provider_attempt_completed", "task_finalized"}
	if got := taskEventKinds(t, store, "task-1"); !equalKinds(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
}

func TestCrashPointSeamFiresOnlyOnExactMatch(t *testing.T) {
	store := openTestStore(t)
	fired := ""
	SetCrashPoint(func(name string) { fired = name })
	defer SetCrashPoint(nil)
	mustTask(t, store, "task-1")
	if fired != "task_started_after" {
		t.Fatalf("seam fired %q, want task_started_after", fired)
	}
	// With the seam removed, no crash point fires on subsequent operations.
	SetCrashPoint(nil)
	actionID, err := store.RecordAction(context.Background(), ActionRecord{TaskID: "task-1", Tool: "t", Arguments: []byte(`{}`)})
	if err != nil || actionID == "" {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if fired != "task_started_after" {
		t.Fatalf("seam fired after being disabled: %q", fired)
	}
}

func equalKinds(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
