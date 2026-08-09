package state

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
