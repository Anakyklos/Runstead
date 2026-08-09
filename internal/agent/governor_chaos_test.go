package agent_test

// Issue #13 - governor / #58 chaos at runtime level. The PR #61 unit tests
// prove the unlimited-text/unknown allowance semantics at the governor layer;
// these tests prove the same invariants through the REAL loop and the REAL
// durable store: unlimited_text never means ungoverned, unknown never
// promotes itself, serialization holds across concurrent loops, hidden
// amplification stays rejected under unlimited text, and severe signals open
// the fail-closed circuit exactly as under any other allowance.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// TestUnlimitedTextRuntimeSevereSignalOpensHumanReviewCircuit proves a
// CAPTCHA (or any severe auth/suspicion signal) under the unlimited-text
// allowance still opens the human-review circuit at runtime: the attempt that
// hits the CAPTCHA is a classified provider failure, the circuit opens and
// survives in the durable governor projection, and the NEXT run is refused
// with the typed account_circuit_open outcome. The task never completes.
func TestUnlimitedTextRuntimeSevereSignalOpensHumanReviewCircuit(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	h := newChaosHarnessUnlimited(t, workspace, true, nil,
		chaosStep{err: typedError(omniroute.ErrorCAPTCHA)},
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-unlimited-captcha"))
	// The attempt that HITS the CAPTCHA is a classified provider failure that
	// opens the circuit; a provider failure is terminal, so the run stops
	// here with the typed classification preserved.
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure (reason %q)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "captcha") {
		t.Fatalf("stop reason must carry the typed classification: %q", result.StopReason)
	}
	if h.provider.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1 (no hidden retry after the CAPTCHA)", h.provider.Attempts())
	}
	// Exactly one governed attempt was debited and the circuit is open.
	snapshot := h.governor.Snapshot()
	if snapshot.Circuit.State != governor.CircuitHumanReviewRequired {
		t.Fatalf("circuit = %s, want human_review_required under unlimited text", snapshot.Circuit.State)
	}
	if snapshot.Budgets.Rolling3hUsed != 1 {
		t.Fatalf("ledger usage = %d, want 1 (the attempt was accounted, unlimited or not)", snapshot.Budgets.Rolling3hUsed)
	}
	// The allowance remains explicitly unlimited_text; the circuit is a
	// separate account-safety layer that never disappears.
	if snapshot.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("allowance kind = %q, want unlimited_text", snapshot.AllowanceKind)
	}
	// The circuit survives in the durable governor projection (restart-safe).
	persisted, ok, err := h.store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = ok %t err %v", ok, err)
	}
	if persisted.Circuit.State != governor.CircuitHumanReviewRequired {
		t.Fatalf("persisted circuit = %s, want human_review_required", persisted.Circuit.State)
	}
	rendered := renderedInspect(t, h.store, "task-unlimited-captcha")
	if strings.Contains(rendered, "Outcome: completed") {
		t.Fatal("a CAPTCHA under unlimited text must never produce a completed task")
	}

	// The NEXT run under the same account is held by the open circuit: the
	// fail-closed gate is enforced at runtime, not only inside the governor.
	nextProvider := &chaosProvider{clock: h.clock, pace: time.Millisecond, sessionPrefix: "gen-2", steps: []chaosStep{
		{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`))},
	}}
	executor2, err := agent.NewExecutor(h.governor, nextProvider, omniroute.Classify)
	if err != nil {
		t.Fatal(err)
	}
	loop2, err := agent.NewLoop(agent.Config{
		Runner: executor2, Registry: h.registry, Limits: agent.Limits{}, Clock: h.clock,
		State: h.store, Policy: h.policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	result2 := loop2.Run(context.Background(), testTask("task-unlimited-captcha-2"))
	if result2.Outcome != agent.OutcomeAccountCircuitOpen {
		t.Fatalf("second run outcome = %q, want account_circuit_open (reason %q)", result2.Outcome, result2.StopReason)
	}
	if nextProvider.Attempts() != 0 {
		t.Fatalf("the open circuit must refuse admission before any provider call; attempts = %d", nextProvider.Attempts())
	}
}

// TestUnlimitedTextRuntimeConcurrentLoopsStaySerialized proves the one
// in-flight attempt contract holds across two real concurrent loops sharing
// one unlimited-text account lane.
func TestUnlimitedTextRuntimeConcurrentLoopsStaySerialized(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	writeFixture(t, workspaceA, "a.txt", "alpha\n")
	writeFixture(t, workspaceB, "b.txt", "beta\n")

	clock := newFakeClock()
	config := governor.DefaultLunaUnlimitedTextConfig("policy-unlimited", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.TaskBudget = 10
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	tracker := &concurrencyTracker{}
	clientA := &scriptedProvider{clock: clock, pace: time.Millisecond, shared: tracker, responses: []provider.Response{
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "inspected A", finalEvidence("obs-000001", "read_file")),
	}}
	clientB := &scriptedProvider{clock: clock, pace: time.Millisecond, shared: tracker, responses: []provider.Response{
		actionResponse("read_file", `{"path":"b.txt"}`),
		finalResponse("complete", "inspected B", finalEvidence("obs-000001", "read_file")),
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

	doneA := make(chan agent.Result, 1)
	doneB := make(chan agent.Result, 1)
	go func() { doneA <- loopA.Run(context.Background(), testTask("task-ul-a")) }()
	go func() { doneB <- loopB.Run(context.Background(), testTask("task-ul-b")) }()
	resultA := <-doneA
	resultB := <-doneB
	if resultA.Outcome != agent.OutcomeCompleted || resultB.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcomes = %q / %q, want both completed", resultA.Outcome, resultB.Outcome)
	}
	if got := tracker.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent provider calls = %d, want exactly 1 under unlimited text", got)
	}
	snapshot := accountGovernor.Snapshot()
	if snapshot.InFlight || snapshot.QueueLength != 0 {
		t.Fatalf("lane not released: %#v", snapshot)
	}
	if snapshot.Tasks["task-ul-a"].Attempts != 2 || snapshot.Tasks["task-ul-b"].Attempts != 2 {
		t.Fatalf("task accounting = %#v, want 2 attempts each", snapshot.Tasks)
	}
}

// TestUnknownRuntimeCeilingsStopLoopAndNeverPromote proves the conservative
// local ceilings of the unknown allowance stop a real loop (typed
// account_delay_timeout) and that repeated success never promotes the
// allowance semantic from unknown to unlimited.
func TestUnknownRuntimeCeilingsStopLoopAndNeverPromote(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	writeFixture(t, workspace, "b.txt", "beta\n")
	writeFixture(t, workspace, "c.txt", "gamma\n")

	clock := newFakeClock()
	config := governor.DefaultUnknownConfig("policy-unknown", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Rolling10m = 2
	config.Rolling1h = 3
	config.Rolling3h = 5
	config.ManualReserve = 1
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedProvider{clock: clock, pace: time.Millisecond, responses: []provider.Response{
		actionResponse("read_file", `{"path":"a.txt"}`),
		actionResponse("read_file", `{"path":"b.txt"}`),
		actionResponse("read_file", `{"path":"c.txt"}`),
	}}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner: executor, Registry: registry, Limits: agent.Limits{TimeBudget: 30 * time.Minute}, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan agent.Result, 1)
	go func() { done <- loop.Run(context.Background(), testTask("task-unknown")) }()

	// The first two turns succeed; the third admission delays on the local 10m
	// ceiling with a timer the fake clock can fire deterministically.
	waitFor(t, func() bool { return clock.pendingTimers() >= 1 }, "task never blocked on the unknown local ceiling")
	clock.Advance(31 * time.Minute)

	result := <-done
	if result.Outcome != agent.OutcomeAccountDelayTimeout {
		t.Fatalf("outcome = %q, want account_delay_timeout (reason %q)", result.Outcome, result.StopReason)
	}
	snapshot := accountGovernor.Snapshot()
	// Success never promoted the allowance: the kind stays unknown and the
	// conservative local ceilings are still in force.
	if snapshot.AllowanceKind != governor.AllowanceKindUnknown {
		t.Fatalf("allowance kind = %q, want unknown (repeated success never promotes)", snapshot.AllowanceKind)
	}
	// The 31-minute advance rolled the 10m window; the 1h/3h windows still
	// hold the two debits, and the third attempt never started.
	if snapshot.Budgets.Rolling10mCeiling != 2 || snapshot.Budgets.Rolling1hUsed != 2 {
		t.Fatalf("local ceiling state = %#v, want 10m ceiling 2 with 1h usage 2 (the third attempt never started)", snapshot.Budgets)
	}
	if snapshot.Tasks["task-unknown"].Attempts != 2 {
		t.Fatalf("task attempts = %d, want 2 (admission was delayed, no attempt started)", snapshot.Tasks["task-unknown"].Attempts)
	}
}

// receiptChaosProvider is the receipt-aware fake used by the runtime hidden
// amplification tests. It returns a scripted response text plus a scripted
// receipt set per call.
type receiptChaosProvider struct {
	clock     *fakeClock
	set       func(now time.Time, requestID string) provider.AttemptReceiptSet
	responses []provider.Response
	calls     int
}

func (p *receiptChaosProvider) RouteSafety() provider.RouteSafety {
	return provider.ReceiptRouteSafety()
}

func (p *receiptChaosProvider) AttemptReceiptsEnabled() bool { return true }

func (p *receiptChaosProvider) Complete(_ context.Context, request provider.Request) (provider.Response, error) {
	p.calls++
	// Simulate provider I/O so the governor pacing timers fire
	// deterministically between turns.
	p.clock.Advance(time.Millisecond)
	set := p.set(p.clock.Now(), request.ClientRequestID)
	text := "response"
	if len(p.responses) > 0 {
		text = p.responses[0].Text
		p.responses = p.responses[1:]
	}
	return provider.Response{
		Text:     text,
		Metadata: provider.ResponseMetadata{AttemptReceipts: &set},
	}, nil
}

// singleReceipt builds one authoritative receipt for the request. The attempt
// id is request-unique so the governor's replay protection never confuses one
// logical attempt with another.
func singleReceipt(now time.Time, requestID string) provider.AttemptReceiptSet {
	return provider.AttemptReceiptSet{
		SchemaVersion:   provider.AttemptReceiptSchemaVersion,
		ClientRequestID: requestID,
		Finalized:       true,
		Receipts: []provider.AttemptReceipt{{
			SchemaVersion:   provider.AttemptReceiptSchemaVersion,
			AttemptID:       "upstream-attempt-" + requestID + "-1",
			ClientRequestID: requestID,
			Sequence:        1,
			Provider:        "provider",
			Model:           "concrete-model",
			AccountLaneHash: "lane",
			StartedAt:       now.Add(-time.Second),
			CompletedAt:     now,
			Outcome:         provider.AttemptOutcomeSuccess,
			Trigger:         provider.AttemptTriggerInitial,
			UpstreamReached: true,
		}},
	}
}

// amplifiedReceipt builds TWO fresh authoritative receipts for ONE logical
// request: the shape a hidden upstream retry would produce.
func amplifiedReceipt(now time.Time, requestID string) provider.AttemptReceiptSet {
	return provider.AttemptReceiptSet{
		SchemaVersion:   provider.AttemptReceiptSchemaVersion,
		ClientRequestID: requestID,
		Finalized:       true,
		Receipts: []provider.AttemptReceipt{
			{
				SchemaVersion:   provider.AttemptReceiptSchemaVersion,
				AttemptID:       "upstream-attempt-" + requestID + "-1",
				ClientRequestID: requestID,
				Sequence:        1,
				Provider:        "provider",
				Model:           "concrete-model",
				AccountLaneHash: "lane",
				StartedAt:       now.Add(-2 * time.Second),
				CompletedAt:     now.Add(-time.Second),
				Outcome:         provider.AttemptOutcomeSuccess,
				Trigger:         provider.AttemptTriggerInitial,
				UpstreamReached: true,
			},
			{
				SchemaVersion:   provider.AttemptReceiptSchemaVersion,
				AttemptID:       "upstream-attempt-" + requestID + "-2",
				ClientRequestID: requestID,
				Sequence:        2,
				Provider:        "provider",
				Model:           "concrete-model",
				AccountLaneHash: "lane",
				StartedAt:       now.Add(-time.Second),
				CompletedAt:     now,
				Outcome:         provider.AttemptOutcomeSuccess,
				Trigger:         provider.AttemptTriggerExecutorRetry,
				UpstreamReached: true,
			},
		},
	}
}

// unlimitedReceiptHarness wires a receipt-aware unlimited-text governor, the
// receipt chaos provider and the real store into one loop composition.
func unlimitedReceiptHarness(t *testing.T, workspace string, build func(time.Time, string) provider.AttemptReceiptSet, responses ...provider.Response) (*agent.Loop, *governor.Governor, *receiptChaosProvider, *state.Store) {
	t.Helper()
	clock := newFakeClock()
	config := governor.DefaultLunaUnlimitedTextConfig("policy-unlimited", "provider", "model", provider.ReceiptRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Model = "concrete-model"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AttemptProviderID = "provider"
	config.AccountLaneHash = "lane"
	store, err := state.Open(state.Options{Path: t.TempDir() + "/runstead.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}, Persistence: store})
	if err != nil {
		t.Fatal(err)
	}
	client := &receiptChaosProvider{clock: clock, set: build, responses: append([]provider.Response(nil), responses...)}
	executor, err := agent.NewExecutor(accountGovernor, client, omniroute.Classify)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock,
		State: store, Verifier: verifier.New(registry, existsPlan("a.txt")),
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop, accountGovernor, client, store
}

// TestUnlimitedTextRuntimeSingleReceiptCompletes is the positive control:
// with one authoritative receipt per logical attempt the receipt-aware
// unlimited loop completes normally, debiting exactly one attempt.
func TestUnlimitedTextRuntimeSingleReceiptCompletes(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	loop, accountGovernor, client, store := unlimitedReceiptHarness(t, workspace, singleReceipt,
		actionResponse("read_file", `{"path":"a.txt"}`),
		finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")),
	)
	result := loop.Run(context.Background(), testTask("task-receipt-ok"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed (reason %q)", result.Outcome, result.StopReason)
	}
	snapshot := accountGovernor.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 2 || snapshot.Tasks["task-receipt-ok"].Attempts != 2 {
		t.Fatalf("receipt accounting = %#v, want one debit per logical attempt", snapshot)
	}
	if snapshot.Telemetry.Unsafe {
		t.Fatal("single-receipt traffic must not mark telemetry unsafe")
	}
	if client.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (one per logical attempt)", client.calls)
	}
	_ = store
}

// TestUnlimitedTextRuntimeHiddenAmplificationFailsClosed proves hidden
// upstream amplification under the unlimited-text allowance is rejected end
// to end: every authoritative receipt is debited (two debits), telemetry
// becomes unsafe, the account lane blocks, and the task can never complete.
func TestUnlimitedTextRuntimeHiddenAmplificationFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	loop, accountGovernor, client, store := unlimitedReceiptHarness(t, workspace, amplifiedReceipt,
		actionResponse("read_file", `{"path":"a.txt"}`),
	)
	result := loop.Run(context.Background(), testTask("task-receipt-amp"))
	if result.Outcome == agent.OutcomeCompleted {
		t.Fatal("hidden amplification must never produce a completed task")
	}
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure (reason %q)", result.Outcome, result.StopReason)
	}
	if client.calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (no retry of an amplified attempt)", client.calls)
	}
	snapshot := accountGovernor.Snapshot()
	// The receipts prove two upstream attempts: both are debited and the
	// accounting turns unsafe (fail closed), exactly as under #29/#58.
	if snapshot.Budgets.Rolling3hUsed != 2 || snapshot.Tasks["task-receipt-amp"].Attempts != 2 {
		t.Fatalf("amplified accounting = %#v, want two authoritative debits", snapshot)
	}
	if !snapshot.Telemetry.Unsafe {
		t.Fatal("hidden amplification must mark telemetry unsafe")
	}
	// The lane stays blocked for any further attempt.
	if blocked := accountGovernor.TryAdmit(context.Background(), governor.AttemptRequest{TaskID: "task-next", ClientRequestID: "next-1"}); blocked.Code != governor.AdmissionUnsafeProviderAmplification {
		t.Fatalf("post-amplification admission = %#v, want unsafe_provider_amplification", blocked)
	}
	// The durable attempt row carries the two debits and the structural
	// receipt error, so the accounting is auditably preserved across restart.
	recovery, err := store.LoadRecoverySnapshot(context.Background(), "task-receipt-amp")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.ProviderAttempts) != 1 || recovery.ProviderAttempts[0].AttemptDebited != 2 {
		t.Fatalf("durable amplified attempt = %+v, want exactly two debits", recovery.ProviderAttempts)
	}
	// The amplified attempt is conservatively uncertain: the accounting is
	// unsafe and the lane is blocked, never a silent success.
	if recovery.ProviderAttempts[0].Status != "uncertain" {
		t.Fatalf("durable amplified attempt status = %q, want uncertain", recovery.ProviderAttempts[0].Status)
	}
}

// TestProfileTransitionAcrossRuntimeRestartPreservesDurableProtection proves
// the #58 profile-transition invariant at runtime level: rebuilding the
// governor under the unlimited_text profile from the persisted projection of
// a published_quota run carries the ledger, task attempts and cooldown over;
// a profile change never creates a fresh logical account.
func TestProfileTransitionAcrossRuntimeRestartPreservesDurableProtection(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Phase 1: published_quota run with persistence: two successful attempts
	// and one rate/capacity outcome that arms a 15s cooldown.
	clock := newFakeClock()
	instant := governor.DefaultInstantConfig("policy-transition", "chaos", "instant", provider.SafeRouteSafety())
	instant.MinimumStartInterval = time.Nanosecond
	instantGovernor, err := governor.New(instant, governor.Options{Clock: clock, Jitter: fixedJitter{}, Persistence: store})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, workspace, "b.txt", "beta\n")
	phase1 := &chaosProvider{clock: clock, pace: time.Millisecond, sessionPrefix: "gen-1", steps: []chaosStep{
		{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`))},
		{response: responsePtr(actionResponse("read_file", `{"path":"b.txt"}`))},
		{err: typedError(omniroute.ErrorRateCapacity)},
	}}
	executor1, err := agent.NewExecutor(instantGovernor, phase1, omniroute.Classify)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	firstLoop, err := agent.NewLoop(agent.Config{
		Runner: executor1, Registry: registry, Limits: agent.Limits{MaxSteps: 10}, Clock: clock, State: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := firstLoop.Run(context.Background(), testTask("task-transition"))
	if first.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("phase 1 outcome = %q, want provider_failure (reason %q)", first.Outcome, first.StopReason)
	}
	if snapshot := instantGovernor.Snapshot(); snapshot.CooldownUntil.IsZero() {
		t.Fatalf("phase 1 must arm the cooldown: %#v", snapshot)
	}

	// Phase 2: the operator switches the account policy to unlimited_text.
	// The runtime rebuilds the governor from the persisted projection, exactly
	// like the CLI resume path does: the ledger, task attempts and cooldown
	// must carry over; the allowance semantic changes, the account does not.
	persisted, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = ok %t err %v", ok, err)
	}
	if persisted.AllowanceProfile != governor.ProfileInstant || persisted.AllowanceKind != governor.AllowanceKindPublishedQuota {
		t.Fatalf("phase 1 persisted allowance = profile %q kind %q", persisted.AllowanceProfile, persisted.AllowanceKind)
	}
	if len(persisted.RollingEvents) != 3 {
		t.Fatalf("phase 1 persisted ledger = %d events, want 3", len(persisted.RollingEvents))
	}

	unlimited := governor.DefaultLunaUnlimitedTextConfig("policy-transition", "chaos", "instant", provider.SafeRouteSafety())
	unlimited.MinimumStartInterval = time.Nanosecond
	transitioned, err := governor.New(unlimited, governor.Options{Clock: newFakeClock(), Jitter: fixedJitter{}, Persistence: store, Restore: &persisted})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := transitioned.Snapshot()
	if snapshot.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("transitioned kind = %q, want unlimited_text", snapshot.AllowanceKind)
	}
	// The durable protection survived the transition: the cooldown is still
	// armed and the ledger/task attempts are still counted.
	if snapshot.CooldownUntil.IsZero() {
		t.Fatal("transition reset the cooldown state")
	}
	if snapshot.Budgets.Rolling3hUsed != 3 || snapshot.Tasks["task-transition"].Attempts != 3 {
		t.Fatalf("transition reset the ledger/task attempts: %#v", snapshot)
	}

	// The transitioned governor executes a fresh attempt after the cooldown
	// expires, under the SAME account: the ledger continues and the new
	// allowance projection is persisted with the unlimited kind.
	phase2Clock := newFakeClock()
	phase2Governor, err := governor.New(unlimited, governor.Options{Clock: phase2Clock, Jitter: fixedJitter{}, Persistence: store, Restore: &persisted})
	if err != nil {
		t.Fatal(err)
	}
	phase2Clock.Advance(31 * time.Second) // past the persisted cooldown
	phase2 := &chaosProvider{clock: phase2Clock, pace: time.Millisecond, sessionPrefix: "gen-2", steps: []chaosStep{
		{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`))},
		{response: responsePtr(finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")))},
	}}
	executor2, err := agent.NewExecutor(phase2Governor, phase2, omniroute.Classify)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh registry mirrors a new process/session continuing the same task
	// under the transitioned policy.
	registry2, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	// The second run continues the SAME task's turn counters so the request
	// ids stay fresh for the governor dedup, exactly like a resumed run.
	secondLoop, err := agent.NewLoop(agent.Config{
		Runner: executor2, Registry: registry2, Limits: agent.Limits{MaxSteps: 10}, Clock: phase2Clock,
		Recovery: &agent.RecoverySeed{Turns: 3, Attempts: 3},
		Verifier: verifier.New(registry2, existsPlan("a.txt")),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := secondLoop.Run(context.Background(), testTask("task-transition"))
	if second.Outcome != agent.OutcomeCompleted {
		t.Fatalf("phase 2 outcome = %q, want completed (reason %q)", second.Outcome, second.StopReason)
	}
	snapshot2 := phase2Governor.Snapshot()
	if snapshot2.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("phase 2 kind = %q, want unlimited_text", snapshot2.AllowanceKind)
	}
	// 3 pre-transition debits + 2 phase-2 debits, all on the same account.
	if snapshot2.Budgets.Rolling3hUsed != 5 || snapshot2.Tasks["task-transition"].Attempts != 5 {
		t.Fatalf("transition created a fresh account ledger: %#v", snapshot2)
	}
	persisted2, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted2.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("persisted transitioned projection kind = %q, want unlimited_text", persisted2.AllowanceKind)
	}
	if len(persisted2.RollingEvents) != 5 {
		t.Fatalf("persisted ledger after transition = %d events, want 5 (no reset)", len(persisted2.RollingEvents))
	}
}
