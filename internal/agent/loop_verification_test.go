package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// verifierLoop builds a loop with a plan-based control-plane verifier over the
// harness's real store and registry.
func verifierLoop(t *testing.T, h *writeHarness, plan *verifier.Plan, limits agent.Limits, recovery *agent.RecoverySeed) *agent.Loop {
	t.Helper()
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               limits,
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, plan),
		AcceptancePlanDigest: planDigest(plan),
		Recovery:             recovery,
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	return loop
}

func planDigest(plan *verifier.Plan) string {
	if plan == nil {
		return ""
	}
	return plan.Digest()
}

// Issue #11 review blocker E2E: the objective requires creating a file; the
// model only reads an existing file and proposes complete. Without an operator
// acceptance plan, completion is refused blocked and the task can never reach
// completed. The task stays durably resumable so the operator can attach a
// plan at resume.
func TestLoopVerificationWithoutAcceptancePlanNeverCompletes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Created result.txt.", finalEvidence("obs-000001", "read_file")),
	)
	loop := verifierLoop(t, h, nil, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, nil)
	result := loop.Run(context.Background(), agent.Task{ID: "task-verify-noplan", Prompt: "Create result.txt."})
	if result.Outcome == agent.OutcomeCompleted {
		t.Fatalf("task must never complete without an acceptance plan: %+v", result)
	}
	if result.Outcome != agent.OutcomeVerificationBlocked {
		t.Fatalf("outcome = %s, want verification_blocked", result.Outcome)
	}
	status, _ := h.store.TaskStatus(context.Background(), "task-verify-noplan")
	if status == "completed" {
		t.Fatal("task status must not be completed")
	}
	if status != "running" {
		t.Fatalf("task status = %q, want running (durably resumable so the operator can attach a plan)", status)
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-verify-noplan")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Decision != "blocked" {
		t.Fatalf("attempts = %+v, want one blocked", attempts)
	}
	var planCheck bool
	for _, check := range attempts[0].Checks {
		if check.CheckID == "acceptance_criteria_required" && check.Status == "blocked" && check.Reason == "acceptance_plan_missing" {
			planCheck = true
		}
	}
	if !planCheck {
		t.Fatalf("blocked attempt must carry the acceptance criteria check: %+v", attempts[0].Checks)
	}
}

// Scenario 13: a failed verification is presented as a structured observation
// and the loop continues; after a real correction the next verification passes
// (scenario 14).
func TestLoopVerificationFailedThenCorrectedPasses(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "answer-file", Type: verifier.CheckFileHash, Path: "answer.txt", SHA256: tools.HashBytes([]byte("42\n")),
	}}}
	// Turn 1: model proposes complete without creating answer.txt -> failed
	// verification, the loop continues. Turn 2: model writes answer.txt.
	// Turn 3: model proposes complete again -> verification passes.
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file")),
		actionResponse("write_file", `{"path":"answer.txt","content":"42\n","expected_before_hash":"absent"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "write_file")),
	)
	loop := verifierLoop(t, h, plan, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, nil)
	result := loop.Run(context.Background(), testTask("task-verify-correct"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-verify-correct")
	if err != nil {
		t.Fatalf("VerificationAttempts() error = %v", err)
	}
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 verification attempts, got %d", len(attempts))
	}
	if attempts[0].Decision != "failed" || attempts[len(attempts)-1].Decision != "passed" {
		t.Fatalf("attempts = %+v, want failed then passed", attempts)
	}
	// The failed verification was persisted as authoritative history with the
	// failed check reason.
	var failedCheck bool
	for _, check := range attempts[0].Checks {
		if check.CheckID == "answer-file" && check.Status == "failed" && check.Reason == "file_not_found" {
			failedCheck = true
		}
	}
	if !failedCheck {
		t.Fatalf("failed attempt must carry the typed failed check: %+v", attempts[0].Checks)
	}
}

// Scenario 1 at the loop level: the model claims a file was created but it
// does not exist; the loop keeps running until the step budget stops it and
// the task is never completed.
func TestLoopVerificationClaimedFileMissingNeverCompletes(t *testing.T) {
	workspace := t.TempDir()
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "artifact", Type: verifier.CheckFileExists, Path: "missing.txt",
	}}}
	// The model keeps proposing complete; the file never appears.
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Created the file.", finalEvidence("obs-000001", "read_file")),
	)
	loop := verifierLoop(t, h, plan, agent.Limits{MaxSteps: 2, MaxCorrections: 3, MaxRepeatedActions: 3}, nil)
	result := loop.Run(context.Background(), testTask("task-verify-missing"))
	if result.Outcome == agent.OutcomeCompleted {
		t.Fatalf("task must never complete: %+v", result)
	}
	status, _ := h.store.TaskStatus(context.Background(), "task-verify-missing")
	if status == "completed" {
		t.Fatal("task status must not be completed")
	}
}

// Scenario 10 at the loop level: an uncertain write attempt blocks completion
// with the typed verification outcome; the task stays resumable.
func TestLoopVerificationUncertainAttemptBlocks(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file")),
	)
	ctx := context.Background()
	// Pre-create the task and record an action with a prepared (uncertain)
	// write attempt, then run a loop seeded to skip task bootstrap.
	if err := h.store.CreateTask(ctx, state.TaskRecord{TaskID: "task-verify-uncertain", Objective: "o", Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.StartTask(ctx, "task-verify-uncertain"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RecordAction(ctx, state.ActionRecord{TaskID: "task-verify-uncertain", Tool: tools.ToolWriteFile, Arguments: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: "task-verify-uncertain", ActionID: "action-000001", Tool: tools.ToolWriteFile,
		Arguments: []byte(`{}`), RecoveryClass: 2,
	}); err != nil {
		t.Fatal(err)
	}
	loop := verifierLoop(t, h, nil, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, &agent.RecoverySeed{})
	result := loop.Run(ctx, testTask("task-verify-uncertain"))
	if result.Outcome != agent.OutcomeVerificationBlocked {
		t.Fatalf("outcome = %s, want verification_blocked (reason %s)", result.Outcome, result.StopReason)
	}
	status, _ := h.store.TaskStatus(ctx, "task-verify-uncertain")
	if status == "completed" {
		t.Fatal("task must not be completed with an uncertain attempt")
	}
	if status != "running" {
		t.Fatalf("task status = %q, want running (durably resumable)", status)
	}
}

// Scenario 12 at the loop level with a recipe: tests pass through a real
// executed recipe (exit 0) and the acceptance check passes; completion is
// accepted.
func TestLoopVerificationRecipePassCompletes(t *testing.T) {
	workspace := t.TempDir()
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "tests-pass", Type: verifier.CheckRecipeExitZero, Recipe: "test",
	}}}
	catalog := testCatalog(t, testRecipe("test"))
	h := newRecipeHarness(t, workspace, allowAllPolicy(), map[string]policy.Mode{"test": policy.ModeAllow}, catalog, &fakeRunner{
		results: []recipe.Result{{Started: true, ExitCode: 0}},
	},
		actionResponse("run_recipe", `{"recipe":"test"}`),
		finalResponse("complete", "tests passed", finalEvidence("obs-000001", "run_recipe")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, plan),
		AcceptancePlanDigest: plan.Digest(),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(context.Background(), testTask("task-verify-recipe"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-verify-recipe")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Decision != "passed" {
		t.Fatalf("attempts = %+v, want one passed", attempts)
	}
	var recipeCheck bool
	for _, check := range attempts[0].Checks {
		if check.CheckID == "tests-pass" && check.Status == "passed" && len(check.Evidence) == 1 && check.Evidence[0] == "obs-000001" {
			recipeCheck = true
		}
	}
	if !recipeCheck {
		t.Fatalf("recipe check must pass with the executed evidence: %+v", attempts[0].Checks)
	}
}

// Scenario 5 at the loop level: the test recipe runs and fails (non-zero);
// the loop continues with the structured verification observation.
func TestLoopVerificationRecipeFailsContinues(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "tests-pass", Type: verifier.CheckRecipeExitZero, Recipe: "test",
	}}}
	catalog := testCatalog(t, testRecipe("test"))
	h := newRecipeHarness(t, workspace, allowAllPolicy(), map[string]policy.Mode{"test": policy.ModeAllow}, catalog, &fakeRunner{
		results: []recipe.Result{{Started: true, ExitCode: 1}},
	},
		actionResponse("read_file", `{"path":"readme.txt"}`),
		actionResponse("run_recipe", `{"recipe":"test"}`),
		finalResponse("complete", "tests passed", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "run_recipe")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, plan),
		AcceptancePlanDigest: plan.Digest(),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(context.Background(), testTask("task-verify-recipe-fail"))
	if result.Outcome == agent.OutcomeCompleted {
		t.Fatal("a failing recipe must never complete the task")
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-verify-recipe-fail")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Decision != "failed" {
		t.Fatalf("attempts = %+v, want one failed", attempts)
	}
	var recipeCheck bool
	for _, check := range attempts[0].Checks {
		if check.CheckID == "tests-pass" && check.Status == "failed" && check.Reason == "recipe_exit_nonzero" {
			recipeCheck = true
		}
	}
	if !recipeCheck {
		t.Fatalf("recipe check must fail with exit reason: %+v", attempts[0].Checks)
	}
}

// Scenario 15: resume keeps the verification state; the recovery context
// surfaces the pending verification failure and a resumed run can complete
// after the real correction.
func TestLoopVerificationStateSurvivesResume(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "answer", Type: verifier.CheckFileHash, Path: "answer.txt", SHA256: tools.HashBytes([]byte("42\n")),
	}}}
	// Run 1: model proposes complete without the file -> verification failed.
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file")),
	)
	loop := verifierLoop(t, h, plan, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, nil)
	ctx := context.Background()
	_ = loop.Run(ctx, testTask("task-verify-resume"))
	status, _ := h.store.TaskStatus(ctx, "task-verify-resume")
	if status == "completed" {
		t.Fatal("task must not be completed after a failed verification")
	}
	// The recovery context built from the persisted snapshot surfaces the
	// failed verification.
	snapshot, err := h.store.LoadRecoverySnapshot(ctx, "task-verify-resume")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.VerificationAttempts) != 1 || snapshot.VerificationAttempts[0].Decision != "failed" {
		t.Fatalf("verification state must survive: %+v", snapshot.VerificationAttempts)
	}
	// Run 2 (resumed): the model writes the file and proposes complete again.
	secondProvider := &scriptedProvider{clock: h.clock, pace: time.Millisecond, responses: []provider.Response{
		actionResponse("write_file", `{"path":"answer.txt","content":"42\n","expected_before_hash":"absent"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "write_file")),
	}}
	executor2, err := agent.NewExecutor(h.governor, secondProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Run 1 consumed three provider attempts: read, final (failed
	// verification), and the exhausted provider after the loop continued.
	seed := &agent.RecoverySeed{
		Turns: 3, Attempts: 3,
		Evidence: observationsFromSnapshot(snapshot.Evidence),
	}
	second, err := agent.NewLoop(agent.Config{
		Runner:               executor2,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, plan),
		AcceptancePlanDigest: plan.Digest(),
		Recovery:             seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := second.Run(ctx, testTask("task-verify-resume"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("resumed run outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	attempts, err := h.store.VerificationAttempts(ctx, "task-verify-resume")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Decision != "failed" || attempts[1].Decision != "passed" {
		t.Fatalf("attempts = %+v, want failed then passed", attempts)
	}
}

// Scenario 16: the verifier report is persisted and `runstead inspect`
// explains the failed decision with the per-check reason.
func TestLoopVerificationInspectExplainsDecision(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "readme.txt"), []byte("info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "artifact", Type: verifier.CheckFileExists, Path: "never.txt",
	}}}
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "Done.", finalEvidence("obs-000001", "read_file")),
	)
	loop := verifierLoop(t, h, plan, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, nil)
	_ = loop.Run(context.Background(), testTask("task-verify-inspect"))
	var out strings.Builder
	if err := h.store.RenderInspect(context.Background(), &out, "task-verify-inspect"); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "\nVerification:\n") || !strings.Contains(rendered, "decision=failed") {
		t.Fatalf("inspect must render the verification section with the failed decision:\n%s", rendered)
	}
	if !strings.Contains(rendered, "check=artifact type=file_exists status=failed") {
		t.Fatalf("inspect must render the failed check:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reason: file_not_found") {
		t.Fatalf("inspect must render the typed reason:\n%s", rendered)
	}
}

// observationsFromSnapshot converts persisted recovery evidence rows back into
// tools.Observation values for seeding a resumed loop's grounding set.
func observationsFromSnapshot(items []state.RecoveryEvidence) []tools.Observation {
	observations := make([]tools.Observation, 0, len(items))
	for _, item := range items {
		observations = append(observations, tools.Observation{
			ID:      item.EvidenceID,
			Tool:    item.Tool,
			Success: true,
			Metadata: tools.Metadata{
				Source:    item.Tool,
				Untrusted: true,
			},
		})
	}
	return observations
}
