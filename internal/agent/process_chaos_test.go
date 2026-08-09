package agent_test

// Issue #13 - process execution chaos at runtime level. Recipes run through
// the REAL bounded process runner (issue #26): the same process-group
// termination, output bounding and truncation flags the production loop uses.
// The model is scripted to narrate success; every test proves the narration
// can never become a completed task because the verifier consumes the REAL
// process evidence (exit status, timeout, truncation, partial output).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// TestMain intercepts the test binary so it can act as the recipe executable
// of the loop-level process chaos tests (the same self-exec helper pattern
// internal/recipe/process_test.go uses).
func TestMain(m *testing.M) {
	if mode := os.Getenv("RUNSTEAD_AGENT_PROCESS_HELPER_MODE"); mode != "" {
		os.Exit(runAgentProcessHelper(mode))
	}
	os.Exit(m.Run())
}

// TestAgentProcessHelper exists so `go test -test.run=TestAgentProcessHelper`
// inside a recipe does not fail with "no tests to run" before TestMain's
// helper dispatch fires.
func TestAgentProcessHelper(t *testing.T) {}

func runAgentProcessHelper(mode string) int {
	switch mode {
	case "spawn-child":
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			return 1
		}
		if path := os.Getenv("RUNSTEAD_AGENT_PROCESS_CHILD_PID_FILE"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		_ = child.Wait()
		return 0
	case "output":
		outBytes, _ := strconv.Atoi(os.Getenv("RUNSTEAD_AGENT_PROCESS_HELPER_OUT"))
		errBytes, _ := strconv.Atoi(os.Getenv("RUNSTEAD_AGENT_PROCESS_HELPER_ERR"))
		for i := 0; i < outBytes; i++ {
			fmt.Fprint(os.Stdout, "o")
		}
		for i := 0; i < errBytes; i++ {
			fmt.Fprint(os.Stderr, "e")
		}
		return 0
	case "partial-then-hang":
		fmt.Fprint(os.Stdout, "PARTIAL-OUTPUT-BEFORE-TIMEOUT\n")
		time.Sleep(300 * time.Second)
		return 0
	case "signal":
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
		return 0
	}
	return 1
}

// chaosRunnerRecipe builds a recipe whose executable is the agent test binary
// and whose argv triggers the helper with the given mode, with the helper
// environment delivered through the allowlist.
func chaosRunnerRecipe(t *testing.T, id, mode string, allowed ...string) recipe.Recipe {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	env := append([]string{
		"RUNSTEAD_AGENT_PROCESS_HELPER_MODE",
		"RUNSTEAD_AGENT_PROCESS_CHILD_PID_FILE",
		"RUNSTEAD_AGENT_PROCESS_HELPER_OUT",
		"RUNSTEAD_AGENT_PROCESS_HELPER_ERR",
	}, allowed...)
	return recipe.Recipe{
		ID:                 id,
		Executable:         exe,
		Argv:               []string{"-test.run=TestAgentProcessHelper"},
		Capabilities:       []recipe.Capability{recipe.CapabilityExecuteRepoCode, recipe.CapabilityInheritEnvironment},
		AllowedEnvironment: env,
	}
}

// processChaosHarness wires the real recipe runner through the registry into
// the loop, so run_recipe executes REAL subprocesses under the #26 runner.
type processChaosHarness struct {
	clock    *fakeClock
	provider *scriptedProvider
	executor *agent.Executor
	registry *tools.Registry
	store    *state.Store
	traces   *traceCapture
}

type realRunner struct{}

func (realRunner) run(ctx context.Context, r recipe.Recipe, cwd string, env []string) recipe.Result {
	return recipe.Run(ctx, r, cwd, env)
}

func newProcessChaosHarness(t *testing.T, workspace string, catalog *recipe.Catalog, responses ...provider.Response) *processChaosHarness {
	t.Helper()
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}, Persistence: store})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedProvider{clock: clock, pace: time.Millisecond, responses: append([]provider.Response(nil), responses...)}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, Recipes: catalog, RunRecipe: realRunner{}.run})
	if err != nil {
		t.Fatal(err)
	}
	return &processChaosHarness{
		clock:    clock,
		provider: client,
		executor: executor,
		registry: registry,
		store:    store,
		traces:   &traceCapture{},
	}
}

func (h *processChaosHarness) loop(t *testing.T, limits agent.Limits, plan *verifier.Plan, recipeID string) *agent.Loop {
	t.Helper()
	writeConfig := allowAllPolicy()
	writeConfig.RecipeModes = map[string]policy.Mode{recipeID: policy.ModeAllow}
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   limits,
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    h.store,
		Policy:   policy.NewStatic(writeConfig, storeApprovals(h.store)),
		Verifier: verifier.New(h.registry, plan),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	return loop
}

// strictRecipePlan is the acceptance plan the process chaos tests use: the
// recipe must have a real zero exit AND its output must not be truncated, so
// truncated or timed-out evidence can never satisfy completion.
func strictRecipePlan(recipeID string) *verifier.Plan {
	return &verifier.Plan{Version: verifier.PlanVersion, Checks: []verifier.Check{{
		ID: "recipe-strict", Type: verifier.CheckRecipeExitZero, Recipe: recipeID, RequireUntruncated: true,
	}}}
}

// TestProcessChaosTimeoutBoundedAndNeverCompletes proves a stuck recipe is
// terminated within its configured timeout, the full process tree dies, the
// typed evidence records the timeout with a negative exit code, and the model
// cannot turn the timeout into completion by narrating success.
func TestProcessChaosTimeoutBoundedAndNeverCompletes(t *testing.T) {
	workspace := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_MODE", "spawn-child")
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_CHILD_PID_FILE", pidFile)

	slow, err := chaosRunnerRecipe(t, "slow", "spawn-child", "RUNSTEAD_AGENT_PROCESS_CHILD_PID_FILE").Normalize()
	if err != nil {
		t.Fatal(err)
	}
	slow.TimeoutNanos = int64(300 * time.Millisecond)
	catalog := testCatalog(t, slow)

	h := newProcessChaosHarness(t, workspace, catalog,
		actionResponse("run_recipe", `{"recipe":"slow"}`),
		finalResponse("complete", "the test suite passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the test suite passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the test suite passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the test suite passed", finalEvidence("obs-000001", "run_recipe")),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10, MaxCorrections: 3, MaxRepeatedActions: 3}, strictRecipePlan("slow"), "slow")

	start := time.Now()
	result := loop.Run(context.Background(), testTask("task-process-timeout"))
	elapsed := time.Since(start)

	if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
		t.Fatalf("outcome = %q, want verification_failures_exhausted (reason %q)", result.Outcome, result.StopReason)
	}
	// The stuck command was terminated within its configured timeout plus a
	// generous bound for the SIGTERM->grace->SIGKILL barrier (the assertion is
	// a bound, not a race: the runner's design guarantees termination).
	if elapsed > 20*time.Second {
		t.Fatalf("stuck command took %v to terminate, want bounded by the configured timeout", elapsed)
	}
	// The spawned child must be dead: the whole process group was terminated.
	if raw, readErr := os.ReadFile(pidFile); readErr == nil {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil && pid > 0 {
			waitForProcessDead(t, pid)
		}
	}

	// Authoritative evidence: the recipe attempt is completed with a REAL
	// non-zero (killed) exit code and the timeout flag; nothing invented.
	evidence := mustPersistedRecipeEvidence(t, h.store, "task-process-timeout", "obs-000001")
	if !evidence.TimedOut {
		t.Fatalf("evidence must record the timeout: %+v", evidence)
	}
	if evidence.ExitCode != -1 {
		t.Fatalf("evidence exit code = %d, want -1 (killed by timeout)", evidence.ExitCode)
	}
	if evidence.Started != true {
		t.Fatalf("evidence must record the process started: %+v", evidence)
	}
	// The task never completed; the verification attempts all failed.
	attempts, err := h.store.VerificationAttempts(context.Background(), "task-process-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 4 {
		t.Fatalf("verification attempts = %d, want 4", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.Decision != "failed" {
			t.Fatalf("verification decision = %s, want failed", attempt.Decision)
		}
	}
	rendered := renderedInspect(t, h.store, "task-process-timeout")
	if strings.Contains(rendered, "Outcome: completed") {
		t.Fatal("a timed-out recipe must never complete the task")
	}
}

// TestProcessChaosOversizedOutputTruncationExplicit proves stdout and stderr
// limits are independent and truncation is explicit, and that truncated
// evidence can never support a completion claim even when the process exit
// code is zero.
func TestProcessChaosOversizedOutputTruncationExplicit(t *testing.T) {
	workspace := t.TempDir()
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_MODE", "output")
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_OUT", "100000")
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_ERR", "50000")

	noisy, err := chaosRunnerRecipe(t, "noisy", "output", "RUNSTEAD_AGENT_PROCESS_HELPER_OUT", "RUNSTEAD_AGENT_PROCESS_HELPER_ERR").Normalize()
	if err != nil {
		t.Fatal(err)
	}
	noisy.OutputLimits.MaxStdoutBytes = 64
	noisy.OutputLimits.MaxStderrBytes = 32
	catalog := testCatalog(t, noisy)

	h := newProcessChaosHarness(t, workspace, catalog,
		actionResponse("run_recipe", `{"recipe":"noisy"}`),
		finalResponse("complete", "all checks passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "all checks passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "all checks passed", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "all checks passed", finalEvidence("obs-000001", "run_recipe")),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10}, strictRecipePlan("noisy"), "noisy")
	result := loop.Run(context.Background(), testTask("task-process-truncated"))
	if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
		t.Fatalf("outcome = %q, want verification_failures_exhausted (reason %q)", result.Outcome, result.StopReason)
	}
	evidence := mustPersistedRecipeEvidence(t, h.store, "task-process-truncated", "obs-000001")
	if !evidence.StdoutTruncated || !evidence.StderrTruncated {
		t.Fatalf("evidence must flag truncation explicitly: %+v", evidence)
	}
	if evidence.StdoutBytes != 100000 || evidence.StderrBytes != 50000 {
		t.Fatalf("evidence byte counts = %d/%d, want the real 100000/50000 (truncation never invents output)", evidence.StdoutBytes, evidence.StderrBytes)
	}
	if len(evidence.Stdout) != 64 || len(evidence.Stderr) != 32 {
		t.Fatalf("bounded buffers = %d/%d bytes, want 64/32", len(evidence.Stdout), len(evidence.Stderr))
	}
	// The process really exited zero; the truncation alone blocks completion.
	if evidence.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (the process itself succeeded)", evidence.ExitCode)
	}
	if rendered := renderedInspect(t, h.store, "task-process-truncated"); strings.Contains(rendered, "Outcome: completed") {
		t.Fatal("truncated output must never support a completion claim")
	}
}

// TestProcessChaosPartialOutputBeforeTimeout proves partial output captured
// before a timeout is preserved as evidence (never invented or discarded) and
// that narration cannot turn the partial output into completion.
func TestProcessChaosPartialOutputBeforeTimeout(t *testing.T) {
	workspace := t.TempDir()
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_MODE", "partial-then-hang")
	partial, err := chaosRunnerRecipe(t, "partial", "partial-then-hang").Normalize()
	if err != nil {
		t.Fatal(err)
	}
	partial.TimeoutNanos = int64(300 * time.Millisecond)
	catalog := testCatalog(t, partial)

	h := newProcessChaosHarness(t, workspace, catalog,
		actionResponse("run_recipe", `{"recipe":"partial"}`),
		finalResponse("complete", "the command finished and printed its result", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the command finished and printed its result", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the command finished and printed its result", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "the command finished and printed its result", finalEvidence("obs-000001", "run_recipe")),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10}, strictRecipePlan("partial"), "partial")
	result := loop.Run(context.Background(), testTask("task-process-partial"))
	if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
		t.Fatalf("outcome = %q, want verification_failures_exhausted (reason %q)", result.Outcome, result.StopReason)
	}
	evidence := mustPersistedRecipeEvidence(t, h.store, "task-process-partial", "obs-000001")
	if !evidence.TimedOut || evidence.ExitCode != -1 {
		t.Fatalf("evidence = %+v, want timed out with killed exit code", evidence)
	}
	if !strings.Contains(string(evidence.Stdout), "PARTIAL-OUTPUT-BEFORE-TIMEOUT") {
		t.Fatalf("partial output before the timeout must be preserved: %q", evidence.Stdout)
	}
}

// TestProcessChaosKilledProcessNeverBecomesSuccess proves a process killed by
// a real signal is recorded with its terminating signal and negative exit
// code, and the model cannot turn the death into a completed task.
func TestProcessChaosKilledProcessNeverBecomesSuccess(t *testing.T) {
	workspace := t.TempDir()
	setHelperEnv(t, "RUNSTEAD_AGENT_PROCESS_HELPER_MODE", "signal")
	killed, err := chaosRunnerRecipe(t, "killed", "signal").Normalize()
	if err != nil {
		t.Fatal(err)
	}
	catalog := testCatalog(t, killed)

	h := newProcessChaosHarness(t, workspace, catalog,
		actionResponse("run_recipe", `{"recipe":"killed"}`),
		finalResponse("complete", "it worked", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "it worked", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "it worked", finalEvidence("obs-000001", "run_recipe")),
		finalResponse("complete", "it worked", finalEvidence("obs-000001", "run_recipe")),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 10}, strictRecipePlan("killed"), "killed")
	result := loop.Run(context.Background(), testTask("task-process-killed"))
	if result.Outcome != agent.OutcomeVerificationFailuresExhausted {
		t.Fatalf("outcome = %q, want verification_failures_exhausted (reason %q)", result.Outcome, result.StopReason)
	}
	evidence := mustPersistedRecipeEvidence(t, h.store, "task-process-killed", "obs-000001")
	if evidence.Started != true || evidence.ExitCode != -1 || evidence.Signal == "" {
		t.Fatalf("evidence must record the real terminating signal: %+v", evidence)
	}
	if evidence.TimedOut || evidence.Canceled {
		t.Fatalf("a self-killed process is not a timeout or cancellation: %+v", evidence)
	}
}

// setHelperEnv sets one recipe helper environment variable for the duration
// of the test. The registry passes os.Environ() through the recipe allowlist,
// so the helper modes reach the child exactly like the #26 runner tests do.
func setHelperEnv(t *testing.T, name, value string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Setenv(name, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// waitForProcessDead polls until the process is gone or the deadline passes.
// The process group termination is guaranteed by the runner's synchronous
// barrier; this is a cleanup assertion, not a timing dependency.
func waitForProcessDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		process, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if signalErr := process.Signal(syscall.Signal(0)); signalErr != nil {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after the termination barrier", pid)
}
