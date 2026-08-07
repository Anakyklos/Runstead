package agent_test

import (
	"context"
	"encoding/json"
	"os"
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
)

// fakeClock implements both the governor clock and the loop clock so task
// deadlines and trace durations are deterministic. It starts in the future so
// real context timers derived from fake deadlines never fire during a test.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	c     chan time.Time
	mu    sync.Mutex
	stop  bool
	fired bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop || t.fired {
		return false
	}
	t.stop = true
	return true
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(delay time.Duration) governor.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, at: c.now.Add(delay), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	timers := append([]*fakeTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.mu.Lock()
		if !timer.stop && !timer.fired && !now.Before(timer.at) {
			timer.fired = true
			timer.c <- now
		}
		timer.mu.Unlock()
	}
}

// pendingTimers reports the number of live un-fired timers. Tests use it to
// observe that the governor is blocked waiting on a specific clock timer.
func (c *fakeClock) pendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.timers {
		timer.mu.Lock()
		if !timer.stop && !timer.fired {
			count++
		}
		timer.mu.Unlock()
	}
	return count
}

type fixedJitter struct{}

func (fixedJitter) Apply(base time.Duration, _ int) time.Duration { return base }

type traceCapture struct {
	mu    sync.Mutex
	lines []agent.TraceLine
}

func (c *traceCapture) emit(line agent.TraceLine) {
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
}

func (c *traceCapture) all() []agent.TraceLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]agent.TraceLine(nil), c.lines...)
}

// scriptedProvider advances the shared clock by pace before each response,
// simulating provider I/O time so governor pacing timers fire deterministically
// between turns. It records every prompt for assertions and can be forced to
// fail a call.
type scriptedProvider struct {
	mu        sync.Mutex
	clock     *fakeClock
	pace      time.Duration
	responses []provider.Response
	attempts  int
	prompts   []string
	failWith  error
	mutate    func(int)
	shared    *concurrencyTracker
}

type concurrencyTracker struct {
	mu     sync.Mutex
	active int
	max    int
}

func (t *concurrencyTracker) enter() {
	t.mu.Lock()
	t.active++
	if t.active > t.max {
		t.max = t.active
	}
	t.mu.Unlock()
}

func (t *concurrencyTracker) leave() {
	t.mu.Lock()
	t.active--
	t.mu.Unlock()
}

func (t *concurrencyTracker) maxConcurrent() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.max
}

func (p *scriptedProvider) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (p *scriptedProvider) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.attempts++
	attempt := p.attempts
	p.prompts = append(p.prompts, request.Prompt)
	var response provider.Response
	var err error
	if len(p.responses) > 0 {
		response = p.responses[0]
		p.responses = p.responses[1:]
	} else if p.failWith != nil {
		err = p.failWith
	} else {
		err = provider.ErrNoPredefinedResponse
	}
	mutate := p.mutate
	shared := p.shared
	p.mu.Unlock()

	if shared != nil {
		shared.enter()
		defer shared.leave()
	}
	if p.pace > 0 {
		p.clock.Advance(p.pace)
	}
	if mutate != nil {
		mutate(attempt)
	}
	select {
	case <-ctx.Done():
		return provider.Response{}, ctx.Err()
	default:
	}
	if err != nil {
		return provider.Response{}, err
	}
	return response, nil
}

func (p *scriptedProvider) Attempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

func (p *scriptedProvider) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

type harness struct {
	clock    *fakeClock
	governor *governor.Governor
	provider *scriptedProvider
	executor *agent.Executor
	registry *tools.Registry
	traces   *traceCapture
}

func newHarness(t *testing.T, workspace string, configure func(*governor.Config), responses ...provider.Response) *harness {
	t.Helper()
	return newHarnessPaced(t, workspace, configure, time.Millisecond, responses...)
}

func newHarnessPaced(t *testing.T, workspace string, configure func(*governor.Config), pace time.Duration, responses ...provider.Response) *harness {
	t.Helper()
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	if configure != nil {
		configure(&config)
	}
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	client := &scriptedProvider{clock: clock, pace: pace, responses: append([]provider.Response(nil), responses...)}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatalf("agent.NewExecutor() error = %v", err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("tools.NewRegistry() error = %v", err)
	}
	return &harness{
		clock:    clock,
		governor: accountGovernor,
		provider: client,
		executor: executor,
		registry: registry,
		traces:   &traceCapture{},
	}
}

func (h *harness) loop(t *testing.T, limits agent.Limits) *agent.Loop {
	t.Helper()
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   limits,
		Clock:    h.clock,
		Trace:    h.traces.emit,
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	return loop
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func actionResponse(tool string, arguments string) provider.Response {
	return provider.Response{Text: "<runstead_action>\n{\"version\":\"runstead.protocol.v1\",\"tool\":\"" + tool + "\",\"arguments\":" + arguments + "}\n</runstead_action>"}
}

func finalResponse(status, summary string, evidence ...string) provider.Response {
	encoded, _ := json.Marshal(evidence)
	return provider.Response{Text: "<runstead_final>\n{\"version\":\"runstead.protocol.v1\",\"status\":\"" + status + "\",\"summary\":\"" + summary + "\",\"evidence\":" + string(encoded) + "}\n</runstead_final>"}
}

func testTask(id string) agent.Task {
	return agent.Task{ID: id, Prompt: "Inspect the repository and answer the question."}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal(message)
}

func waitForGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if runtime.NumGoroutine() > before+1 {
		t.Fatalf("goroutines remained after run: before=%d after=%d", before, runtime.NumGoroutine())
	}
}

func TestLoopCompletesGroundedMultiTurn(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("list_files", `{"path":"."}`),
		finalResponse("complete", "Inspected the fixture repository.", "obs-000001", "obs-000002"),
	)
	loop := h.loop(t, agent.Limits{})

	result := loop.Run(context.Background(), testTask("task-1"))

	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q stop_reason=%q, want completed", result.Outcome, result.StopReason)
	}
	if result.Turns != 3 || result.Attempts != 3 || result.Observations != 2 {
		t.Fatalf("run stats = %#v, want 3 turns, 3 attempts, 2 observations", result)
	}
	if len(result.Evidence) != 2 || result.Evidence[0] != "obs-000001" || result.Evidence[1] != "obs-000002" {
		t.Fatalf("evidence = %v", result.Evidence)
	}
	if h.provider.Attempts() != 3 {
		t.Fatalf("provider attempts = %d, want 3", h.provider.Attempts())
	}
	lines := h.traces.all()
	if len(lines) == 0 || lines[len(lines)-1].Kind != agent.TraceStop || lines[len(lines)-1].Status != string(agent.OutcomeCompleted) {
		t.Fatalf("trace did not end with a completed stop line: %#v", lines)
	}
	kinds := traceKinds(lines)
	for _, want := range []string{agent.TraceAttempt, agent.TraceAction, agent.TraceObservation, agent.TraceStop} {
		if !kinds[want] {
			t.Errorf("trace missing %q lines: %v", want, kinds)
		}
	}
}

func traceKinds(lines []agent.TraceLine) map[string]bool {
	seen := make(map[string]bool)
	for _, line := range lines {
		seen[line.Kind] = true
	}
	return seen
}

func TestLoopObservationReturnsUntrustedDataNotSystemInstructions(t *testing.T) {
	workspace := t.TempDir()
	secret := "repo-secret-value-that-must-not-become-a-policy"
	writeFixture(t, workspace, "a.txt", secret)
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}
	prompts := h.provider.Requests()
	if len(prompts) < 2 {
		t.Fatalf("provider turns = %d, want at least 2", len(prompts))
	}
	contractSection := extractSection(prompts[0], "system")
	if strings.Contains(contractSection, secret) {
		t.Fatal("repository content leaked into the system contract")
	}
	if !strings.Contains(prompts[0], "read_file") {
		t.Fatal("system contract does not describe the registered tools")
	}
	followUp := prompts[1]
	if !strings.Contains(followUp, "=== runstead:observation ===") {
		t.Fatal("observation was not returned under the observation role")
	}
	if !strings.Contains(followUp, `"untrusted":true`) {
		t.Fatal("observation lost the untrusted marker")
	}
	if strings.Contains(extractSection(followUp, "system"), secret) {
		t.Fatal("repository content entered the system section of a later prompt")
	}
}

func extractSection(prompt, role string) string {
	marker := "=== runstead:" + role + " ==="
	start := strings.Index(prompt, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	next := strings.Index(prompt[start:], "=== runstead:")
	if next < 0 {
		return prompt[start:]
	}
	return prompt[start : start+next]
}

func TestLoopInvalidActionCorrectionThenSuccess(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		provider.Response{Text: "<runstead_action>\n{\"version\":\"runstead.protocol.v1\",\"tool\":\"read_file\",\"arguments\":{\"path\":42}}\n</runstead_action>"},
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "done", "obs-000001"),
	)
	loop := h.loop(t, agent.Limits{MaxCorrections: 2})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q stop_reason=%q, want completed", result.Outcome, result.StopReason)
	}
	if result.Corrections != 1 {
		t.Fatalf("corrections = %d, want 1", result.Corrections)
	}
	lines := h.traces.all()
	found := false
	for _, line := range lines {
		if line.Kind == agent.TraceCorrection && line.Code == "invalid_arguments" {
			found = true
			if line.RetriesRemaining != 1 {
				t.Fatalf("correction retries = %d, want 1", line.RetriesRemaining)
			}
		}
	}
	if !found {
		t.Fatal("trace has no invalid_arguments correction line")
	}
}

func TestLoopCorrectionsExhausted(t *testing.T) {
	workspace := t.TempDir()
	h := newHarness(t, workspace, nil,
		provider.Response{Text: "I refuse to use the protocol."},
		provider.Response{Text: "I refuse to use the protocol."},
		provider.Response{Text: "I refuse to use the protocol."},
	)
	loop := h.loop(t, agent.Limits{MaxCorrections: 2})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCorrectionsExhausted {
		t.Fatalf("outcome = %q, want corrections_exhausted", result.Outcome)
	}
	if result.Corrections != 2 {
		t.Fatalf("corrections = %d, want 2", result.Corrections)
	}
	if h.provider.Attempts() != 3 {
		t.Fatalf("provider attempts = %d, want 3 (initial plus two corrections)", h.provider.Attempts())
	}
}

func TestLoopRepeatedActionGuardStopsWithoutReExecution(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
	)
	loop := h.loop(t, agent.Limits{MaxRepeatedActions: 1})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeRepeatedAction {
		t.Fatalf("outcome = %q stop_reason=%q, want repeated_action", result.Outcome, result.StopReason)
	}
	if result.Observations != 1 {
		t.Fatalf("observations = %d, want exactly one tool execution", result.Observations)
	}
	if result.Repeated != 2 {
		t.Fatalf("repeated corrections = %d, want 2", result.Repeated)
	}
	lines := h.traces.all()
	executions := 0
	for _, line := range lines {
		if line.Kind == agent.TraceAction {
			executions++
		}
	}
	if executions != 1 {
		t.Fatalf("tool executions in trace = %d, want 1", executions)
	}
}

func TestLoopRepeatedActionAllowedAfterWorkspaceChange(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "version-one\n")
	h := newHarnessPaced(t, workspace, nil, time.Millisecond,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "observed both versions", "obs-000001", "obs-000002"),
	)
	h.provider.mutate = func(attempt int) {
		if attempt == 3 {
			if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("version-two\n"), 0o644); err != nil {
				t.Errorf("workspace mutation: %v", err)
			}
		}
	}
	loop := h.loop(t, agent.Limits{MaxRepeatedActions: 2})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q stop_reason=%q, want completed after workspace change", result.Outcome, result.StopReason)
	}
	if result.Observations != 2 {
		t.Fatalf("observations = %d, want the repeated action re-executed after the workspace changed", result.Observations)
	}
	if result.Repeated != 1 {
		t.Fatalf("repeated corrections = %d, want exactly one repeat before the change", result.Repeated)
	}
}

func TestLoopFabricatedEvidenceRejected(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "I invented an observation.", "obs-999999"),
	)
	loop := h.loop(t, agent.Limits{})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeFinalNotGrounded {
		t.Fatalf("outcome = %q, want final_not_grounded", result.Outcome)
	}
	if len(result.Evidence) != 1 || result.Evidence[0] != "obs-999999" {
		t.Fatalf("missing evidence = %v", result.Evidence)
	}
}

func TestLoopStepsExhausted(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
	)
	loop := h.loop(t, agent.Limits{MaxSteps: 2})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeStepsExhausted {
		t.Fatalf("outcome = %q, want steps_exhausted", result.Outcome)
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d, want 2", result.Turns)
	}
}

func TestLoopTimeBudgetExhausted(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	// Each provider turn advances the clock by 20m; the budget is 30m, so the
	// deadline fires before the third turn starts.
	h := newHarnessPaced(t, workspace, nil, 20*time.Minute,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("list_files", `{"path":"."}`),
	)
	loop := h.loop(t, agent.Limits{TimeBudget: 30 * time.Minute})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeTimeBudgetExhausted {
		t.Fatalf("outcome = %q, want time_budget_exhausted", result.Outcome)
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d, want 2 before the budget fired", result.Turns)
	}
}

func TestLoopProviderBudgetExhausted(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil,
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"a.txt"}`),
	)
	loop := h.loop(t, agent.Limits{ProviderBudget: 2})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeProviderBudgetExhausted {
		t.Fatalf("outcome = %q, want provider_budget_exhausted", result.Outcome)
	}
	if h.provider.Attempts() != 2 {
		t.Fatalf("provider attempts = %d, want 2", h.provider.Attempts())
	}
}

func TestLoopAccountDelayTimeout(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newHarness(t, workspace, nil)
	// Warm the governor into a long cooldown so admission is delayed by the
	// account lane with a timer the fake clock can fire deterministically.
	admission := h.governor.TryAdmit(context.Background(), governor.AttemptRequest{
		TaskID:          "warmup",
		ClientRequestID: "warmup-1",
	})
	if !admission.Admitted() {
		t.Fatalf("warmup admission = %#v", admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	if completion := admission.Permit.Finish(governor.Outcome{Class: governor.OutcomeRateCapacity, RetryAfter: time.Hour}); completion.Err != nil {
		t.Fatalf("warmup finish = %#v", completion)
	}
	h.governor.DrainEvents()
	if snapshot := h.governor.Snapshot(); !snapshot.CooldownUntil.After(h.clock.Now()) {
		t.Fatalf("warmup did not arm a cooldown: %#v", snapshot)
	}

	loop := h.loop(t, agent.Limits{TimeBudget: 30 * time.Minute})
	before := runtime.NumGoroutine()
	done := make(chan agent.Result, 1)
	go func() { done <- loop.Run(context.Background(), testTask("task-1")) }()

	// The loop's first turn blocks in the governor's cooldown wait. The test
	// observes the pending cooldown timer, so the clock advances only after the
	// wait is established: the first step passes the task deadline while the
	// cooldown is still active, and the second step fires the cooldown timer so
	// admission re-checks the deadline and reports an account delay.
	waitFor(t, func() bool { return h.clock.pendingTimers() >= 1 }, "task never blocked in the cooldown wait")
	h.clock.Advance(31 * time.Minute)
	h.clock.Advance(31 * time.Minute)

	result := <-done
	if result.Outcome != agent.OutcomeAccountDelayTimeout {
		t.Fatalf("outcome = %q stop_reason=%q, want account_delay_timeout", result.Outcome, result.StopReason)
	}
	waitForGoroutines(t, before)
}

func TestLoopCanceledWhileQueuedConsumesNoAttempt(t *testing.T) {
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
	traces := &traceCapture{}
	newLoop := func() *agent.Loop {
		loop, err := agent.NewLoop(agent.Config{Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock, Trace: traces.emit})
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
	doneB := make(chan agent.Result, 1)
	go func() { doneB <- newLoop().Run(ctxB, testTask("task-b")) }()
	waitFor(t, func() bool { return accountGovernor.Snapshot().QueueLength >= 1 }, "task B never queued behind A")
	cancelB()

	resultB := <-doneB
	if resultB.Outcome != agent.OutcomeCanceled {
		t.Fatalf("queued task outcome = %q, want canceled", resultB.Outcome)
	}
	if blocking.Attempts() != 1 {
		t.Fatalf("provider attempts after B cancellation = %d, want 1 (task A only)", blocking.Attempts())
	}

	// Release task A; its attempt was conservatively debited because the
	// upstream may have been reached.
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

func TestLoopAccountCircuitOpen(t *testing.T) {
	workspace := t.TempDir()
	h := newHarness(t, workspace, nil, provider.Response{Text: "any response"})
	// Open the account circuit through the governor before the loop runs.
	admission := h.governor.TryAdmit(context.Background(), governor.AttemptRequest{
		TaskID:          "warmup",
		ClientRequestID: "warmup-1",
	})
	if !admission.Admitted() {
		t.Fatalf("warmup admission = %#v", admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	h.governor.DrainEvents()
	if completion := admission.Permit.Finish(governor.Outcome{Class: governor.OutcomeAuthenticationDenied, UpstreamReached: true}); completion.Err != nil {
		t.Fatalf("warmup finish = %#v", completion)
	}
	h.governor.DrainEvents()
	if snapshot := h.governor.Snapshot(); snapshot.Circuit.State != governor.CircuitHumanReviewRequired {
		t.Fatalf("circuit state = %q, want human_review_required", snapshot.Circuit.State)
	}

	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeAccountCircuitOpen {
		t.Fatalf("outcome = %q, want account_circuit_open", result.Outcome)
	}
}

func TestLoopProviderFailureClassified(t *testing.T) {
	workspace := t.TempDir()
	h := newHarness(t, workspace, nil)
	h.provider.responses = nil
	h.provider.failWith = provider.ErrNoPredefinedResponse
	loop := h.loop(t, agent.Limits{})

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure", result.Outcome)
	}
	if result.Classification == "" {
		t.Fatal("provider failure lost its classification")
	}
}
