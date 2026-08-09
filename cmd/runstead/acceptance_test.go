package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// writeAcceptanceFile writes an operator acceptance plan and returns its path.
func writeAcceptanceFile(t *testing.T, plan string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acceptance.json")
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunAcceptanceRejectsMissingFile proves the full CLI flow: with an
// acceptance plan requiring a file that the model never creates, completion
// is refused by the runtime verifier; the failed verification is persisted
// and inspect explains the decision.
func TestRunAcceptanceRejectsMissingFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"artifact","type":"file_exists","path":"result.txt"}]}`)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Create result.txt.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must not complete when the acceptance check fails\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=failed") {
		t.Fatalf("inspect must show the failed verification:\n%s", rendered)
	}
	if !strings.Contains(rendered, "check=artifact type=file_exists status=failed") {
		t.Fatalf("inspect must show the failed artifact check:\n%s", rendered)
	}
	if strings.Contains(rendered, "Status: completed") {
		t.Fatalf("task must not be completed:\n%s", rendered)
	}
}

// TestRunAcceptancePassesWhenFileCreated proves the happy path: the model
// creates the required file, verification passes, and the task completes.
func TestRunAcceptancePassesWhenFileCreated(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"artifact","type":"file_exists","path":"result.txt"}]}`)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"result.txt","content":"ok\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"created","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"write_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Create result.txt.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptance,
		"--write-policy", "write_file=allow",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run must complete:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=passed") {
		t.Fatalf("inspect must show the passed verification:\n%s", rendered)
	}
}

// TestResumeRejectsAcceptancePlanDrift proves resume rejects a divergent
// --acceptance override fail-closed: the plan is persisted with the task at
// run start, so a task cannot continue under a different acceptance plan.
func TestResumeRejectsAcceptancePlanDrift(t *testing.T) {
	workspace := t.TempDir()
	planSpec := `{"version":1,"checks":[{"id":"a","type":"file_exists","path":"a.txt"}]}`
	plan, err := verifier.ParsePlan([]byte(planSpec))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: "task-plan", Objective: "o", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartTask(ctx, "task-plan"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAcceptancePlan(ctx, "task-plan", []byte(planSpec), plan.Digest()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	script := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	// A divergent plan is rejected before any recovery side effect.
	planB := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"a","type":"file_exists","path":"b.txt"}]}`)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", "task-plan",
		"--state-dir", stateDir,
		"--scripted", script,
		"--acceptance", planB,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitUsage {
		t.Fatalf("resume exit = %d, want %d (plan drift)\nstderr:\n%s", resumeCode, exitUsage, resumeErr.String())
	}
	if !strings.Contains(resumeErr.String(), "acceptance plan") {
		t.Fatalf("resume diagnostic = %q, want a plan drift diagnostic", resumeErr.String())
	}
}

// TestResumeAttachesAcceptancePlanToPlanlessTask proves the operator-owned
// continuation path of the fail-closed completion gate (issue #11 review): a
// task run without an acceptance plan can never complete (verification
// blocked), and the operator attaches a plan at resume with --acceptance. The
// plan is persisted with the task, so the same plan is authoritative from
// then on.
func TestResumeAttachesAcceptancePlanToPlanlessTask(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must not complete without an acceptance plan\n%s", out.String())
	}
	if !strings.Contains(out.String(), "outcome: verification_blocked") {
		t.Fatalf("run output must show verification_blocked:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "check=acceptance_criteria_required type=structural status=blocked") {
		t.Fatalf("inspect must show the blocked acceptance criteria check:\n%s", rendered)
	}
	if strings.Contains(rendered, "Status: completed") {
		t.Fatalf("task must not be completed:\n%s", rendered)
	}

	// The operator attaches the acceptance plan at resume; the resumed run
	// completes under it.
	acceptance := acceptanceFor(t, "readme.txt")
	resumeScript := writeScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--acceptance", acceptance,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr.String())
	}
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("resume must complete under the attached plan:\n%s", resumeOut.String())
	}
	// The attached plan is persisted: a second resume without the flag loads
	// it from state and stays consistent.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	spec, digest, ok, err := store.AcceptancePlan(context.Background(), taskID)
	store.Close()
	if err != nil || !ok {
		t.Fatalf("AcceptancePlan() = ok %v, err %v", ok, err)
	}
	plan, err := verifier.ParsePlan(spec)
	if err != nil {
		t.Fatalf("persisted attached plan must parse: %v", err)
	}
	if plan.Digest() != digest || plan.Digest() == "" {
		t.Fatalf("attached plan digest mismatch: %q vs %q", plan.Digest(), digest)
	}
}

// TestRunRejectsTypeIncompatibleEvidenceCitation proves Blocker 2 of the
// issue #11 review at the CLI level: the model reads a file and proposes
// complete citing that observation with a WRONG claimed tool (read_file
// evidence cited as run_recipe). The runtime verification fails with
// evidence_claims_typed, execution continues, and the corrected citation
// completes. The failed attempt is persisted and inspect explains it.
func TestRunRejectsTypeIncompatibleEvidenceCitation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptance := acceptanceFor(t, "readme.txt")
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"run_recipe"}]}</runstead_final>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d, want 0 (corrected citation must complete)\nstderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run output missing completed:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "decision=failed") {
		t.Fatalf("inspect must show the failed attempt:\n%s", rendered)
	}
	if !strings.Contains(rendered, "check=evidence_claims_typed type=structural status=failed") {
		t.Fatalf("inspect must show the typed mismatch check:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reason: evidence_type_mismatch") {
		t.Fatalf("inspect must show the mismatch reason:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: completed") {
		t.Fatalf("task must complete after the corrected citation:\n%s", rendered)
	}
}

// Issue #11 review blocker regression at the CLI level: the acceptance plan
// contains only file_exists(readme.txt), the model cites a legitimate
// read_file observation HONESTLY, and the final summary claims "tests passed"
// with no recipe evidence. The task completes (the acceptance check passes),
// but the model text must never appear as the verified summary: stdout and
// inspect carry the verifier-produced summary, and "tests passed" is only a
// labeled unverified note.
func TestRunModelTextNeverVerifiedSummary(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptance := acceptanceFor(t, "readme.txt")
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"tests passed","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--scripted", script,
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d, want 0\nstderr:\n%s", code, errOut.String())
	}
	if strings.Contains(out.String(), "summary: tests passed") {
		t.Fatalf("the model claim must never be the verified summary:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "summary: completion verified: acceptance check passed (artifact)") {
		t.Fatalf("stdout must carry the verifier-produced summary:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "note (unverified): tests passed") {
		t.Fatalf("stdout must carry the model text as a labeled unverified note:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if strings.Contains(rendered, "Summary: tests passed") {
		t.Fatalf("the model claim must never be the persisted task summary:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Summary: completion verified: acceptance check passed (artifact)") {
		t.Fatalf("inspect must render the verified summary:\n%s", rendered)
	}
}
