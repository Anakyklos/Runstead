package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

func TestLoopCanceledDuringProviderIO(t *testing.T) {
	workspace := t.TempDir()
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	blocking := provider.NewBlockingFake()
	executor, err := agent.NewExecutor(accountGovernor, blocking, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan agent.Result, 1)
	go func() { done <- loop.Run(ctx, testTask("task-1")) }()
	waitFor(t, func() bool { return blocking.Attempts() == 1 }, "provider I/O never started")
	cancel()

	result := <-done
	if result.Outcome != agent.OutcomeCanceled {
		t.Fatalf("outcome = %q, want canceled", result.Outcome)
	}
	// Cancellation after the upstream may have been reached stays conservatively
	// accounted: exactly one attempt was debited, never zero and never two.
	snapshot := accountGovernor.Snapshot()
	if snapshot.Tasks["task-1"].Attempts != 1 {
		t.Fatalf("task attempts debited = %d, want 1", snapshot.Tasks["task-1"].Attempts)
	}
	if blocking.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want 1", blocking.Attempts())
	}
	waitForGoroutines(t, before)
}

func TestLoopCanceledDuringToolExecution(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedProvider{
		clock: clock,
		pace:  time.Millisecond,
		responses: []provider.Response{
			actionResponse("git_status", `{}`),
		},
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	registry, err := tools.NewRegistry(tools.Options{
		Workspace: workspace,
		RunGit: func(ctx context.Context, _ []string, _ string) tools.CommandResult {
			close(entered)
			<-ctx.Done()
			return tools.CommandResult{Err: ctx.Err(), ExitCode: 1}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan agent.Result, 1)
	go func() { done <- loop.Run(ctx, testTask("task-1")) }()
	<-entered
	cancel()

	result := <-done
	if result.Outcome != agent.OutcomeCanceled {
		t.Fatalf("outcome = %q, want canceled", result.Outcome)
	}
	if client.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want 1", client.Attempts())
	}
	waitForGoroutines(t, before)
}

func TestLoopMixedProseRecordedButNotExecuted(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		provider.Response{Text: "Let me check the file first.\n" + actionResponse("read_file", `{"path":"a.txt"}`).Text},
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
	)
	loop := h.loopPlan(t, agent.Limits{}, existsPlan("a.txt"))

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}
	if result.MixedProse != 1 {
		t.Fatalf("mixed prose deviations = %d, want 1", result.MixedProse)
	}
	lines := h.traces.all()
	found := false
	for _, line := range lines {
		if line.Kind == agent.TraceDeviation && line.Status == "mixed_prose" {
			found = true
		}
	}
	if !found {
		t.Fatal("trace has no mixed_prose deviation line")
	}
	if h.provider.Attempts() != 2 {
		t.Fatalf("provider attempts = %d, want 2 (prose was not executed as a tool)", h.provider.Attempts())
	}
}

func TestLoopGitToolsProduceGroundedEvidence(t *testing.T) {
	// The loop's git path (git_status, git_diff) is exercised end to end: a
	// real repository is created with an uncommitted change, the loop executes
	// both git tools through the real registry and grounds the final answer on
	// their observation IDs.
	workspace := t.TempDir()
	runGit(t, workspace, "init", "--quiet")
	runGit(t, workspace, "config", "user.email", "runstead@example.invalid")
	runGit(t, workspace, "config", "user.name", "Runstead Test")
	writeFixture(t, workspace, "tracked.txt", "before\n")
	runGit(t, workspace, "add", "tracked.txt")
	runGit(t, workspace, "commit", "--quiet", "-m", "fixture")
	writeFixture(t, workspace, "tracked.txt", "after\n")

	h := newHarness(t, workspace, nil,
		actionResponse("git_status", `{}`),
		actionResponse("git_diff", `{}`),
		finalResponse("complete", "The working tree has an uncommitted modification.", finalEvidence("obs-000001", "git_status"), finalEvidence("obs-000002", "git_diff")),
	)
	loop := h.loopPlan(t, agent.Limits{}, existsPlan("tracked.txt"))

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q stop_reason=%q, want completed", result.Outcome, result.StopReason)
	}
	if result.Observations != 2 {
		t.Fatalf("observations = %d, want 2 git observations", result.Observations)
	}
	// The git observations must be untrusted data in the follow-up prompt and
	// never leak into the system contract.
	prompts := h.provider.Requests()
	if len(prompts) != 3 {
		t.Fatalf("provider turns = %d, want 3", len(prompts))
	}
	if !strings.Contains(prompts[1], `"tool":"git_status"`) || !strings.Contains(prompts[1], `"untrusted":true`) {
		t.Fatalf("git observation not framed as untrusted data:\n%s", prompts[1])
	}
}

func TestLoopSearchTextProducesGroundedEvidence(t *testing.T) {
	// search_text is the one remaining tool without loop-level coverage: it
	// runs through the real registry (rg or fallback), returns an untrusted
	// observation, and grounds a final answer.
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "needle appears here\n")
	writeFixture(t, workspace, "b.txt", "nothing relevant\n")

	h := newHarness(t, workspace, nil,
		actionResponse("search_text", `{"query":"needle","path":"."}`),
		finalResponse("complete", "Found the needle in the workspace.", finalEvidence("obs-000001", "search_text")),
	)
	loop := h.loopPlan(t, agent.Limits{}, existsPlan("a.txt"))

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q stop_reason=%q, want completed", result.Outcome, result.StopReason)
	}
	if result.Observations != 1 {
		t.Fatalf("observations = %d, want 1 search observation", result.Observations)
	}
	prompts := h.provider.Requests()
	if len(prompts) != 2 {
		t.Fatalf("provider turns = %d, want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], `"tool":"search_text"`) || !strings.Contains(prompts[1], `"untrusted":true`) {
		t.Fatalf("search observation not framed as untrusted data:\n%s", prompts[1])
	}
	if strings.Contains(extractSection(prompts[1], "system"), "needle appears here") {
		t.Fatal("search output leaked into the system section")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func TestLoopConcurrentCancellationRace(t *testing.T) {
	// Multiple goroutines cancel the same one-shot context while the loop is
	// blocked waiting behind another task on the account lane. Exactly one
	// canceled outcome must surface, no attempt may be double-executed and no
	// goroutine may leak.
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	blocking := provider.NewBlockingFake()
	executor, err := agent.NewExecutor(accountGovernor, blocking, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	newLoop := func() *agent.Loop {
		loop, err := agent.NewLoop(agent.Config{Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		return loop
	}

	before := runtime.NumGoroutine()
	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan agent.Result, 1)
	go func() { doneA <- newLoop().Run(ctxA, testTask("task-a")) }()
	waitFor(t, func() bool { return accountGovernor.Snapshot().InFlight }, "task A never entered the lane")

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneB := make(chan agent.Result, 1)
	go func() { doneB <- newLoop().Run(ctxB, testTask("task-b")) }()
	waitFor(t, func() bool { return accountGovernor.Snapshot().QueueLength >= 1 }, "task B never queued behind A")

	// Race two cancellations against the same context.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancelB()
		}()
	}
	wg.Wait()

	resultB := <-doneB
	if resultB.Outcome != agent.OutcomeCanceled {
		t.Fatalf("queued task outcome = %q, want canceled", resultB.Outcome)
	}
	if blocking.Attempts() != 1 {
		t.Fatalf("provider attempts after B cancellation = %d, want 1 (task A only; no double execution)", blocking.Attempts())
	}

	cancelA()
	resultA := <-doneA
	if resultA.Outcome != agent.OutcomeCanceled {
		t.Fatalf("task A outcome = %q, want canceled", resultA.Outcome)
	}
	if blocking.Attempts() != 1 {
		t.Fatalf("provider attempts after A release = %d, want 1", blocking.Attempts())
	}
	waitForGoroutines(t, before)
}

func TestLoopConcurrentTasksShareAccountLane(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeFixture(t, workspaceA, "a.txt", "alpha\n")
	writeFixture(t, workspaceB, "b.txt", "beta\n")

	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.TaskBudget = 10
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	tracker := &concurrencyTracker{}
	clientA := &scriptedProvider{clock: clock, pace: time.Millisecond, shared: tracker, responses: []provider.Response{
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("list_files", `{"path":"."}`),
		finalResponse("complete", "inspected A", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "list_files")),
	}}
	clientB := &scriptedProvider{clock: clock, pace: time.Millisecond, shared: tracker, responses: []provider.Response{
		actionResponse("read_file", `{"path":"b.txt"}`),
		actionResponse("list_files", `{"path":"."}`),
		finalResponse("complete", "inspected B", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "list_files")),
	}}
	executorA, err := agent.NewExecutor(accountGovernor, clientA, nil)
	if err != nil {
		t.Fatal(err)
	}
	executorB, err := agent.NewExecutor(accountGovernor, clientB, nil)
	if err != nil {
		t.Fatal(err)
	}
	registryA, err := tools.NewRegistry(tools.Options{Workspace: workspaceA})
	if err != nil {
		t.Fatal(err)
	}
	registryB, err := tools.NewRegistry(tools.Options{Workspace: workspaceB})
	if err != nil {
		t.Fatal(err)
	}
	loopA, err := agent.NewLoop(agent.Config{Runner: executorA, Registry: registryA, Limits: agent.Limits{}, Clock: clock, Verifier: verifier.New(registryA, existsPlan("a.txt"))})
	if err != nil {
		t.Fatal(err)
	}
	loopB, err := agent.NewLoop(agent.Config{Runner: executorB, Registry: registryB, Limits: agent.Limits{}, Clock: clock, Verifier: verifier.New(registryB, existsPlan("b.txt"))})
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	doneA := make(chan agent.Result, 1)
	doneB := make(chan agent.Result, 1)
	go func() { doneA <- loopA.Run(context.Background(), testTask("task-a")) }()
	go func() { doneB <- loopB.Run(context.Background(), testTask("task-b")) }()
	resultA := <-doneA
	resultB := <-doneB

	if resultA.Outcome != agent.OutcomeCompleted || resultB.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcomes = %q / %q, want both completed", resultA.Outcome, resultB.Outcome)
	}
	if clientA.Attempts() != 3 || clientB.Attempts() != 3 {
		t.Fatalf("provider attempts = %d / %d, want 3 each", clientA.Attempts(), clientB.Attempts())
	}
	if got := tracker.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent provider calls = %d, want 1 (serialized account lane)", got)
	}
	snapshot := accountGovernor.Snapshot()
	if snapshot.InFlight || snapshot.QueueLength != 0 {
		t.Fatalf("lane not released after tasks: %#v", snapshot)
	}
	if snapshot.Tasks["task-a"].Attempts != 3 || snapshot.Tasks["task-b"].Attempts != 3 {
		t.Fatalf("governor task accounting = %#v, want 3 attempts each", snapshot.Tasks)
	}
	waitForGoroutines(t, before)
}

func TestLoopNoProviderClientBypass(t *testing.T) {
	// The loop owns no provider client: its only runner seam is the governed
	// attempt executor. Scanning the loop implementation for a direct provider
	// client reference or Complete call makes the boundary explicit. Comment
	// lines are skipped so documentation cannot trip the guard.
	files := []string{"loop.go", "system.go", "transcript.go", "grounding.go", "guard.go", "outcome.go", "trace.go"}
	for _, name := range files {
		content, err := os.ReadFile(filepath.Join("..", "..", "internal", "agent", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineNumber, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "provider.Client") {
				t.Errorf("%s:%d references provider.Client directly; every model turn must pass through the governor executor", name, lineNumber+1)
			}
			if strings.Contains(trimmed, ".Complete(") {
				t.Errorf("%s:%d calls a provider Complete method directly; the loop must never bypass admission", name, lineNumber+1)
			}
		}
	}
	// The executor remains the single seam that owns the provider client.
	content, err := os.ReadFile(filepath.Join("..", "..", "internal", "agent", "executor.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "provider.Client") {
		t.Fatal("executor.go must own the provider.Client boundary")
	}
}

func TestOutcomeExitCodesAreStableAndDistinct(t *testing.T) {
	outcomes := []agent.Outcome{
		agent.OutcomeCompleted,
		agent.OutcomeStepsExhausted,
		agent.OutcomeCorrectionsExhausted,
		agent.OutcomeRepeatedAction,
		agent.OutcomeTimeBudgetExhausted,
		agent.OutcomeProviderBudgetExhausted,
		agent.OutcomeAccountDelayTimeout,
		agent.OutcomeAccountCircuitOpen,
		agent.OutcomeCanceled,
		agent.OutcomeFinalNotGrounded,
		agent.OutcomeProviderFailure,
		agent.OutcomePersistenceFailure,
		agent.OutcomePersistencePaused,
		agent.OutcomeFinalIncomplete,
	}
	seen := make(map[int]agent.Outcome)
	for _, outcome := range outcomes {
		code := outcome.ExitCode()
		if previous, exists := seen[code]; exists {
			t.Fatalf("exit code %d shared by %q and %q", code, previous, outcome)
		}
		seen[code] = outcome
		if outcome == agent.OutcomeCompleted && code != 0 {
			t.Fatalf("completed exit code = %d, want 0", code)
		}
		if outcome == agent.OutcomeCanceled && code != 130 {
			t.Fatalf("canceled exit code = %d, want 130", code)
		}
		if code < 0 || code > 255 {
			t.Fatalf("exit code %d for %q is not a valid process code", code, outcome)
		}
	}
}

func TestSystemContractIsDeterministicAndDescribesRegisteredTools(t *testing.T) {
	workspace := t.TempDir()
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	first, err := agent.BuildSystemContract(registry)
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.BuildSystemContract(registry)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("system contract is not deterministic")
	}
	for _, tool := range []string{"read_file", "list_files", "search_text", "git_status", "git_diff", "write_file", "apply_patch", "run_recipe"} {
		if !strings.Contains(first, tool) {
			t.Errorf("system contract missing tool %q", tool)
		}
	}
	if !strings.Contains(first, "[read-only]") || !strings.Contains(first, "[policy-gated effect]") {
		t.Fatal("system contract must distinguish read-only and policy-gated effect tools")
	}
	if !strings.Contains(first, "expected_before_hash") {
		t.Fatal("system contract missing the stale-state precondition rule")
	}
	if !strings.Contains(first, "No recipes are configured") {
		t.Fatal("system contract must state that no recipes are configured")
	}
	if !strings.Contains(first, "runstead.protocol.v1") {
		t.Fatal("system contract missing protocol version")
	}
	if !strings.Contains(first, "UNTRUSTED") {
		t.Fatal("system contract missing the untrusted-data rule")
	}
}

func TestLoopFinalIncompleteIsTerminal(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("incomplete", "I could not answer fully.", finalEvidence("obs-000001", "read_file")),
	)
	loop := h.loop(t, agent.Limits{})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeFinalIncomplete {
		t.Fatalf("outcome = %q, want final_incomplete", result.Outcome)
	}
	if result.Summary != "I could not answer fully." {
		t.Fatalf("summary = %q", result.Summary)
	}
}
