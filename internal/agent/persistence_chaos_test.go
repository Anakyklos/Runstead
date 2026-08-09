package agent_test

// Issue #13 - persistence failure injection at runtime level. A minimal
// test-only wrapper over state.Persistence fails a named persistence method
// after a deterministic number of calls, and the loop must prove the
// fail-closed contract for every boundary: no state advances past what was
// confirmed, no effect executes without a committed intent, no SQLite error
// is converted into task success, and an uncertain persistence outcome never
// becomes a completed task.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// errInjectedPersistenceFailure is the typed error the fault seam injects.
var errInjectedPersistenceFailure = errors.New("injected persistence failure")

// faultyPersistence wraps the real store and fails one named method after a
// deterministic number of calls. It is the smallest fault-injection seam that
// proves the persistence failure contract at the loop boundary; production
// code stays untouched.
type faultyPersistence struct {
	state.Persistence
	mu       sync.Mutex
	method   string
	after    int
	calls    int
	failures int
}

func (f *faultyPersistence) failNext(method string, after int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.method = method
	f.after = after
	f.calls = 0
}

func (f *faultyPersistence) maybeFail(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.method != method {
		return nil
	}
	f.calls++
	if f.calls >= f.after {
		f.failures++
		// Keep the seam armed only once: one injected failure per test.
		f.method = ""
		return fmt.Errorf("%w: %s", errInjectedPersistenceFailure, method)
	}
	return nil
}

func (f *faultyPersistence) CreateTask(ctx context.Context, record state.TaskRecord) error {
	if err := f.maybeFail("CreateTask"); err != nil {
		return err
	}
	return f.Persistence.CreateTask(ctx, record)
}

func (f *faultyPersistence) StartTask(ctx context.Context, taskID string) error {
	if err := f.maybeFail("StartTask"); err != nil {
		return err
	}
	return f.Persistence.StartTask(ctx, taskID)
}

func (f *faultyPersistence) RecordAction(ctx context.Context, record state.ActionRecord) (string, error) {
	if err := f.maybeFail("RecordAction"); err != nil {
		return "", err
	}
	return f.Persistence.RecordAction(ctx, record)
}

func (f *faultyPersistence) PrepareToolAttempt(ctx context.Context, record state.ToolAttemptPrepared) (string, error) {
	if err := f.maybeFail("PrepareToolAttempt"); err != nil {
		return "", err
	}
	return f.Persistence.PrepareToolAttempt(ctx, record)
}

func (f *faultyPersistence) CompleteToolAttempt(ctx context.Context, record state.ToolAttemptCompleted) error {
	if err := f.maybeFail("CompleteToolAttempt"); err != nil {
		return err
	}
	return f.Persistence.CompleteToolAttempt(ctx, record)
}

func (f *faultyPersistence) FinalizeTask(ctx context.Context, record state.TaskFinalize) error {
	if err := f.maybeFail("FinalizeTask"); err != nil {
		return err
	}
	return f.Persistence.FinalizeTask(ctx, record)
}

func (f *faultyPersistence) SaveVerificationAttempt(ctx context.Context, record state.VerificationAttemptRecord) error {
	if err := f.maybeFail("SaveVerificationAttempt"); err != nil {
		return err
	}
	return f.Persistence.SaveVerificationAttempt(ctx, record)
}

func (f *faultyPersistence) RecordProviderPrepared(ctx context.Context, record governor.ProviderPrepared) error {
	if err := f.maybeFail("RecordProviderPrepared"); err != nil {
		return err
	}
	return f.Persistence.(governor.Persistence).RecordProviderPrepared(ctx, record)
}

func (f *faultyPersistence) RecordProviderFinished(ctx context.Context, record governor.ProviderFinished) error {
	if err := f.maybeFail("RecordProviderFinished"); err != nil {
		return err
	}
	return f.Persistence.(governor.Persistence).RecordProviderFinished(ctx, record)
}

// persistenceChaosHarness is the loop composition over the fault wrapper.
type persistenceChaosHarness struct {
	store    *state.Store
	faulty   *faultyPersistence
	clock    *fakeClock
	governor *governor.Governor
	provider *scriptedProvider
	executor *agent.Executor
	registry *tools.Registry
	policy   policy.Policy
	traces   *traceCapture
}

func newPersistenceChaosHarness(t *testing.T, workspace string, responses ...provider.Response) *persistenceChaosHarness {
	t.Helper()
	clock := newFakeClock()
	config := governor.DefaultInstantConfig("policy-loop-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedProvider{clock: clock, pace: time.Millisecond, responses: append([]provider.Response(nil), responses...)}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return &persistenceChaosHarness{
		store:    store,
		faulty:   &faultyPersistence{Persistence: store},
		clock:    clock,
		governor: accountGovernor,
		provider: client,
		executor: executor,
		registry: registry,
		policy:   policy.NewStatic(allowAllPolicy(), storeApprovals(store)),
		traces:   &traceCapture{},
	}
}

func (h *persistenceChaosHarness) loop(t *testing.T, limits agent.Limits, plan *verifier.Plan) *agent.Loop {
	t.Helper()
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   limits,
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    h.faulty,
		Policy:   h.policy,
		Verifier: verifier.New(h.registry, plan),
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

// TestPersistenceChaosBeforeAnyStatePersists is the bootstrap boundary: a
// failure creating the durable task root stops the run fail-closed before any
// provider attempt, and no task row exists afterwards.
func TestPersistenceChaosCreateTaskFailure(t *testing.T) {
	workspace := t.TempDir()
	h := newPersistenceChaosHarness(t, workspace)
	h.faulty.failNext("CreateTask", 1)

	loop := h.loop(t, agent.Limits{}, nil)
	result := loop.Run(context.Background(), testTask("task-db-create"))
	if result.Outcome != agent.OutcomePersistenceFailure {
		t.Fatalf("outcome = %q, want persistence_failure (reason %q)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "injected persistence failure") {
		t.Fatalf("stop reason must carry the real error: %q", result.StopReason)
	}
	if _, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-create"); !errors.Is(err, state.ErrTaskNotFound) {
		t.Fatalf("no task row may exist after a failed CreateTask: %v", err)
	}
}

// TestPersistenceChaosActionRecordFailure proves a failed intent record stops
// the run before any effect: the tool never executes and the workspace stays
// untouched.
func TestPersistenceChaosActionRecordFailure(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newPersistenceChaosHarness(t, workspace, writeFixtureAction("a.txt", "bravo\n", "alpha\n"))
	// The write proposal is the first (and only) action: its intent record
	// fails, so the write effect must never start.
	h.faulty.failNext("RecordAction", 1)

	loop := h.loop(t, agent.Limits{}, nil)
	result := loop.Run(context.Background(), testTask("task-db-action"))
	if result.Outcome != agent.OutcomePersistenceFailure {
		t.Fatalf("outcome = %q, want persistence_failure (reason %q)", result.Outcome, result.StopReason)
	}
	content, err := readFileContent(workspace, "a.txt")
	if err != nil || content != "alpha\n" {
		t.Fatalf("workspace changed despite the failed intent: %q (err %v)", content, err)
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-action")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ToolAttempts) != 0 {
		t.Fatalf("no tool attempt may exist when the action intent failed: %+v", snapshot.ToolAttempts)
	}
}

// TestPersistenceChaosTX1FailureProvesEffectNeverStarts is the durable-intent
// boundary: when the TX 1 intent commit fails, the effect must not execute.
func TestPersistenceChaosTX1FailureProvesEffectNeverStarts(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newPersistenceChaosHarness(t, workspace, writeFixtureAction("a.txt", "bravo\n", "alpha\n"))
	h.faulty.failNext("PrepareToolAttempt", 1)

	loop := h.loop(t, agent.Limits{}, nil)
	result := loop.Run(context.Background(), testTask("task-db-tx1"))
	if result.Outcome != agent.OutcomePersistenceFailure {
		t.Fatalf("outcome = %q, want persistence_failure (reason %q)", result.Outcome, result.StopReason)
	}
	content, err := readFileContent(workspace, "a.txt")
	if err != nil || content != "alpha\n" {
		t.Fatalf("workspace changed despite the failed TX 1 intent: %q (err %v)", content, err)
	}
}

// TestPersistenceChaosTX2FailurePausesForRecovery is the uncertain-effect
// boundary: the write effect happened, the TX 2 result commit failed. The
// task must NOT advance to completed AND must NOT be finalized terminally: it
// pauses durably resumable with the prepared attempt intact, and the recovery
// pipeline reconciles the effect from the observable filesystem state instead
// of re-executing it (issue #13 review).
func TestPersistenceChaosTX2FailurePausesForRecovery(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newPersistenceChaosHarness(t, workspace, writeFixtureAction("a.txt", "bravo\n", "alpha\n"))
	h.faulty.failNext("CompleteToolAttempt", 1)

	loop := h.loop(t, agent.Limits{}, nil)
	result := loop.Run(context.Background(), testTask("task-db-tx2"))
	if result.Outcome != agent.OutcomePersistencePaused {
		t.Fatalf("outcome = %q, want persistence_paused (reason %q)", result.Outcome, result.StopReason)
	}
	// The effect DID happen (the file was written) but the result commit
	// failed: the durable attempt stays prepared, with no citable evidence.
	content, err := readFileContent(workspace, "a.txt")
	if err != nil || content != "bravo\n" {
		t.Fatalf("workspace = %q, want the executed write bravo\\n (err %v)", content, err)
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-tx2")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.Status != "running" {
		t.Fatalf("task status = %q, want running (resumable, not finalized)", snapshot.Task.Status)
	}
	if len(snapshot.ToolAttempts) != 1 || snapshot.ToolAttempts[0].Status != "prepared" {
		t.Fatalf("attempt must stay prepared after the failed TX 2: %+v", snapshot.ToolAttempts)
	}
	if len(snapshot.Evidence) != 0 {
		t.Fatalf("no citable evidence may exist after the failed TX 2: %+v", snapshot.Evidence)
	}
	rendered := renderedInspect(t, h.store, "task-db-tx2")
	if strings.Contains(rendered, "Outcome: completed") {
		t.Fatal("a failed result commit must never produce a completed task")
	}
	if !strings.Contains(rendered, "Outcome: persistence_paused") {
		t.Fatalf("inspect must show the typed persistence pause:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: running") {
		t.Fatalf("inspect must show the task as resumable:\n%s", rendered)
	}

	// Resume through the REAL recovery pipeline: the prepared write attempt is
	// reconciled from the current filesystem state (the file matches the
	// expected after-hash), never re-executed. The recovered seed then runs
	// the loop to a verified completion with a new provider conversation.
	plan, err := recovery.Resume(context.Background(), h.store, recovery.Options{TaskID: "task-db-tx2"})
	if err != nil {
		t.Fatalf("recovery.Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("recovery decision = %s, want continue (reason %s)", plan.Decision, plan.Reason)
	}
	if plan.ReconciledToolAttempts != 1 {
		t.Fatalf("reconciled tool attempts = %d, want 1", plan.ReconciledToolAttempts)
	}
	// The harness governor is not persistence-backed (unlike the CLI), so the
	// recovery seed cannot derive the continued turn counters from persisted
	// provider attempts; seed them explicitly so the resumed loop's request
	// ids stay fresh for the governor's dedup, exactly like the CLI resume
	// path does.
	plan.Seed.Turns = 1
	plan.Seed.Attempts = 1
	// The reconciled evidence must be citable in the resumed run and the write
	// attempt must be durably reconciled as completed from observable state.
	resumedSnapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-tx2")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumedSnapshot.Evidence) != 1 || resumedSnapshot.Evidence[0].Tool != "write_file" {
		t.Fatalf("reconciled evidence = %+v, want the write evidence", resumedSnapshot.Evidence)
	}
	if resumedSnapshot.ToolAttempts[0].Status != "reconciled" || resumedSnapshot.ToolAttempts[0].RecoveryReason != "write_effect_completed" {
		t.Fatalf("reconciled attempt = %+v, want write_effect_completed", resumedSnapshot.ToolAttempts[0])
	}
	// The resumed loop continues with a fresh provider that proposes the final
	// grounded on the reconciled evidence; the file is written exactly once.
	newProvider := &scriptedProvider{clock: h.clock, pace: time.Millisecond, responses: []provider.Response{
		finalResponse("complete", "done", finalEvidence("obs-000001", "write_file")),
	}}
	executor2, err := agent.NewExecutor(h.governor, newProvider, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop2, err := agent.NewLoop(agent.Config{
		Runner: executor2, Registry: h.registry, Limits: agent.Limits{MaxSteps: 10}, Clock: h.clock,
		State: h.store, Policy: h.policy, Recovery: plan.Seed,
		Verifier: verifier.New(h.registry, existsPlan("a.txt")),
	})
	if err != nil {
		t.Fatal(err)
	}
	result2 := loop2.Run(context.Background(), testTask("task-db-tx2"))
	if result2.Outcome != agent.OutcomeCompleted {
		t.Fatalf("resumed outcome = %q, want completed (reason %q)", result2.Outcome, result2.StopReason)
	}
	content, err = readFileContent(workspace, "a.txt")
	if err != nil || content != "bravo\n" {
		t.Fatalf("workspace = %q, want bravo\\n exactly once (err %v)", content, err)
	}
	counts := make(map[string]int)
	finalSnapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-tx2")
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range finalSnapshot.ToolAttempts {
		counts[attempt.Tool+"/"+attempt.Status]++
	}
	if counts["write_file/prepared"] != 0 || counts["write_file/reconciled"] != 1 || counts["write_file/completed"] != 0 {
		t.Fatalf("write attempt projections = %v, want exactly one reconciled attempt (no duplication)", counts)
	}
}

// TestPersistenceChaosFinalizeFailureLeavesResumableState is the terminal
// boundary: a failure persisting the final outcome must not advance the task
// past its confirmed state. The history (action + attempt) survives, the task
// stays running (resumable), and nothing becomes completed.
func TestPersistenceChaosFinalizeFailureLeavesResumableState(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newPersistenceChaosHarness(t, workspace,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
	)
	h.faulty.failNext("FinalizeTask", 1)

	loop := h.loop(t, agent.Limits{}, existsPlan("a.txt"))
	result := loop.Run(context.Background(), testTask("task-db-finalize"))
	if result.Outcome != agent.OutcomePersistenceFailure {
		t.Fatalf("outcome = %q, want persistence_failure (reason %q)", result.Outcome, result.StopReason)
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-finalize")
	if err != nil {
		t.Fatal(err)
	}
	// The task stays running: the finalize transaction rolled back atomically.
	if snapshot.Task.Status != "running" {
		t.Fatalf("task status = %q, want running (resumable, not completed)", snapshot.Task.Status)
	}
	if len(snapshot.Actions) != 1 || len(snapshot.ToolAttempts) != 1 {
		t.Fatalf("confirmed history must survive the finalize failure: %+v / %+v", snapshot.Actions, snapshot.ToolAttempts)
	}
}

// TestPersistenceChaosVerificationPersistFailureBlocksCompletion is the
// completion-gate boundary: when the verification attempt cannot be
// persisted, the completion proposal is refused blocked (the gate cannot be
// proven durable), the task stays resumable, and the model claim never
// becomes completed.
func TestPersistenceChaosVerificationPersistFailureBlocksCompletion(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newPersistenceChaosHarness(t, workspace,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
	)
	h.faulty.failNext("SaveVerificationAttempt", 1)

	loop := h.loop(t, agent.Limits{}, existsPlan("a.txt"))
	result := loop.Run(context.Background(), testTask("task-db-verify"))
	if result.Outcome != agent.OutcomeVerificationBlocked {
		t.Fatalf("outcome = %q, want verification_blocked (reason %q)", result.Outcome, result.StopReason)
	}
	snapshot, err := h.store.LoadRecoverySnapshot(context.Background(), "task-db-verify")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.Status != "running" {
		t.Fatalf("task status = %q, want running: an unprovable completion gate must leave the task resumable", snapshot.Task.Status)
	}
	if rendered := renderedInspect(t, h.store, "task-db-verify"); strings.Contains(rendered, "Outcome: completed") {
		t.Fatal("a failed verification persistence must never complete the task")
	}
}

// writeFixtureAction builds a write_file action against the fixture file,
// with the expected before-hash computed from the given before content.
func writeFixtureAction(path, content, before string) provider.Response {
	return actionResponse("write_file", `{"path":"`+path+`","content":`+mustJSONString(content)+`,"expected_before_hash":"`+tools.HashBytes([]byte(before))+`"}`)
}
