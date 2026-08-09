package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// The recipe harness already carries everything the coding-loop tests need:
// real store, real registry, fake clock, scripted provider and fake runner.
// The loop-level tests build the loop directly from it.

// fixtureFile writes one fixture file into the workspace and returns its
// content.
func fixtureFile(t *testing.T, workspace, name, content string) string {
	t.Helper()
	path := filepath.Join(workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return content
}

// codingFixtureRoot is the committed deterministic fixture of the #12
// coding-loop scenario. Go tests run with the package directory as the
// working directory, so ../../fixtures/coding-loop resolves from
// internal/agent to the repository root.
const codingFixtureRoot = "../../fixtures/coding-loop"

// fixtureContent reads one committed fixture file.
func fixtureContent(t *testing.T, relative string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(codingFixtureRoot, relative))
	if err != nil {
		t.Fatalf("read coding-loop fixture %s: %v", relative, err)
	}
	return string(content)
}

// calcInitial is the initial buggy implementation of the fixture.
func calcInitial(t *testing.T) string {
	t.Helper()
	return fixtureContent(t, "app/calc.go")
}

// calcWrongFix is the deterministic first (insufficient) fix attempt.
func calcWrongFix(t *testing.T) string {
	t.Helper()
	return fixtureContent(t, "fixes/calc-wrong.go")
}

// calcCorrectFix is the deterministic corrective fix.
func calcCorrectFix(t *testing.T) string {
	t.Helper()
	return fixtureContent(t, "fixes/calc-correct.go")
}

// writeAction builds a write_file action response against the file that the
// fixture scenario modifies.
func writeAction(path, content, expectedBeforeHash string) provider.Response {
	return actionResponse("write_file", `{"path":"`+path+`","content":`+mustJSONString(content)+`,"expected_before_hash":"`+expectedBeforeHash+`"}`)
}

func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// codingLoopPlan is the acceptance plan of the loop-level coding scenario:
// the test recipe must have a real zero-exit execution AND the corrected file
// must match the exact fixed content.
func codingLoopPlan(recipeID, path, fixedHash string) *verifier.Plan {
	return &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{
		{ID: "tests-pass", Type: verifier.CheckRecipeExitZero, Recipe: recipeID, RequireUntruncated: true},
		{ID: "fix-hash", Type: verifier.CheckFileHash, Path: path, SHA256: fixedHash},
	}}
}

// TestCodingLoopFailDiagnoseFixRerunComplete is the loop-level deterministic
// scenario (regressions A and B): inspect -> failing recipe -> diagnosis from
// the real process evidence -> corrective write -> passing rerun -> verified
// completion. The first recipe run fails with a real non-zero exit and the
// task does NOT end: the next model turn receives the structured recipe
// evidence. The corrective write changes the workspace, so the repeat guard
// allows the second (and third) recipe execution, and the last rerun exits
// zero with fresh evidence.
func TestCodingLoopFailDiagnoseFixRerunComplete(t *testing.T) {
	workspace := t.TempDir()
	fixtureFile(t, workspace, "app/calc.go", calcInitial(t))
	initialHash := tools.HashBytes([]byte(calcInitial(t)))
	wrongHash := tools.HashBytes([]byte(calcWrongFix(t)))
	correctHash := tools.HashBytes([]byte(calcCorrectFix(t)))

	failingOutput := []byte("--- FAIL: TestParseValuesTrimsWhitespace (0.00s)\n    calc_test.go:15: strconv.Atoi: parsing \" 2\": invalid syntax\nFAIL\n")
	catalog := testCatalog(t, testRecipe("test"))
	h := newRecipeHarness(t, workspace, allowAllPolicy(), map[string]policy.Mode{"test": policy.ModeAllow}, catalog, &fakeRunner{
		results: []recipe.Result{
			{Started: true, ExitCode: 1, Stdout: failingOutput},
			{Started: true, ExitCode: 1, Stdout: []byte("--- FAIL: TestParseValuesTrimsWhitespace (0.00s)\n    calc_test.go:15: strconv.Atoi: parsing \" 2\": invalid syntax\nFAIL\n")},
			{Started: true, ExitCode: 0, Stdout: []byte("ok  runstead.fixture.calc 0.002s\n")},
		},
	},
		// Turn 1: inspect the implementation.
		actionResponse("read_file", `{"path":"app/calc.go"}`),
		// Turn 2: run the tests -> REAL failure (exit 1), citable evidence.
		actionResponse("run_recipe", `{"recipe":"test"}`),
		// Turn 3: first scoped write (wrong fix: only SumValues trims).
		writeAction("app/calc.go", calcWrongFix(t), initialHash),
		// Turn 4: rerun the same recipe -> still failing (exit 1).
		actionResponse("run_recipe", `{"recipe":"test"}`),
		// Turn 5: re-inspect the file to diagnose from the persisted state.
		actionResponse("read_file", `{"path":"app/calc.go"}`),
		// Turn 6: corrective write (trim inside ParseValues).
		writeAction("app/calc.go", calcCorrectFix(t), wrongHash),
		// Turn 7: rerun the same recipe -> exit 0.
		actionResponse("run_recipe", `{"recipe":"test"}`),
		// Turn 8: propose completion, citing the real evidence.
		finalResponse("complete", "Fixed the calculator.", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "run_recipe"), finalEvidence("obs-000003", "write_file"), finalEvidence("obs-000004", "run_recipe"), finalEvidence("obs-000006", "write_file"), finalEvidence("obs-000007", "run_recipe")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 20, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, codingLoopPlan("test", "app/calc.go", correctHash)),
		AcceptancePlanDigest: codingLoopPlan("test", "app/calc.go", correctHash).Digest(),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(context.Background(), testTask("task-coding-loop"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	// The same recipe executed three times: fail, fail, pass. The repeat guard
	// must NOT block the legitimate reruns after the workspace changed.
	if len(h.runner.cwds) != 3 {
		t.Fatalf("recipe executions = %d, want 3 (fail, fail, pass)", len(h.runner.cwds))
	}
	// The final workspace state is the corrected implementation (real write).
	content, err := os.ReadFile(filepath.Join(workspace, "app/calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if tools.HashBytes(content) != correctHash {
		t.Fatalf("workspace does not hold the corrected implementation")
	}
	// Regression A: after the first failed recipe, the next model turn
	// received the STRUCTURED process evidence with the real exit code and
	// bounded output. Process output is untrusted observation data, never a
	// system instruction.
	prompts := h.provider.Requests()
	if len(prompts) < 3 {
		t.Fatalf("provider turns = %d, want at least 3", len(prompts))
	}
	afterFailure := prompts[2]
	if !strings.Contains(afterFailure, `"exit_code":1`) {
		t.Fatalf("the model turn after the failing recipe must carry the real exit code:\n%s", afterFailure)
	}
	if !strings.Contains(afterFailure, "TestParseValuesTrimsWhitespace") {
		t.Fatalf("the model turn after the failing recipe must carry the bounded process output:\n%s", afterFailure)
	}
	if !strings.Contains(afterFailure, `"untrusted":true`) {
		t.Fatalf("the process evidence must remain untrusted observation data:\n%s", afterFailure)
	}
	// Regression B: two recipe executions produced citable evidence rows and
	// the final verified report names the passing execution.
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-coding-loop")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Decision != "passed" {
		t.Fatalf("verification attempts = %+v, want one passed", attempts)
	}
}

// TestCodingLoopPrematureCompletionFailsThenPasses is regression C: the model
// proposes complete while the test suite is still red. The verifier rejects
// the proposal (recipe_exit_nonzero) and the structured verification failure
// returns to the loop as an observation; after the corrective write and the
// passing rerun, a NEW completion proposal passes.
func TestCodingLoopPrematureCompletionFailsThenPasses(t *testing.T) {
	workspace := t.TempDir()
	fixtureFile(t, workspace, "app/calc.go", calcInitial(t))
	initialHash := tools.HashBytes([]byte(calcInitial(t)))
	correctHash := tools.HashBytes([]byte(calcCorrectFix(t)))

	catalog := testCatalog(t, testRecipe("test"))
	h := newRecipeHarness(t, workspace, allowAllPolicy(), map[string]policy.Mode{"test": policy.ModeAllow}, catalog, &fakeRunner{
		results: []recipe.Result{
			{Started: true, ExitCode: 1, Stdout: []byte("--- FAIL: TestParseValuesTrimsWhitespace\nFAIL\n")},
			{Started: true, ExitCode: 0, Stdout: []byte("ok\n")},
		},
	},
		actionResponse("read_file", `{"path":"app/calc.go"}`),
		actionResponse("run_recipe", `{"recipe":"test"}`),
		// Premature completion proposal while the suite is red.
		finalResponse("complete", "tests pass", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "run_recipe")),
		// The loop continues after the failed verification: corrective write.
		writeAction("app/calc.go", calcCorrectFix(t), initialHash),
		actionResponse("run_recipe", `{"recipe":"test"}`),
		// A NEW completion proposal after the correction.
		finalResponse("complete", "tests pass", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "run_recipe"), finalEvidence("obs-000003", "write_file"), finalEvidence("obs-000004", "run_recipe")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 20, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, codingLoopPlan("test", "app/calc.go", correctHash)),
		AcceptancePlanDigest: codingLoopPlan("test", "app/calc.go", correctHash).Digest(),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(context.Background(), testTask("task-coding-premature"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s", result.Outcome, result.StopReason)
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-coding-premature")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Decision != "failed" || attempts[1].Decision != "passed" {
		t.Fatalf("verification attempts = %+v, want failed then passed", attempts)
	}
	var recipeCheck bool
	for _, check := range attempts[0].Checks {
		if check.CheckID == "tests-pass" && check.Status == "failed" && check.Reason == "recipe_exit_nonzero" {
			recipeCheck = true
		}
	}
	if !recipeCheck {
		t.Fatalf("failed attempt must carry the recipe_exit_nonzero check: %+v", attempts[0].Checks)
	}
	// The verification failure returned to the loop as an observation: the
	// model turn after the premature final carries the structured verification
	// result under the verification role.
	prompts := h.provider.Requests()
	var sawVerification bool
	for _, prompt := range prompts {
		if strings.Contains(prompt, "=== runstead:verification ===") && strings.Contains(prompt, "recipe_exit_nonzero") {
			sawVerification = true
		}
	}
	if !sawVerification {
		t.Fatalf("the loop must return the failed verification to the model as a structured observation")
	}
}

// TestLoopConsecutiveFailuresExhausted proves the #12 consecutive
// tool/process failure guard: distinct failing observations with no success
// in between stop the loop with the typed consecutive_failures_exhausted
// outcome instead of consuming the whole step budget.
func TestLoopConsecutiveFailuresExhausted(t *testing.T) {
	workspace := t.TempDir()
	var responses []provider.Response
	// Six distinct failing read actions: six consecutive failures, the sixth
	// exceeds the default allowance of five.
	for index := 1; index <= 6; index++ {
		responses = append(responses, actionResponse("read_file", `{"path":"missing-`+string(rune('a'+index-1))+`.txt"}`))
	}
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil, responses...)
	loop := h.loop(t, agent.Limits{MaxSteps: 20})
	result := loop.Run(context.Background(), testTask("task-consecutive-failures"))
	if result.Outcome != agent.OutcomeConsecutiveFailuresExhausted {
		t.Fatalf("outcome = %s, want consecutive_failures_exhausted (reason %s)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "6") {
		t.Fatalf("stop reason must carry the failure count: %q", result.StopReason)
	}
	// Every failing observation consumed a normal model turn and was
	// persisted as durable history.
	if h.provider.Attempts() != 6 {
		t.Fatalf("provider attempts = %d, want 6", h.provider.Attempts())
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-consecutive-failures")
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, attempt := range snapshot.ToolAttempts {
		if attempt.Status == "failed" {
			failed++
		}
	}
	if failed != 6 {
		t.Fatalf("persisted failed attempts = %d, want 6", failed)
	}
}

// TestLoopConsecutiveFailuresResetsOnSuccess proves the guard streak resets
// on a successful observation: a failing recipe followed by a successful
// write followed by another failing recipe is NOT a consecutive streak. This
// is exactly the legitimate coding-loop pattern (fail -> fix -> fail -> fix
// -> pass), which the guard must never stop.
func TestLoopConsecutiveFailuresResetsOnSuccess(t *testing.T) {
	workspace := t.TempDir()
	fixtureFile(t, workspace, "app/calc.go", calcInitial(t))
	initialHash := tools.HashBytes([]byte(calcInitial(t)))
	wrongHash := tools.HashBytes([]byte(calcWrongFix(t)))
	catalog := testCatalog(t, testRecipe("test"))
	h := newRecipeHarness(t, workspace, allowAllPolicy(), map[string]policy.Mode{"test": policy.ModeAllow}, catalog, &fakeRunner{
		results: []recipe.Result{
			{Started: true, ExitCode: 1},
			{Started: true, ExitCode: 1},
			{Started: true, ExitCode: 0},
		},
	},
		actionResponse("run_recipe", `{"recipe":"test"}`),
		writeAction("app/calc.go", calcWrongFix(t), initialHash),
		actionResponse("run_recipe", `{"recipe":"test"}`),
		writeAction("app/calc.go", calcCorrectFix(t), wrongHash),
		actionResponse("run_recipe", `{"recipe":"test"}`),
		finalResponse("complete", "done", finalEvidence("obs-000001", "run_recipe"), finalEvidence("obs-000002", "write_file"), finalEvidence("obs-000003", "run_recipe"), finalEvidence("obs-000004", "write_file"), finalEvidence("obs-000005", "run_recipe")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 20, MaxCorrections: 3, MaxRepeatedActions: 3},
		Clock:                h.clock,
		Trace:                h.traces.emit,
		State:                h.store,
		Policy:               h.policy,
		Verifier:             verifier.New(h.registry, recipePlan("test")),
		AcceptancePlanDigest: recipePlan("test").Digest(),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	result := loop.Run(context.Background(), testTask("task-consecutive-reset"))
	// Each failing recipe run is separated by a successful write, so the
	// streak never exceeds one and the guard never fires; the task completes.
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %s, reason = %s (the writes must reset the failure streak)", result.Outcome, result.StopReason)
	}
	if len(h.runner.cwds) != 3 {
		t.Fatalf("recipe executions = %d, want 3", len(h.runner.cwds))
	}
}

// TestLoopVerificationRetriesExhausted proves the #12 repeated-verification
// failure guard: repeated premature completion proposals stop with the typed
// verification_failures_exhausted outcome after the configured allowance.
func TestLoopVerificationRetriesExhausted(t *testing.T) {
	workspace := t.TempDir()
	fixtureFile(t, workspace, "readme.txt", "info\n")
	// The plan requires never.txt; every completion proposal fails.
	plan := existsPlan("never.txt")
	h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
		actionResponse("read_file", `{"path":"readme.txt"}`),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
	)
	loop, err := agent.NewLoop(agent.Config{
		Runner:               h.executor,
		Registry:             h.registry,
		Limits:               agent.Limits{MaxSteps: 20, MaxCorrections: 3, MaxRepeatedActions: 3},
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
	result := loop.Run(context.Background(), testTask("task-verify-retries"))
	if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
		t.Fatalf("outcome = %s, want verification_failures_exhausted (reason %s)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "4") {
		t.Fatalf("stop reason must carry the failure count: %q", result.StopReason)
	}
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-verify-retries")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 4 {
		t.Fatalf("verification attempts = %d, want 4 (all failed, all persisted)", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Decision != "failed" {
			t.Fatalf("verification attempt decision = %s, want failed", attempt.Decision)
		}
	}
}

// TestLoopFailureGuardsContinueFromRecoverySeed proves the #12 guard counters
// survive resume: a seeded run continues the trailing streaks from persisted
// history instead of silently resetting them.
func TestLoopFailureGuardsContinueFromRecoverySeed(t *testing.T) {
	t.Run("consecutive failures", func(t *testing.T) {
		workspace := t.TempDir()
		// Four consecutive failures before the interruption, then one more in
		// the resumed run reaches the default allowance of five and the NEXT
		// one stops the loop. The task row must already exist: a seeded run
		// skips the bootstrap (mirroring a resumed task).
		ctx := context.Background()
		seed := &agent.RecoverySeed{ConsecutiveFailures: 4}
		h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
			actionResponse("read_file", `{"path":"missing-a.txt"}`),
			actionResponse("read_file", `{"path":"missing-b.txt"}`),
		)
		if err := h.store.CreateTask(ctx, state.TaskRecord{TaskID: "task-seed-failures", Objective: "o", Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.StartTask(ctx, "task-seed-failures"); err != nil {
			t.Fatal(err)
		}
		loop, err := agent.NewLoop(agent.Config{
			Runner:   h.executor,
			Registry: h.registry,
			Limits:   agent.Limits{MaxSteps: 10},
			Clock:    h.clock,
			Trace:    h.traces.emit,
			State:    h.store,
			Policy:   h.policy,
			Recovery: seed,
		})
		if err != nil {
			t.Fatalf("agent.NewLoop() error = %v", err)
		}
		result := loop.Run(ctx, testTask("task-seed-failures"))
		if result.Outcome != agent.OutcomeConsecutiveFailuresExhausted {
			t.Fatalf("outcome = %s, want consecutive_failures_exhausted (reason %s)", result.Outcome, result.StopReason)
		}
		if !strings.Contains(result.StopReason, "6") {
			t.Fatalf("stop reason must carry the continued count: %q", result.StopReason)
		}
	})

	t.Run("verification retries", func(t *testing.T) {
		workspace := t.TempDir()
		fixtureFile(t, workspace, "readme.txt", "info\n")
		plan := existsPlan("never.txt")
		ctx := context.Background()
		// Two failed verifications before the interruption; one more in the
		// resumed run reaches the default allowance of three, the next stops.
		seed := &agent.RecoverySeed{VerificationRetries: 2}
		h := newWriteHarness(t, workspace, allowAllPolicy(), nil,
			actionResponse("read_file", `{"path":"readme.txt"}`),
			finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
			finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
		)
		if err := h.store.CreateTask(ctx, state.TaskRecord{TaskID: "task-seed-verify", Objective: "o", Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.StartTask(ctx, "task-seed-verify"); err != nil {
			t.Fatal(err)
		}
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
			Recovery:             seed,
		})
		if err != nil {
			t.Fatalf("agent.NewLoop() error = %v", err)
		}
		result := loop.Run(ctx, testTask("task-seed-verify"))
		if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
			t.Fatalf("outcome = %s, want verification_failures_exhausted (reason %s)", result.Outcome, result.StopReason)
		}
		if !strings.Contains(result.StopReason, "4") {
			t.Fatalf("stop reason must carry the continued count: %q", result.StopReason)
		}
	})
}
