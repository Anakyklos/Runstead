package state

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TestSaveVerificationAttemptPersistsProjectionAndChecks proves one
// verification attempt (projection + per-check results) and its journal event
// commit atomically.
func TestSaveVerificationAttemptPersistsProjectionAndChecks(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	report, err := json.Marshal(map[string]any{"task_id": "task-1", "decision": "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "failed", Summary: "completion refused", ReportJSON: report,
		Checks: []VerificationCheckRecord{
			{CheckID: "evidence_grounded", Type: "structural", Status: "passed"},
			{CheckID: "artifact", Type: "file_exists", Status: "failed", Expected: "file exists", Observed: "absent", Reason: "file_not_found"},
		},
	}); err != nil {
		t.Fatalf("SaveVerificationAttempt() error = %v", err)
	}
	attempts, err := store.VerificationAttempts(ctx, "task-1")
	if err != nil {
		t.Fatalf("VerificationAttempts() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0].AttemptID == "" || attempts[0].Decision != "failed" {
		t.Fatalf("attempts = %+v", attempts)
	}
	if len(attempts[0].Checks) != 2 {
		t.Fatalf("checks = %+v", attempts[0].Checks)
	}
	var artifactCheck *VerificationCheckRow
	for index := range attempts[0].Checks {
		if attempts[0].Checks[index].CheckID == "artifact" {
			artifactCheck = &attempts[0].Checks[index]
		}
	}
	if artifactCheck == nil || artifactCheck.Status != "failed" || artifactCheck.Reason != "file_not_found" {
		t.Fatalf("artifact check = %+v", artifactCheck)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsKind(kinds, "verification_recorded") {
		t.Fatalf("journal missing verification_recorded: %v", kinds)
	}
}

// TestFinalizeTaskRequiresPassedVerification proves the completion gate at the
// state layer: completed is refused without a verification attempt, refused
// when the latest attempt did not pass, and allowed after a passed attempt.
func TestFinalizeTaskRequiresPassedVerification(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	// No verification -> refused.
	err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"})
	if !errors.Is(err, ErrVerificationRequired) {
		t.Fatalf("FinalizeTask() error = %v, want ErrVerificationRequired", err)
	}
	// Latest verification failed -> refused.
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "failed", Summary: "nope", ReportJSON: []byte(`{"decision":"failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	err = store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"})
	if !errors.Is(err, ErrVerificationNotPassed) {
		t.Fatalf("FinalizeTask() error = %v, want ErrVerificationNotPassed", err)
	}
	// A later passed verification allows completion.
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "passed", Summary: "ok", ReportJSON: []byte(`{"decision":"passed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("FinalizeTask() after passed verification error = %v", err)
	}
	status, _ := store.TaskStatus(ctx, "task-1")
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}
}

// TestAcceptancePlanAndBaselinePersist proves the operator acceptance plan and
// the workspace git baseline are persisted with the task and survive reload.
func TestAcceptancePlanAndBaselinePersist(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	spec := []byte(`{"version":1,"checks":[{"id":"a","type":"file_exists","path":"x.txt"}]}`)
	digest := "plan-digest-1"
	if err := store.SaveAcceptancePlan(ctx, "task-1", spec, digest); err != nil {
		t.Fatalf("SaveAcceptancePlan() error = %v", err)
	}
	loaded, loadedDigest, ok, err := store.AcceptancePlan(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("AcceptancePlan() = ok %v, err %v", ok, err)
	}
	if string(loaded) != string(spec) || loadedDigest != digest {
		t.Fatalf("plan = %s digest=%s", loaded, loadedDigest)
	}
	if err := store.SaveWorkspaceBaseline(ctx, "task-1", " M a.txt\n", "diff", true, false); err != nil {
		t.Fatalf("SaveWorkspaceBaseline() error = %v", err)
	}
	status, diff, statusTruncated, diffTruncated, ok, err := store.WorkspaceBaseline(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("WorkspaceBaseline() = ok %v, err %v", ok, err)
	}
	if status != " M a.txt\n" || diff != "diff" {
		t.Fatalf("baseline = %q/%q", status, diff)
	}
	if !statusTruncated || diffTruncated {
		t.Fatalf("baseline truncation flags = %t/%t, want true/false", statusTruncated, diffTruncated)
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AcceptancePlanSpec != string(spec) || snapshot.AcceptancePlanDigest != digest {
		t.Fatalf("snapshot plan = %q/%q", snapshot.AcceptancePlanSpec, snapshot.AcceptancePlanDigest)
	}
	if snapshot.BaselineGitStatus != " M a.txt\n" {
		t.Fatalf("snapshot baseline = %q", snapshot.BaselineGitStatus)
	}
	if !snapshot.BaselineGitStatusTruncated || snapshot.BaselineGitDiffTruncated {
		t.Fatalf("snapshot baseline truncation flags = %t/%t, want true/false", snapshot.BaselineGitStatusTruncated, snapshot.BaselineGitDiffTruncated)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsKind(kinds, "acceptance_plan_saved") || !containsKind(kinds, "workspace_baseline_saved") {
		t.Fatalf("journal missing plan/baseline events: %v", kinds)
	}
}

// TestMarkTaskVerificationPausedKeepsTaskResumable proves the verification
// pause leaves the task durably resumable (status running) with the typed
// outcome and a journal event; no terminal finalize happens.
func TestMarkTaskVerificationPausedKeepsTaskResumable(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	if err := store.MarkTaskVerificationPaused(ctx, "task-1", "uncertain effect blocks completion"); err != nil {
		t.Fatalf("MarkTaskVerificationPaused() error = %v", err)
	}
	status, _ := store.TaskStatus(ctx, "task-1")
	if status != "running" {
		t.Fatalf("status = %q, want running", status)
	}
	kinds := taskEventKinds(t, store, "task-1")
	if !containsKind(kinds, "task_verification_paused") {
		t.Fatalf("journal missing task_verification_paused: %v", kinds)
	}
}

// TestRenderInspectShowsVerification proves `runstead inspect` renders the
// verification section with the decision and per-check results.
func TestRenderInspectShowsVerification(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "failed", Summary: "completion refused: 1 check(s) failed", ReportJSON: []byte(`{}`),
		Checks: []VerificationCheckRecord{{
			CheckID: "artifact", Type: "file_exists", Status: "failed",
			Expected: "file exists at x.txt", Observed: "absent", Reason: "file_not_found",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := store.RenderInspect(ctx, &out, "task-1"); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "\nVerification:\n") {
		t.Fatalf("inspect missing Verification section:\n%s", rendered)
	}
	if !strings.Contains(rendered, "decision=failed") {
		t.Fatalf("inspect missing failed decision:\n%s", rendered)
	}
	if !strings.Contains(rendered, "check=artifact type=file_exists status=failed") {
		t.Fatalf("inspect missing failed check:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reason: file_not_found") {
		t.Fatalf("inspect missing typed reason:\n%s", rendered)
	}
}

func TestRenderInspectShowsPersistedGitDiff(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	report := []byte(`{"task_id":"task-1","decision":"passed","summary":"completion verified","git":{"available":true,"current_status":" M app/calc.go\n","current_diff":"diff --git a/app/calc.go b/app/calc.go\n+fixed\n","truncated":false,"pre_existing":[],"during_task":[{"path":"app/calc.go","status":" M"}]}}`)
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "passed", Summary: "completion verified", ReportJSON: report,
		Checks: []VerificationCheckRecord{{CheckID: "fix-hash", Type: "file_hash", Status: "passed"}},
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := store.RenderInspect(ctx, &out, "task-1"); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	for _, want := range []string{
		"Git diff (bounded):",
		"+fixed",
		"diff truncated: false",
		"during-task changes: app/calc.go ( M)",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("inspect missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderFinalRequiresCompletedPassedTask(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	mustTask(t, store, "task-running")
	var runningOut strings.Builder
	if err := store.RenderFinal(ctx, &runningOut, "task-running"); err == nil {
		t.Fatal("RenderFinal() must refuse a running task")
	}
	if strings.Contains(runningOut.String(), "Verified runtime result:") {
		t.Fatalf("running task must not render a verified result:\n%s", runningOut.String())
	}

	mustTask(t, store, "task-failed")
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-failed", Outcome: "provider_failure", StopReason: "provider failed"}); err != nil {
		t.Fatal(err)
	}
	var failedOut strings.Builder
	if err := store.RenderFinal(ctx, &failedOut, "task-failed"); err == nil {
		t.Fatal("RenderFinal() must refuse a failed task")
	}
	if strings.Contains(failedOut.String(), "Verified runtime result:") {
		t.Fatalf("failed task must not render a verified result:\n%s", failedOut.String())
	}

	mustTask(t, store, "task-blocked")
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-blocked", Decision: "blocked", Summary: "completion refused", ReportJSON: []byte(`{"decision":"blocked"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskVerificationPaused(ctx, "task-blocked", "acceptance plan missing"); err != nil {
		t.Fatal(err)
	}
	var blockedOut strings.Builder
	if err := store.RenderFinal(ctx, &blockedOut, "task-blocked"); err == nil {
		t.Fatal("RenderFinal() must refuse a blocked task")
	}
	if strings.Contains(blockedOut.String(), "Verified runtime result:") {
		t.Fatalf("blocked task must not render a verified result:\n%s", blockedOut.String())
	}
}

func TestRenderFinalShowsAuthoritativeProjection(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), Fingerprint: "recipe-fingerprint",
	})
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "run_recipe", Arguments: []byte(`{"recipe":"test"}`), RecoveryClass: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := tools.Observation{
		ID: "obs-000001", Tool: "run_recipe", Success: true,
		Data: map[string]any{
			"recipe_id": "test", "exit_code": 0, "stdout_truncated": false, "stderr_truncated": false,
			"network_isolation": "unenforced",
		},
		Metadata: tools.Metadata{Source: "run_recipe", Untrusted: true, ExitCode: 0},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-1", ExecutionID: executionID, Status: "completed", EvidenceID: observation.ID,
		DurationNanos: 1000, Observation: observation,
	}); err != nil {
		t.Fatal(err)
	}

	report := []byte(`{"task_id":"task-1","decision":"passed","summary":"completion verified: acceptance check passed (tests-pass)","git":{"available":true,"current_status":" M app/calc.go\n","current_diff":"diff --git a/app/calc.go b/app/calc.go\n+fixed\n","truncated":false,"pre_existing":[],"during_task":[{"path":"app/calc.go","status":" M"}]}}`)
	if err := store.SaveVerificationAttempt(ctx, VerificationAttemptRecord{
		TaskID: "task-1", Decision: "passed", Summary: "completion verified: acceptance check passed (tests-pass)", ReportJSON: report,
		Checks: []VerificationCheckRecord{{CheckID: "tests-pass", Type: "recipe_exit_zero", Status: "passed", Evidence: []string{"obs-000001"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: "task-1", Outcome: "completed", StopReason: "verification passed", Summary: "completion verified: acceptance check passed (tests-pass)", Evidence: []string{"obs-000001"}}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := store.RenderFinal(ctx, &out, "task-1"); err != nil {
		t.Fatalf("RenderFinal() error = %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"Verified runtime result:",
		"status: completed",
		"outcome: completed",
		"verifier: passed",
		"check=tests-pass type=recipe_exit_zero status=passed",
		"obs-000001 tool=run_recipe",
		"recipe=test exit=0 evidence=obs-000001",
		"during-task changes: app/calc.go ( M)",
		"Git diff (bounded):",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("final output missing %q:\n%s", want, rendered)
		}
	}
}
