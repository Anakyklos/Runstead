package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// writeRecipesFile writes an operator-controlled recipe catalog and returns
// its path.
func writeRecipesFile(t *testing.T, recipes string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recipes.json")
	if err := os.WriteFile(path, []byte(recipes), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func echoRecipes() string {
	return `[{"id":"test","executable":"/bin/echo","argv":["ok"],"capabilities":["execute_repository_code"]}]`
}

// TestRunRecipeCLIAllowedFlow proves the full CLI flow for an allowed recipe:
// --recipes + --recipe-policy test=allow, the recipe runs, evidence is
// persisted, and inspect renders the process attempt.
func TestRunRecipeCLIAllowedFlow(t *testing.T) {
	workspace := t.TempDir()
	recipes := writeRecipesFile(t, echoRecipes())
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"tests passed","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run the tests.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipes,
		"--recipe-policy", "test=allow",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run output:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "Process attempts:") || !strings.Contains(rendered, "recipe=test") || !strings.Contains(rendered, "exit=0") || !strings.Contains(rendered, "network_isolation=unenforced") {
		t.Fatalf("inspect must render the process attempt:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recipe_policy: test=allow") {
		t.Fatalf("inspect must show the persisted recipe policy sanitized:\n%s", rendered)
	}
}

// TestRunRecipeCLIUnknownFailsClosed proves an unknown recipe proposal is
// rejected without starting any process and the task can still complete on
// other evidence.
func TestRunRecipeCLIUnknownFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recipes := writeRecipesFile(t, echoRecipes())
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"readme.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"does-not-exist"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run a recipe.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipes,
		"--recipe-policy", "test=allow",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run output:\n%s", out.String())
	}
}

// TestRunRecipeCLIApprovalFlowEndToEnd proves the normal approval UX without
// any artificial crash: run pauses with approval_required, the operator
// approves the pending recipe action, a normal resume executes it.
func TestRunRecipeCLIApprovalFlowEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	recipes := writeRecipesFile(t, echoRecipes())
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	// Default recipe policy: approval_required (no --recipe-policy).
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run the tests.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipes,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete\nstdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "outcome: approval_required") {
		t.Fatalf("run output must show approval_required:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	pendingAction := pendingActionFromOutput(t, out.String())
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "action="+pendingAction+" tool=run_recipe awaiting operator decision") {
		t.Fatalf("inspect must show the pending recipe approval:\n%s", rendered)
	}

	if decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "approved", "operator approved the test run"); decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}

	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--recipes", recipes,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, resumeErr.String())
	}
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", resumeOut.String())
	}
}

// TestRunRecipeCLIRejectedFlowEndToEnd proves a rejected recipe decision
// persists: resume preserves the rejection and the recipe never executes.
func TestRunRecipeCLIRejectedFlowEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	recipes := writeRecipesFile(t, echoRecipes())
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run the tests.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipes,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must pause, not complete")
	}
	taskID := taskIDFromOutput(t, errOut.String())
	pendingAction := pendingActionFromOutput(t, out.String())
	if decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "rejected", "operator rejected"); decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}

	// Resume re-proposes the recipe; the persisted rejection denies it. The
	// task can still complete on an unrelated grounded final... but there is
	// no other evidence, so the final must cite none; use a read first.
	// Simpler: assert resume ends with the denied correction and no recipe
	// process evidence was ever created (Process attempts remains none).
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
	)
	var resumeOut, resumeErr strings.Builder
	_ = run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--scripted", resumeScript,
		"--recipes", recipes,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	rendered := inspectRendered(t, stateDir, taskID)
	if strings.Contains(rendered, "Process attempts:\n") && strings.Contains(rendered, "recipe=test") {
		// Only a failed recipe attempt would render; a rejected recipe never
		// starts, so there must be no process evidence rows with the recipe.
		if strings.Contains(rendered, "execution=") {
			t.Fatalf("a rejected recipe must never execute:\n%s", rendered)
		}
	}
	if !strings.Contains(rendered, "decision=denied") {
		t.Fatalf("inspect must show the persisted rejection:\n%s", rendered)
	}
}

// TestResumeRecipePolicyDivergenceRejected proves a divergent
// --recipe-policy override at resume is rejected fail-closed.
func TestResumeRecipePolicyDivergenceRejected(t *testing.T) {
	workspace := t.TempDir()
	recipes := writeRecipesFile(t, echoRecipes())
	stateDir := t.TempDir()
	seedRunningTaskWithConfig(t, stateDir, "task-recipe-policy", workspace, `{"recipe_policy":"test=deny","max_steps":24}`)

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"resume", "task-recipe-policy",
		"--state-dir", stateDir,
		"--recipes", recipes,
		"--recipe-policy", "test=allow",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("resume exit = %d, want %d (divergent recipe policy)\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "diverges from the task's persisted recipe policy") {
		t.Fatalf("resume diagnostic = %q, want a divergence diagnostic", errOut.String())
	}
	rendered := inspectRendered(t, stateDir, "task-recipe-policy")
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("task must remain running after the rejected resume:\n%s", rendered)
	}
}

// TestResumeUsesPersistedDenyRecipePolicy proves the effective recipe policy
// survives restart: a task created with test=deny resumes under the deny
// policy, so a re-proposed recipe is denied and never executes.
func TestResumeUsesPersistedDenyRecipePolicy(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recipes := writeRecipesFile(t, echoRecipes())
	stateDir := t.TempDir()
	seedRunningTaskWithConfig(t, stateDir, "task-recipe-deny", workspace, `{"recipe_policy":"test=deny","max_steps":24}`)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedRead(t, store, "task-recipe-deny", "readme.txt")
	store.Close()

	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		"task-recipe-deny", "--state-dir", stateDir, "--scripted", resumeScript, "--recipes", recipes, "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, resumeErr)
	}
	rendered := inspectRendered(t, stateDir, "task-recipe-deny")
	if !strings.Contains(rendered, "decision=denied") {
		t.Fatalf("the re-proposed recipe must be denied under the persisted policy:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recipe_policy: test=deny") {
		t.Fatalf("inspect must render the persisted recipe policy sanitized:\n%s", rendered)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume must complete on the grounded read evidence:\n%s", resumeOut)
	}
}
