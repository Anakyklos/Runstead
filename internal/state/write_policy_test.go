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

func TestRecordWritePolicyDecisionPersistsWithEvent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}

	if err := store.RecordWritePolicyDecision(ctx, WritePolicyDecision{
		TaskID: "task-1", ActionID: actionID, Tool: "write_file",
		Decision: "denied", Reason: "policy_deny",
	}); err != nil {
		t.Fatalf("RecordWritePolicyDecision() error = %v", err)
	}
	if countRows(t, store, "write_policy_decisions") != 1 {
		t.Fatal("policy decision must be persisted")
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsKind(kinds, "write_policy_decision") {
		t.Fatalf("journal missing write_policy_decision: %v", kinds)
	}
}

func TestRecordApprovalUpsertsAndApprovalLookup(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-write-1",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	approvalID, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-1", ActionID: actionID, Decision: "approved", Reason: "ok", Actor: "operator",
	})
	if err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	if approvalID == "" {
		t.Fatal("approval id must be returned")
	}
	approval, ok, err := store.Approval(ctx, "task-1", "fp-write-1")
	if err != nil || !ok {
		t.Fatalf("Approval() = ok %v, err %v", ok, err)
	}
	if approval.Decision != "approved" {
		t.Fatalf("decision = %q, want approved", approval.Decision)
	}
	// Re-deciding replaces the previous decision and keeps one row.
	if _, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-1", ActionID: actionID, Decision: "rejected", Reason: "changed my mind", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() re-decision error = %v", err)
	}
	if countRows(t, store, "approvals") != 1 {
		t.Fatal("re-decision must keep exactly one approval row per (task, action)")
	}
	approval, _, _ = store.Approval(ctx, "task-1", "fp-write-1")
	if approval.Decision != "rejected" {
		t.Fatalf("decision after re-decision = %q, want rejected", approval.Decision)
	}
	// An unapproved fingerprint has no approval.
	if _, ok, err := store.Approval(ctx, "task-1", "fp-never-approved"); err != nil || ok {
		t.Fatalf("missing approval lookup = ok %v, err %v", ok, err)
	}
}

func TestPrepareToolAttemptPersistsEffectAfterHash(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	afterHash := tools.HashBytes([]byte("content\n"))
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "write_file",
		Arguments:     []byte(`{"path":"a.txt","content":"content\n","expected_before_hash":"absent"}`),
		RecoveryClass: 2, EffectAfterHash: afterHash,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	var attempt *RecoveryToolAttempt
	for index := range snapshot.ToolAttempts {
		if snapshot.ToolAttempts[index].ExecutionID == executionID {
			attempt = &snapshot.ToolAttempts[index]
		}
	}
	if attempt == nil {
		t.Fatal("prepared attempt not in recovery snapshot")
	}
	if attempt.EffectAfterHash != afterHash {
		t.Fatalf("effect_after_hash = %q, want %q", attempt.EffectAfterHash, afterHash)
	}
	if attempt.RecoveryClass != 2 {
		t.Fatalf("recovery class = %d, want 2", attempt.RecoveryClass)
	}
}

func TestReconcileWriteAttemptPersistsCitableEvidence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "write_file",
		Arguments:     []byte(`{"path":"a.txt","content":"x\n","expected_before_hash":"absent"}`),
		RecoveryClass: 2, EffectAfterHash: tools.HashBytes([]byte("x\n")),
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	if err := store.ReconcileWriteAttempt(ctx, ReconcileWriteAttempt{
		TaskID: "task-1", ExecutionID: executionID, Status: "reconciled",
		Reason: "write_effect_completed", EvidenceID: "obs-000001",
		Evidence: tools.WriteEvidence{Path: "a.txt", BeforeHash: "absent", AfterHash: tools.HashBytes([]byte("x\n")), ChangeKind: "created", Outcome: tools.WriteSuccess},
	}); err != nil {
		t.Fatalf("ReconcileWriteAttempt() error = %v", err)
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].EvidenceID != "obs-000001" {
		t.Fatalf("reconciled write evidence not citable: %+v", snapshot.Evidence)
	}
	var action *RecoveryAction
	for index := range snapshot.Actions {
		if snapshot.Actions[index].ActionID == actionID {
			action = &snapshot.Actions[index]
		}
	}
	if action == nil || action.Status != "completed" {
		t.Fatalf("verified write must complete its action, status = %+v", action)
	}
}

// Write crash windows. The store crash helper performs a real filesystem
// write between TX 1 and TX 2, so the parent can prove which durable state
// and which workspace state survived each boundary.

func TestCrashStoreWriteHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_CRASH_STORE_HELPER") == "" {
		t.Skip("store crash helper")
	}
	point := os.Getenv("RUNSTEAD_CRASH_STORE_POINT")
	workspace := os.Getenv("RUNSTEAD_CRASH_STORE_WS")
	SetCrashPoint(func(name string) {
		if name == point {
			os.Exit(crashExitCode)
		}
	})
	store, err := Open(Options{Path: os.Getenv("RUNSTEAD_CRASH_STORE_DB"), Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-1", Objective: "o", Workspace: workspace}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-1"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "write_file",
		Arguments:     []byte(`{"path":"a.txt","content":"new\n","expected_before_hash":"absent"}`),
		RecoveryClass: 2, EffectAfterHash: tools.HashBytes([]byte("new\n")),
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	// The filesystem effect happens between TX 1 and TX 2.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write effect: %v", err)
	}
	observation := tools.Observation{
		ID: "obs-000001", Tool: "write_file", Success: true,
		Data:     tools.WriteEvidence{Path: "a.txt", BeforeHash: "absent", AfterHash: tools.HashBytes([]byte("new\n")), ChangeKind: "created", Outcome: tools.WriteSuccess},
		Metadata: tools.Metadata{Source: "write_file", Untrusted: true, Path: "a.txt", ExitCode: -1},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-1", ExecutionID: executionID, Status: "completed",
		EvidenceID: observation.ID, DurationNanos: 1, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
}

func crashWriteDBPath(t *testing.T, point string) (string, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "runstead.db")
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashStoreWriteHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_CRASH_STORE_HELPER=1",
		"RUNSTEAD_CRASH_STORE_DB="+dbPath,
		"RUNSTEAD_CRASH_STORE_POINT="+point,
		"RUNSTEAD_CRASH_STORE_WS="+workspace,
	)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != crashExitCode {
			t.Fatalf("crash helper exit error = %v", err)
		}
	}
	return dbPath, workspace
}

func TestCrashAfterWriteTX1BeforeEffectKeepsPreparedAndOldFile(t *testing.T) {
	dbPath, workspace := crashWriteDBPath(t, "tool_tx1_after")
	store := reopenCrashedStore(t, dbPath)

	var status, effectAfterHash string
	if err := store.db.QueryRow(`SELECT status, effect_after_hash FROM tool_attempts WHERE task_id = 'task-1'`).Scan(&status, &effectAfterHash); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("status = %q, want prepared", status)
	}
	if effectAfterHash != tools.HashBytes([]byte("new\n")) {
		t.Fatalf("effect_after_hash = %q, want the planned after-state", effectAfterHash)
	}
	// The effect never ran: the file must not exist.
	if _, err := os.Stat(filepath.Join(workspace, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("file must not exist after a pre-effect crash: %v", err)
	}
}

func TestCrashAfterWriteEffectBeforeTX2LeavesPreparedAndNewFile(t *testing.T) {
	dbPath, workspace := crashWriteDBPath(t, "tool_tx2_before")
	store := reopenCrashedStore(t, dbPath)

	var status, evidenceID string
	if err := store.db.QueryRow(`SELECT status, evidence_id FROM tool_attempts WHERE task_id = 'task-1'`).Scan(&status, &evidenceID); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if status != "prepared" {
		t.Fatalf("status = %q, want prepared (TX 2 not committed)", status)
	}
	if evidenceID != "" || countRows(t, store, "tool_results") != 0 {
		t.Fatal("no citable evidence may exist before TX 2")
	}
	// The effect DID run: the file exists with the new content.
	content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
	if err != nil {
		t.Fatalf("file must exist after a post-effect crash: %v", err)
	}
	if string(content) != "new\n" {
		t.Fatalf("file content = %q, want new\\n", content)
	}
}

func containsKind(kinds []string, kind string) bool {
	for _, existing := range kinds {
		if existing == kind {
			return true
		}
	}
	return false
}
