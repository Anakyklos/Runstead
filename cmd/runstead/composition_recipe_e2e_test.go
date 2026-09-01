package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// TestProfileRecipeSurfaceExactAllowlistE2E proves the M10 recipe_ids blocker
// fix (issue #54 review) through the REAL agent loop with a scripted provider:
// the configured catalog contains go-test AND deploy, the Profile selects only
// go-test, so the model proposing deploy is rejected with unknown_recipe
// before any process starts, go-test stays available and executes through the
// existing policy, and inspect + the frozen contract expose only go-test.
func TestProfileRecipeSurfaceExactAllowlistE2E(t *testing.T) {
	workspace := t.TempDir()
	recipesFile := writeRecipesFile(t, `[
  {"id":"go-test","executable":"/bin/echo","argv":["go-test-ok"],"capabilities":["execute_repository_code"]},
  {"id":"deploy","executable":"/bin/echo","argv":["deploy-ran"],"capabilities":["execute_repository_code"]}
]`)
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"recipes","profile_version":"1.0.0","packages":[{"id":"process.recipes","version":"1.0.0"},{"id":"repo.read","version":"1.0.0"}],"recipe_ids":["go-test"]}`)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"deploy"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"go-test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"run_recipe"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run the selected recipe.",
		"--workspace", workspace,
		"--scripted", script,
		"--recipes", recipesFile,
		"--recipe-policy", "go-test=allow",
		"--profile", profile,
		"--acceptance", acceptanceRecipeFor(t, "go-test"),
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstderr:\n%s\nstdout:\n%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("run output:\n%s", out.String())
	}
	taskID := taskIDFromOutput(t, errOut.String())
	rendered := inspectRendered(t, stateDir, taskID)

	// Frozen contract + inspect surface: only go-test, with its own digest.
	if !strings.Contains(rendered, "recipe_catalog: digest=") || !strings.Contains(rendered, "ids=go-test") {
		t.Fatalf("inspect must show the effective recipe catalog ids=go-test:\n%s", rendered)
	}
	// deploy must never appear as an executed process, and never as part of
	// the effective catalog identity.
	if strings.Contains(rendered, "recipe=deploy") {
		t.Fatalf("deploy must never execute on the effective surface:\n%s", rendered)
	}
	// go-test executed exactly once through the existing policy path.
	if !strings.Contains(rendered, "recipe=go-test") || !strings.Contains(rendered, "execution=") {
		t.Fatalf("go-test must have executed with a durable execution row:\n%s", rendered)
	}
	// The contract hash is frozen and persisted.
	hash := contractHashFromInspect(t, rendered)
	if hash == "" {
		t.Fatal("frozen contract hash must be persisted")
	}
	// The deploy proposal was rejected BEFORE any effect: the store has no
	// execution row for deploy.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	count, err := store.RecipeExecutionCount(context.Background(), taskID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deploy executed %d times, want 0", count)
	}
}

// TestProfileRecipeSurfaceApprovalResumeDriftE2E proves the rest of the M10
// recipe surface contract through the real agent loop: the SELECTED recipe
// still passes through the normal policy/approval boundary (the Profile never
// authorizes execution), resume preserves the exact surface, and changing the
// recipe selection fails closed as drift before any recovery or dispatch.
func TestProfileRecipeSurfaceApprovalResumeDriftE2E(t *testing.T) {
	workspace := t.TempDir()
	recipesFile := writeRecipesFile(t, `[
  {"id":"go-test","executable":"/bin/echo","argv":["go-test-ok"],"capabilities":["execute_repository_code"]},
  {"id":"deploy","executable":"/bin/echo","argv":["deploy-ran"],"capabilities":["execute_repository_code"]}
]`)
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"recipes","profile_version":"1.0.0","packages":[{"id":"process.recipes","version":"1.0.0"}],"recipe_ids":["go-test"]}`)
	acceptance := acceptanceRecipeFor(t, "go-test")
	stateDir := t.TempDir()
	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"go-test"}}</runstead_action>`,
	)
	// Step 1: the SELECTED recipe still hits the normal policy gate and pauses
	// for operator approval. The Profile must not authorize execution.
	var runOut, runErr strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "Run the selected recipe.",
		"--workspace", workspace,
		"--scripted", runScript,
		"--recipes", recipesFile,
		"--profile", profile,
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}, &runOut, &runErr)
	if code == exitSuccess || !strings.Contains(runOut.String(), "outcome: approval_required") {
		t.Fatalf("run must pause for approval (Profile cannot authorize): exit=%d\nstdout:\n%s\nstderr:\n%s", code, runOut.String(), runErr.String())
	}
	taskID := taskIDFromOutput(t, runErr.String())
	pendingAction := pendingActionFromOutput(t, runOut.String())
	before := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(before, "ids=go-test") {
		t.Fatalf("frozen surface must be go-test only:\n%s", before)
	}
	frozenHash := contractHashFromInspect(t, before)

	// Step 2: changing the recipe selection (swap go-test for deploy) must
	// fail closed as drift BEFORE recovery journals anything or dispatch.
	driftProfile := writeCompositionProfile(t, `{"version":1,"profile_id":"recipes","profile_version":"1.0.0","packages":[{"id":"process.recipes","version":"1.0.0"}],"recipe_ids":["deploy"]}`)
	driftScript := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run"}</runstead_final>`)
	var driftOut, driftErr strings.Builder
	driftCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--profile", driftProfile,
		"--scripted", driftScript, "--recipes", recipesFile, "--acceptance", acceptance,
		"--log-level", "error",
	}, &driftOut, &driftErr)
	if driftCode != exitUsage || !strings.Contains(driftErr.String(), "profile composition drift") {
		t.Fatalf("drifted recipe selection = %d, want usage with explicit drift\nstderr:\n%s", driftCode, driftErr.String())
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.ResumeCount != 0 {
		t.Fatalf("drifted recipe selection must fail before recovery, resume count = %d", snapshot.Task.ResumeCount)
	}

	// Step 3: after operator approval, resume with the SAME profile rebuilds
	// the same surface: deploy is rejected, go-test executes, completion is
	// grounded in the approved recipe evidence, and the frozen hash is
	// unchanged.
	if decideCode, decideOut := runDecide(t, stateDir, taskID, pendingAction, "approved", "operator approved go-test"); decideCode != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", decideCode, decideOut)
	}
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"deploy"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"go-test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"run_recipe"}]}</runstead_final>`,
	)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--profile", profile,
		"--scripted", resumeScript, "--recipes", recipesFile, "--acceptance", acceptance,
		"--log-level", "error",
	}, &resumeOut, &resumeErr)
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s\nstdout:\n%s", resumeCode, resumeErr.String(), resumeOut.String())
	}
	if !strings.Contains(resumeOut.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", resumeOut.String())
	}
	after := inspectRendered(t, stateDir, taskID)
	if got := contractHashFromInspect(t, after); got != frozenHash {
		t.Fatalf("resume changed the frozen contract hash from %q to %q\n%s", frozenHash, got, after)
	}
	if strings.Contains(after, "recipe=deploy") {
		t.Fatalf("deploy must never execute on resume either:\n%s", after)
	}
	if !strings.Contains(after, "recipe=go-test") || !strings.Contains(after, "execution=") {
		t.Fatalf("approved go-test must have executed on resume:\n%s", after)
	}
}
