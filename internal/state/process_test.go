package state

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

func TestPrepareToolAttemptPersistsProcessIntent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), Fingerprint: "fp-recipe"})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	processIntent := []byte(`{"recipe_id":"test","executable":"go","argv":["test","./..."],"capabilities":["execute_repository_code"],"timeout_nanos":60000000000}`)
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "run_recipe",
		Arguments: []byte(`{"recipe":"test"}`), RecoveryClass: 4, ProcessIntent: processIntent,
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
	if attempt.RecoveryClass != 4 {
		t.Fatalf("recovery class = %d, want 4", attempt.RecoveryClass)
	}
	if attempt.ProcessIntentJSON == "" {
		t.Fatal("process_intent_json must be persisted at TX 1")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(attempt.ProcessIntentJSON), &decoded); err != nil {
		t.Fatalf("decode process intent: %v", err)
	}
	if decoded["recipe_id"] != "test" || decoded["executable"] != "go" {
		t.Fatalf("process intent = %v", decoded)
	}
}

func TestRecordRecipePolicyDecisionPersistsWithEvent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	if _, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), Fingerprint: "fp-recipe"}); err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if err := store.RecordRecipePolicyDecision(ctx, RecipePolicyDecision{
		TaskID: "task-1", ActionID: "action-000001", Recipe: "test", Decision: "denied", Reason: "policy_deny",
	}); err != nil {
		t.Fatalf("RecordRecipePolicyDecision() error = %v", err)
	}
	if !containsKind(taskEventKinds(t, store, "task-1"), "recipe_policy_decision") {
		t.Fatal("recipe_policy_decision event must be journaled")
	}
	// The pending query must surface an approval_required recipe decision.
	if err := store.RecordRecipePolicyDecision(ctx, RecipePolicyDecision{
		TaskID: "task-1", ActionID: "action-000001", Recipe: "test", Decision: "approval_required", Reason: "approval_required",
	}); err != nil {
		t.Fatalf("RecordRecipePolicyDecision() error = %v", err)
	}
	pending, err := store.PendingApprovals(ctx, "task-1")
	if err != nil {
		t.Fatalf("PendingApprovals() error = %v", err)
	}
	if len(pending) != 1 || pending[0].Tool != "run_recipe" {
		t.Fatalf("pending = %+v, want the run_recipe action", pending)
	}
}

func TestCompleteRecipeAttemptPersistsProcessEvidence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), Fingerprint: "fp-recipe"})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "run_recipe",
		Arguments: []byte(`{"recipe":"test"}`), RecoveryClass: 4,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	observation := tools.Observation{
		ID: "obs-000001", Tool: "run_recipe", Success: true,
		Data: map[string]any{
			"recipe_id": "test", "exit_code": 0, "stdout": "ok\n",
			"stdout_bytes": 3, "network_isolation": "unenforced",
		},
		Metadata: tools.Metadata{Source: "run_recipe", Untrusted: true, ExitCode: 0},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-1", ExecutionID: executionID, Status: "completed",
		EvidenceID: observation.ID, DurationNanos: 1000, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
	// The completed attempt is terminal verified progress in recovery.
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(snapshot.Evidence) != 1 || snapshot.Evidence[0].EvidenceID != "obs-000001" {
		t.Fatalf("process evidence not citable: %+v", snapshot.Evidence)
	}
}

func TestInspectRendersProcessAttempts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), Fingerprint: "fp-recipe"})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "run_recipe",
		Arguments: []byte(`{"recipe":"test"}`), RecoveryClass: 4,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	observation := tools.Observation{
		ID: "obs-000001", Tool: "run_recipe", Success: true,
		Data: map[string]any{
			"recipe_id": "test", "exit_code": 4, "signal": "killed",
			"stdout_truncated": true, "network_isolation": "unenforced",
		},
		Metadata: tools.Metadata{Source: "run_recipe", Untrusted: true, ExitCode: 4},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-1", ExecutionID: executionID, Status: "completed",
		EvidenceID: observation.ID, DurationNanos: 5000, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
	var builder strings.Builder
	if err := store.RenderInspect(ctx, &builder, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := builder.String()
	if !strings.Contains(rendered, "Process attempts:") || !strings.Contains(rendered, "recipe=test exit=4 evidence=obs-000001") || !strings.Contains(rendered, "signal=killed") || !strings.Contains(rendered, "truncated=stdout:true/stderr:false") || !strings.Contains(rendered, "network_isolation=unenforced") {
		t.Fatalf("inspect must render process evidence:\n%s", rendered)
	}
}
