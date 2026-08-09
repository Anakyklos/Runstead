package state

import (
	"context"
	"encoding/json"
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
		PlannedEffect: tools.PlannedEffect{
			Diff: "--- a.txt\n+++ a.txt\n@@ -0,0 +1,1 @@\n+new\n",
		},
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

func TestCrashAfterWriteEffectPersistsPlannedDiffForReconciledEvidence(t *testing.T) {
	dbPath, workspace := crashWriteDBPath(t, "tool_tx2_before")
	store := reopenCrashedStore(t, dbPath)
	ctx := context.Background()

	// The TX 1 intent persisted the planned diff (bounded, sanitized).
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.ToolAttempts) != 1 {
		t.Fatalf("tool attempts = %d, want 1", len(snapshot.ToolAttempts))
	}
	attempt := snapshot.ToolAttempts[0]
	if attempt.PlannedEffectJSON == "" {
		t.Fatal("planned_diff_json must be persisted with the TX 1 intent")
	}
	var planned tools.PlannedEffect
	if err := json.Unmarshal([]byte(attempt.PlannedEffectJSON), &planned); err != nil {
		t.Fatalf("decode planned effect: %v", err)
	}
	if planned.Diff == "" {
		t.Fatal("planned diff must be persisted for crash-reconciled evidence")
	}

	// The effect completed on the filesystem but TX 2 never committed.
	// Reconcile from observable state: the current hash matches the expected
	// after-state, so the planned diff is promoted to reconciled evidence.
	reconciled := tools.ReconcileWrite(ctx, workspace, tools.WriteIntent{
		Tool:              attempt.Tool,
		Arguments:         []byte(attempt.ArgumentsJSON),
		ExpectedAfterHash: attempt.EffectAfterHash,
		PlannedEffect:     planned,
	})
	if reconciled.Status != tools.ReconcileCompleted {
		t.Fatalf("reconcile status = %q, want effect_completed", reconciled.Status)
	}
	if reconciled.Evidence.Diff != planned.Diff {
		t.Fatalf("reconciled evidence diff = %q, want the planned diff %q", reconciled.Evidence.Diff, planned.Diff)
	}
	if reconciled.Evidence.BeforeHash == "" || reconciled.Evidence.AfterHash == "" {
		t.Fatalf("reconciled evidence must carry before/after hashes: %+v", reconciled.Evidence)
	}

	// Persisting the reconciled evidence must keep the diff fields.
	if err := store.ReconcileWriteAttempt(ctx, ReconcileWriteAttempt{
		TaskID: "task-1", ExecutionID: attempt.ExecutionID, Status: "reconciled",
		Reason: "write_effect_completed", EvidenceID: "obs-000001", Evidence: reconciled.Evidence,
	}); err != nil {
		t.Fatalf("ReconcileWriteAttempt() error = %v", err)
	}
	after, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(after.Evidence) != 1 {
		t.Fatalf("reconciled evidence rows = %d, want 1", len(after.Evidence))
	}
	var persistedEvidence tools.WriteEvidence
	if err := json.Unmarshal([]byte(after.Evidence[0].DataJSON), &persistedEvidence); err != nil {
		t.Fatalf("decode persisted evidence: %v", err)
	}
	if persistedEvidence.Diff != planned.Diff || persistedEvidence.DiffBytes != planned.DiffBytes || persistedEvidence.DiffTruncated != planned.DiffTruncated {
		t.Fatalf("persisted reconciled evidence diff fields = %+v, want the planned effect %+v", persistedEvidence, planned)
	}
}

func TestMarkTaskApprovalRequiredKeepsTaskResumable(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	if _, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-write",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordWritePolicyDecision(ctx, WritePolicyDecision{
		TaskID: "task-1", ActionID: "action-000001", Tool: "write_file",
		Decision: "approval_required", Reason: "approval_required",
	}); err != nil {
		t.Fatalf("RecordWritePolicyDecision() error = %v", err)
	}
	if err := store.MarkTaskApprovalRequired(ctx, "task-1", "action-000001", "write approval required"); err != nil {
		t.Fatalf("MarkTaskApprovalRequired() error = %v", err)
	}
	status, err := store.TaskStatus(ctx, "task-1")
	if err != nil {
		t.Fatalf("TaskStatus() error = %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running (durably resumable)", status)
	}
	pending, err := store.PendingApprovals(ctx, "task-1")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ActionID != "action-000001" {
		t.Fatalf("pending approvals = %+v, want the paused write", pending)
	}
	if !containsKind(taskEventKinds(t, store, "task-1"), "task_approval_required") {
		t.Fatal("task_approval_required event must be journaled")
	}
}

func TestPendingApprovalsResolvedByOperatorDecision(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	if _, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-write",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordWritePolicyDecision(ctx, WritePolicyDecision{
		TaskID: "task-1", ActionID: "action-000001", Tool: "write_file",
		Decision: "approval_required", Reason: "approval_required",
	}); err != nil {
		t.Fatalf("RecordWritePolicyDecision() error = %v", err)
	}
	pending, err := store.PendingApprovals(ctx, "task-1")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1 before the decision", len(pending))
	}
	// An operator approval resolves the pending set.
	if _, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-1", ActionID: "action-000001", Decision: "approved", Reason: "ok", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	pending, err = store.PendingApprovals(ctx, "task-1")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0 after the operator decision", len(pending))
	}
}

func TestFinalizeTaskRefusesCompletedWithPendingApproval(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	if _, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "fp-write",
	}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordWritePolicyDecision(ctx, WritePolicyDecision{
		TaskID: "task-1", ActionID: "action-000001", Tool: "write_file",
		Decision: "approval_required", Reason: "approval_required",
	}); err != nil {
		t.Fatalf("RecordWritePolicyDecision() error = %v", err)
	}
	if err := store.MarkTaskApprovalRequired(ctx, "task-1", "action-000001", "waiting"); err != nil {
		t.Fatalf("MarkTaskApprovalRequired() error = %v", err)
	}
	err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"})
	if err == nil {
		t.Fatal("FinalizeTask must refuse completed while a write approval is pending")
	}
	if !errors.Is(err, ErrPendingApprovals) {
		t.Fatalf("error = %v, want ErrPendingApprovals", err)
	}
	// The task stays resumable.
	status, _ := store.TaskStatus(ctx, "task-1")
	if status != "running" {
		t.Fatalf("status = %q, want running", status)
	}
	// After the operator decides, completed is allowed.
	if _, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-1", ActionID: "action-000001", Decision: "approved", Reason: "ok", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	mustPassVerification(t, store, "task-1")
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("FinalizeTask() after approval error = %v", err)
	}
}

// TestRecipeApprovalBoundToRecipeFingerprint proves the approval identity of a
// run_recipe action is the digest-bound recipe fingerprint, never the plain
// repeat fingerprint: an approval for one effective recipe definition can
// never authorize a different definition of the same id (issue #26 review).
func TestRecipeApprovalBoundToRecipeFingerprint(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-recipe-fp")

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-recipe-fp", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`),
		Fingerprint: "fp-repeat", RecipeFingerprint: "fp-recipe-digest-v1",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if _, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-recipe-fp", ActionID: actionID, Decision: "approved", Reason: "ok", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	// The approval is keyed by the recipe fingerprint...
	approval, ok, err := store.Approval(ctx, "task-recipe-fp", "fp-recipe-digest-v1")
	if err != nil || !ok {
		t.Fatalf("Approval(recipe fp) = ok %v, err %v", ok, err)
	}
	if approval.Decision != "approved" {
		t.Fatalf("decision = %q, want approved", approval.Decision)
	}
	// ...NOT by the plain repeat fingerprint...
	if _, ok, err := store.Approval(ctx, "task-recipe-fp", "fp-repeat"); err != nil || ok {
		t.Fatalf("Approval(plain fp) = ok %v, err %v (must not match)", ok, err)
	}
	// ...and a different recipe definition (different digest) has no approval.
	if _, ok, err := store.Approval(ctx, "task-recipe-fp", "fp-recipe-digest-v2"); err != nil || ok {
		t.Fatalf("Approval(different digest) = ok %v, err %v (must not match)", ok, err)
	}
}

// TestPendingRecipeApprovalUsesRecipeFingerprint proves a pending run_recipe
// approval is derived from the digest-bound recipe fingerprint stored with the
// action: RecordApproval resolves the action's recipe_fingerprint (never the
// plain repeat fingerprint), the approval resolves the pending decision, and
// the task can then complete.
func TestPendingRecipeApprovalUsesRecipeFingerprint(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-recipe-pending")

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-recipe-pending", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`),
		Fingerprint: "fp-repeat", RecipeFingerprint: "fp-recipe-digest-v1",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordRecipePolicyDecision(ctx, RecipePolicyDecision{
		TaskID: "task-recipe-pending", ActionID: actionID, Recipe: "test",
		Decision: "approval_required", Reason: "recipe_policy",
	}); err != nil {
		t.Fatalf("RecordRecipePolicyDecision() error = %v", err)
	}
	pending, err := store.PendingApprovals(ctx, "task-recipe-pending")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ActionID != actionID {
		t.Fatalf("pending = %+v, want the pending recipe action", pending)
	}
	// The task cannot complete around the pending recipe approval.
	err = store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-recipe-pending", Outcome: "completed", StopReason: "done"})
	if !errors.Is(err, ErrPendingApprovals) {
		t.Fatalf("FinalizeTask() error = %v, want ErrPendingApprovals", err)
	}
	// RecordApproval keys the approval by the action's digest-bound recipe
	// fingerprint, which resolves the pending decision; the task can complete.
	if _, err := store.RecordApproval(ctx, Approval{
		TaskID: "task-recipe-pending", ActionID: actionID, Decision: "approved", Reason: "ok", Actor: "operator",
	}); err != nil {
		t.Fatalf("RecordApproval() error = %v", err)
	}
	pending, err = store.PendingApprovals(ctx, "task-recipe-pending")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the approval to resolve the pending decision", pending)
	}
	mustPassVerification(t, store, "task-recipe-pending")
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-recipe-pending", Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("FinalizeTask() after recipe approval error = %v", err)
	}
}
