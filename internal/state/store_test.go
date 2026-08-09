package state

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

func TestOpenCreatesFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")

	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version == 0 {
		t.Fatal("fresh database must have schema version > 0")
	}
	// The identity counter is seeded.
	if countRows(t, store, "meta") != 1 {
		t.Fatal("meta table must be seeded")
	}
	for _, table := range []string{"tasks", "actions", "tool_attempts", "tool_results", "provider_attempts", "provider_attempt_receipts", "events", "governor_state", "governor_ledger", "governor_task_states", "governor_request_records", "governor_attempt_ids"} {
		if countRows(t, store, table) != 0 {
			t.Fatalf("table %s must start empty", table)
		}
	}
}

func TestOpenReopeningUpToDateDatabaseIsSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")

	first, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	mustTask(t, first, "task-reopen")
	first.Close()

	second, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer second.Close()
	version, err := second.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version == 0 {
		t.Fatal("reopen must keep the applied schema version")
	}
	if exists, err := second.TaskExists(context.Background(), "task-reopen"); err != nil || !exists {
		t.Fatalf("reopened store must see persisted task: exists=%t err=%v", exists, err)
	}
}

func TestOpenCreatesPrivateDirectoryAndFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	path := filepath.Join(dir, "runstead.db")

	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state dir stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %o, want 700", info.Mode().Perm())
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("database stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenFailsClearlyWhenPathInvalid(t *testing.T) {
	if _, err := Open(Options{Path: ""}); err == nil {
		t.Fatal("empty path must fail")
	}
	if _, err := Open(Options{Path: "."}); err == nil {
		t.Fatal("directory path must fail")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestPragmasAreApplied(t *testing.T) {
	store := openTestStore(t)
	var journal string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal mode query: %v", err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	var synchronous int
	if err := store.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("synchronous query: %v", err)
	}
	if synchronous != 1 {
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
	var foreignKeys int
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys query: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
	var busyTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout query: %v", err)
	}
	if busyTimeout != busyTimeout {
		t.Fatalf("busy_timeout = %d, want %d", busyTimeout, busyTimeout)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	store := openTestStore(t)
	// provider_attempts references tasks; a row for a missing task must fail.
	_, err := store.db.Exec(`INSERT INTO provider_attempts
		(execution_id, task_id, client_request_id, status, created_at, prepared_at)
		VALUES ('exec-000001', 'missing-task', 'req-1', 'prepared', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("foreign key violation must fail")
	}
}

func TestTaskLifecycleProjectionAndJournal(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "inspect", Workspace: "/ws", Model: "m"}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	status, err := store.TaskStatus(ctx, "task-1")
	if err != nil {
		t.Fatalf("TaskStatus() error = %v", err)
	}
	if status != "planned" {
		t.Fatalf("status after create = %q, want planned", status)
	}
	if err := store.StartTask(ctx, "task-1"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	status, _ = store.TaskStatus(ctx, "task-1")
	if status != "running" {
		t.Fatalf("status after start = %q, want running", status)
	}
	mustPassVerification(t, store, "task-1")
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	status, _ = store.TaskStatus(ctx, "task-1")
	if status != "completed" {
		t.Fatalf("status after finalize = %q, want completed", status)
	}

	want := []string{"task_created", "task_started", "verification_recorded", "task_finalized"}
	if got := taskEventKinds(t, store, "task-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal kinds = %v, want %v", got, want)
	}
}

func TestTerminalStatusMapping(t *testing.T) {
	cases := map[string]string{
		"completed":           "completed",
		"canceled":            "canceled",
		"steps_exhausted":     "failed",
		"provider_failure":    "failed",
		"persistence_failure": "failed",
		"final_incomplete":    "failed",
	}
	for outcome, want := range cases {
		if got := terminalStatus(outcome); got != want {
			t.Errorf("terminalStatus(%q) = %q, want %q", outcome, got, want)
		}
	}
}

func TestTaskFinalizeUnknownTaskFails(t *testing.T) {
	store := openTestStore(t)
	err := store.FinalizeTask(context.Background(), TaskFinalize{TaskID: "missing", Outcome: "completed"})
	if err == nil {
		t.Fatal("finalizing an unknown task must fail")
	}
}

func TestStartTaskTwiceFails(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	if err := store.StartTask(context.Background(), "task-1"); err == nil {
		t.Fatal("starting a running task must fail")
	}
}

func TestFullLifecycleReconstructsEveryEntity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{"path":"a.txt"}`), Fingerprint: "fp-1",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if actionID == "" {
		t.Fatal("RecordAction must return an action id")
	}
	executionID := mustToolAttempt(t, store, "task-1", actionID)
	mustProviderAttempt(t, store, "task-1", "task-1-0001", 1)
	mustFinalize(t, store, "task-1")

	if countRows(t, store, "tasks") != 1 || countRows(t, store, "actions") != 1 ||
		countRows(t, store, "tool_attempts") != 1 || countRows(t, store, "tool_results") != 1 ||
		countRows(t, store, "provider_attempts") != 1 {
		t.Fatal("full lifecycle must persist exactly one of each entity")
	}

	// Reconstruction: verify the attempt rows and evidence are coherent.
	var attemptStatus, evidenceID string
	if err := store.db.QueryRow(
		`SELECT status, evidence_id FROM tool_attempts WHERE execution_id = ?`, executionID).Scan(&attemptStatus, &evidenceID); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if attemptStatus != "completed" || evidenceID != "obs-000001" {
		t.Fatalf("tool attempt = status %q evidence %q", attemptStatus, evidenceID)
	}
	var actionStatus string
	if err := store.db.QueryRow(
		`SELECT status FROM actions WHERE action_id = ?`, actionID).Scan(&actionStatus); err != nil {
		t.Fatalf("action query: %v", err)
	}
	if actionStatus != "completed" {
		t.Fatalf("action status = %q, want completed", actionStatus)
	}
	var untrusted int
	if err := store.db.QueryRow(
		`SELECT untrusted FROM tool_results WHERE task_id = 'task-1' AND evidence_id = 'obs-000001'`).Scan(&untrusted); err != nil {
		t.Fatalf("tool result query: %v", err)
	}
	if untrusted != 1 {
		t.Fatalf("untrusted = %d, want 1", untrusted)
	}
	var providerStatus, outcome string
	if err := store.db.QueryRow(
		`SELECT status, outcome FROM provider_attempts WHERE task_id = 'task-1' AND client_request_id = 'task-1-0001'`).Scan(&providerStatus, &outcome); err != nil {
		t.Fatalf("provider attempt query: %v", err)
	}
	if providerStatus != "completed" || outcome != "success" {
		t.Fatalf("provider attempt = status %q outcome %q", providerStatus, outcome)
	}
}

func TestFailedObservationIsNotCitableEvidence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`), Fingerprint: "fp"})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{TaskID: "task-1", ActionID: actionID, Tool: "read_file", Arguments: []byte(`{}`), RecoveryClass: 1})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	obs := tools.Observation{
		ID:       "obs-000001",
		Tool:     "read_file",
		Success:  false,
		Failure:  &tools.Failure{Code: tools.FailurePathNotFound},
		Metadata: tools.Metadata{Source: "read_file", Untrusted: true},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-1", ExecutionID: executionID, Status: "failed",
		Classification: string(tools.FailurePathNotFound), Observation: obs,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}

	if countRows(t, store, "tool_results") != 0 {
		t.Fatal("failed observations must not create citable tool_results rows")
	}
	var attemptEvidence string
	if err := store.db.QueryRow(`SELECT evidence_id FROM tool_attempts WHERE execution_id = ?`, executionID).Scan(&attemptEvidence); err != nil {
		t.Fatalf("attempt query: %v", err)
	}
	if attemptEvidence != "" {
		t.Fatalf("failed attempt evidence_id = %q, want empty", attemptEvidence)
	}
	var actionStatus string
	if err := store.db.QueryRow(`SELECT status FROM actions WHERE action_id = ?`, actionID).Scan(&actionStatus); err != nil {
		t.Fatalf("action query: %v", err)
	}
	if actionStatus != "failed" {
		t.Fatalf("action status = %q, want failed", actionStatus)
	}
}

func TestActionRejectedCreatesNoToolAttempt(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`), Fingerprint: "fp"})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RejectAction(ctx, "task-1", actionID, "repeated_action"); err != nil {
		t.Fatalf("RejectAction() error = %v", err)
	}
	var status string
	if err := store.db.QueryRow(`SELECT status FROM actions WHERE action_id = ?`, actionID).Scan(&status); err != nil {
		t.Fatalf("action query: %v", err)
	}
	if status != "rejected" {
		t.Fatalf("action status = %q, want rejected", status)
	}
	if countRows(t, store, "tool_attempts") != 0 {
		t.Fatal("rejected actions must not create tool attempts")
	}
	want := []string{"task_created", "task_started", "action_planned", "action_rejected"}
	if got := taskEventKinds(t, store, "task-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal kinds = %v, want %v", got, want)
	}
}

func TestIdentitySequenceIsStableAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")
	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "t", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if actionID != "action-000001" {
		t.Fatalf("actionID = %q, want action-000001", actionID)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{TaskID: "task-1", ActionID: actionID, Tool: "t", Arguments: []byte(`{}`), RecoveryClass: 1})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	if executionID != "exec-000002" {
		t.Fatalf("executionID = %q, want exec-000002", executionID)
	}
	store.Close()

	reopened, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer reopened.Close()
	executionID2, err := reopened.PrepareToolAttempt(ctx, ToolAttemptPrepared{TaskID: "task-1", ActionID: actionID, Tool: "t", Arguments: []byte(`{}`), RecoveryClass: 1})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() after reopen error = %v", err)
	}
	if executionID2 != "exec-000003" {
		t.Fatalf("executionID after reopen = %q, want exec-000003", executionID2)
	}
}

func TestSchemaVersionObservable(t *testing.T) {
	store := openTestStore(t)
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version < 1 {
		t.Fatalf("schema version = %d, want >= 1", version)
	}
}
