package agent_test

// Issue #13 - provider failure chaos at runtime level. Every provider failure
// is injected through a deterministic fake client whose errors are classified
// by the REAL omniroute classifier, so the typed outcome vocabulary (timeout,
// connection_reset, captcha, ...) is exercised end to end: governor admission
// -> classified finish -> durable provider attempt -> typed loop outcome. No
// live network, no OmniRoute, no retries hidden below the governor.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
)

// chaosStep is one scripted provider call: an optional response and an
// optional typed adapter error. Exactly one must be set.
type chaosStep struct {
	response *provider.Response
	kind     omniroute.ErrorKind
	err      error
	// sessionID optionally stamps the response metadata (stale/session tests).
	sessionID string
}

// chaosProvider replays scripted steps, records every attempt, and can
// additionally simulate a replaced provider conversation between runs (the
// same task continues against a NEW provider object with new session
// metadata, mirroring "start a new upstream conversation").
type chaosProvider struct {
	clock   *fakeClock
	pace    time.Duration
	steps   []chaosStep
	attempt int
	seen    []string
	// sessions records every response session id the provider stamped, so
	// tests can assert the session metadata actually reached the client.
	sessions []string
	// sessionPrefix is stamped into every response metadata so the test can
	// distinguish one provider generation from another.
	sessionPrefix string
}

func (p *chaosProvider) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (p *chaosProvider) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	p.attempt++
	p.seen = append(p.seen, request.Prompt)
	if p.clock != nil && p.pace > 0 {
		// Simulate provider I/O time so the governor's pacing timers fire
		// deterministically between turns, exactly like scriptedProvider.
		p.clock.Advance(p.pace)
	}
	if len(p.steps) == 0 {
		return provider.Response{}, provider.ErrNoPredefinedResponse
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	if step.err != nil {
		return provider.Response{}, step.err
	}
	if step.response == nil {
		return provider.Response{}, provider.ErrNoPredefinedResponse
	}
	response := *step.response
	switch {
	case step.sessionID != "":
		response.Metadata.SessionID = step.sessionID
	case p.sessionPrefix != "" && response.Metadata.SessionID == "":
		response.Metadata.SessionID = p.sessionPrefix + "-session"
	}
	if response.Metadata.SessionID != "" {
		p.sessions = append(p.sessions, response.Metadata.SessionID)
	}
	return response, nil
}

func (p *chaosProvider) Attempts() int { return p.attempt }

func (p *chaosProvider) prompts() []string { return append([]string(nil), p.seen...) }

func (p *chaosProvider) sessionIDs() []string { return append([]string(nil), p.sessions...) }

func typedError(kind omniroute.ErrorKind) error {
	return &omniroute.Error{Kind: kind, UpstreamReached: true}
}

// chaosHarness wires a governor, the chaos provider, the omniroute classifier
// and the real store into one loop composition, mirroring the CLI wiring for
// the live path (classifier + executor) without any network.
type chaosHarness struct {
	clock    *fakeClock
	governor *governor.Governor
	provider *chaosProvider
	executor *agent.Executor
	registry *tools.Registry
	store    *state.Store
	policy   policy.Policy
	traces   *traceCapture
}

func newChaosHarness(t *testing.T, workspace string, configure func(*governor.Config), steps ...chaosStep) *chaosHarness {
	t.Helper()
	return newChaosHarnessUnlimited(t, workspace, false, configure, steps...)
}

func newChaosHarnessUnlimited(t *testing.T, workspace string, unlimited bool, configure func(*governor.Config), steps ...chaosStep) *chaosHarness {
	t.Helper()
	return newChaosHarnessPersistence(t, workspace, unlimited, configure, nil, steps...)
}

// newChaosHarnessPersistence is the general harness constructor. A non-nil
// persist overrides the governor's persistence boundary (the fault wrapper),
// so tests can inject persistence failures into the provider TX 1/TX 2 paths.
func newChaosHarnessPersistence(t *testing.T, workspace string, unlimited bool, configure func(*governor.Config), persist governor.Persistence, steps ...chaosStep) *chaosHarness {
	t.Helper()
	return newChaosHarnessWithStore(t, workspace, unlimited, configure, nil, persist, steps...)
}

// newChaosHarnessWithStore is the full harness constructor: the caller may
// provide the shared store (so a fault wrapper and the loop task state live
// in the SAME database) and the governor persistence boundary.
func newChaosHarnessWithStore(t *testing.T, workspace string, unlimited bool, configure func(*governor.Config), shared *state.Store, persist governor.Persistence, steps ...chaosStep) *chaosHarness {
	t.Helper()
	clock := newFakeClock()
	var config governor.Config
	if unlimited {
		config = governor.DefaultLunaUnlimitedTextConfig("policy-chaos", "chaos", "instant", provider.SafeRouteSafety())
	} else {
		config = governor.DefaultInstantConfig("policy-chaos", "chaos", "instant", provider.SafeRouteSafety())
	}
	config.MinimumStartInterval = time.Nanosecond
	if configure != nil {
		configure(&config)
	}
	var store *state.Store
	var err error
	if shared != nil {
		store = shared
	} else {
		store, err = state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
		if err != nil {
			t.Fatalf("state.Open() error = %v", err)
		}
		t.Cleanup(func() { store.Close() })
	}
	if persist == nil {
		persist = store
	}
	accountGovernor, err := governor.New(config, governor.Options{
		Clock:       clock,
		Jitter:      fixedJitter{},
		Persistence: persist,
	})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	client := &chaosProvider{clock: clock, pace: time.Millisecond, steps: append([]chaosStep(nil), steps...), sessionPrefix: "gen-1"}
	executor, err := agent.NewExecutor(accountGovernor, client, omniroute.Classify)
	if err != nil {
		t.Fatalf("agent.NewExecutor() error = %v", err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("tools.NewRegistry() error = %v", err)
	}
	return &chaosHarness{
		clock:    clock,
		governor: accountGovernor,
		provider: client,
		executor: executor,
		registry: registry,
		store:    store,
		policy:   policy.NewStatic(allowAllPolicy(), storeApprovals(store)),
		traces:   &traceCapture{},
	}
}

func (h *chaosHarness) loop(t *testing.T, limits agent.Limits) *agent.Loop {
	t.Helper()
	return h.loopPlan(t, limits, nil)
}

func (h *chaosHarness) loopPlan(t *testing.T, limits agent.Limits, plan *verifier.Plan) *agent.Loop {
	t.Helper()
	loop, err := agent.NewLoop(agent.Config{
		Runner:   h.executor,
		Registry: h.registry,
		Limits:   limits,
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    h.store,
		Policy:   h.policy,
		Verifier: verifier.New(h.registry, plan),
	})
	if err != nil {
		t.Fatalf("agent.NewLoop() error = %v", err)
	}
	return loop
}

// TestProviderChaosTimeoutClassifiedBounded proves a provider timeout is a
// classified, terminal, single-attempt failure: exactly one governed attempt,
// exactly one ledger debit, no hidden retry, and the durable attempt row
// records the typed classification.
func TestProviderChaosTimeoutClassifiedBounded(t *testing.T) {
	workspace := t.TempDir()
	h := newChaosHarness(t, workspace, nil,
		chaosStep{err: typedError(omniroute.ErrorTimeout)},
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-timeout"))
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure (reason %q)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "timeout") {
		t.Fatalf("stop reason must carry the typed classification: %q", result.StopReason)
	}
	if result.Classification != "timeout" {
		t.Fatalf("classification = %q, want timeout", result.Classification)
	}
	// No hidden retry: the provider was called exactly once.
	if h.provider.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1", h.provider.Attempts())
	}
	// The governor ledger debited exactly one attempt.
	snapshot := h.governor.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 1 || snapshot.Tasks["task-timeout"].Attempts != 1 {
		t.Fatalf("governor accounting = %#v, want one debit", snapshot)
	}
	// The durable provider attempt row carries the classified outcome.
	recovery, err := h.store.LoadRecoverySnapshot(context.Background(), "task-timeout")
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	if len(recovery.ProviderAttempts) != 1 {
		t.Fatalf("persisted provider attempts = %d, want 1", len(recovery.ProviderAttempts))
	}
	attempt := recovery.ProviderAttempts[0]
	if attempt.Status != "failed" || attempt.Outcome != string(governor.OutcomeTimeout) {
		t.Fatalf("durable attempt = %+v, want failed with outcome timeout", attempt)
	}
	if attempt.AttemptDebited != 1 {
		t.Fatalf("durable attempt debit = %d, want 1", attempt.AttemptDebited)
	}
	if !attempt.UpstreamReached {
		t.Fatalf("durable attempt must record upstream-reached: %+v", attempt)
	}
}

// TestProviderChaosConnectionResetClassified proves the same bounded,
// classified single-attempt contract for a connection reset.
func TestProviderChaosConnectionResetClassified(t *testing.T) {
	workspace := t.TempDir()
	h := newChaosHarness(t, workspace, nil,
		chaosStep{err: typedError(omniroute.ErrorConnectionReset)},
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-reset"))
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure (reason %q)", result.Outcome, result.StopReason)
	}
	if result.Classification != "connection_reset" {
		t.Fatalf("classification = %q, want connection_reset", result.Classification)
	}
	if h.provider.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1 (no hidden retry)", h.provider.Attempts())
	}
}

// TestProviderChaosNoRetryBypassesGovernor proves that even a retryable
// classification (timeout) never re-issues the same request outside the
// governor: the loop turns the classified failure into a terminal typed
// outcome and the account lane is fully released afterwards.
func TestProviderChaosNoRetryBypassesGovernor(t *testing.T) {
	workspace := t.TempDir()
	h := newChaosHarness(t, workspace, nil,
		chaosStep{err: typedError(omniroute.ErrorTimeout)},
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-retry"))
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("outcome = %q, want provider_failure", result.Outcome)
	}
	if h.provider.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1", h.provider.Attempts())
	}
	snapshot := h.governor.Snapshot()
	if snapshot.InFlight || snapshot.QueueLength != 0 {
		t.Fatalf("account lane must be fully released after the failure: %#v", snapshot)
	}
}

// TestProviderChaosDuplicatedResponseNoDuplicateEffects proves a duplicated
// provider response does not silently duplicate a completed side effect: the
// identical write proposal is re-executed as a NEW attempt and fails closed on
// the stale-state precondition, so the file is written exactly once.
func TestProviderChaosDuplicatedResponseNoDuplicateEffects(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	before := toolsHash("alpha\n")
	duplicate := `<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"a.txt","content":"bravo\n","expected_before_hash":"` + before + `"}}</runstead_action>`

	h := newChaosHarness(t, workspace, nil,
		chaosStep{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`))},
		chaosStep{response: responsePtr(providerResponseText(duplicate))},
		// The provider repeats the SAME response: the model re-proposes the
		// same write against the already-changed workspace.
		chaosStep{response: responsePtr(providerResponseText(duplicate))},
		chaosStep{response: responsePtr(finalResponse("complete", "done", finalEvidence("obs-000001", "read_file"), finalEvidence("obs-000002", "write_file")))},
	)
	loop := h.loopPlan(t, agent.Limits{MaxCorrections: 3, MaxRepeatedActions: 3}, existsPlan("a.txt"))
	result := loop.Run(context.Background(), testTask("task-dup-response"))
	// The stale second write fails deterministically; the final cites the
	// first write's evidence and completes.
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed (reason %q)", result.Outcome, result.StopReason)
	}
	content, err := readFileContent(workspace, "a.txt")
	if err != nil || content != "bravo\n" {
		t.Fatalf("file content = %q, want bravo\\n exactly once (err %v)", content, err)
	}
	recovery, err := h.store.LoadRecoverySnapshot(context.Background(), "task-dup-response")
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one completed write attempt; the duplicated proposal produced a
	// second attempt that failed with the stale-state classification.
	var completed, failed int
	for _, attempt := range recovery.ToolAttempts {
		if attempt.Tool != "write_file" {
			continue
		}
		if attempt.Status == "completed" {
			completed++
		}
		if attempt.Status == "failed" {
			failed++
			if attempt.Classification == "" {
				t.Fatalf("failed duplicate write must carry a typed classification: %+v", attempt)
			}
		}
	}
	if completed != 1 || failed != 1 {
		t.Fatalf("write attempts = completed %d failed %d, want 1/1 (no silent duplication)", completed, failed)
	}
}

// TestProviderChaosStaleSessionMetadataDisposable proves old remote session
// identifiers are disposable metadata: after an interruption, the task
// continues against a NEW provider conversation whose metadata carries a new
// session id, and the authoritative local task state (objective, evidence,
// attempts) is preserved.
func TestProviderChaosStaleSessionMetadataDisposable(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	ctx := context.Background()

	// Run 1: one committed read observation, then an interruption-like stop
	// (the provider's session dies: a classified connection reset).
	h := newChaosHarness(t, workspace, nil,
		chaosStep{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`)), sessionID: "stale-session-1"},
		chaosStep{err: typedError(omniroute.ErrorConnectionReset), sessionID: "stale-session-1"},
	)
	store := h.store
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(ctx, testTask("task-session"))
	if result.Outcome != agent.OutcomeProviderFailure {
		t.Fatalf("run 1 outcome = %q, want provider_failure", result.Outcome)
	}

	// Run 2: a brand-new provider conversation (new session metadata) resumes
	// the same task from the reconstructed durable state.
	newProvider := &chaosProvider{clock: h.clock, pace: time.Millisecond, sessionPrefix: "gen-2", steps: []chaosStep{
		{response: responsePtr(finalResponse("complete", "done", finalEvidence("obs-000001", "read_file")))},
	}}
	executor2, err := agent.NewExecutor(h.governor, newProvider, omniroute.Classify)
	if err != nil {
		t.Fatal(err)
	}
	seed := &agent.RecoverySeed{
		Turns:    2,
		Attempts: 2,
		Evidence: []tools.Observation{{
			ID:      "obs-000001",
			Tool:    "read_file",
			Success: true,
			Data:    map[string]any{"path": "a.txt", "content": "alpha\n"},
			Metadata: tools.Metadata{
				Source: "read_file", Untrusted: true, Path: "a.txt", ExitCode: 0,
			},
		}},
		Context: "RecoveryMarker-CONTINUE\nObjective: inspect the workspace.\n",
	}
	loop2, err := agent.NewLoop(agent.Config{
		Runner:   executor2,
		Registry: h.registry,
		Limits:   agent.Limits{MaxSteps: 10},
		Clock:    h.clock,
		Trace:    h.traces.emit,
		State:    store,
		Recovery: seed,
		Verifier: verifier.New(h.registry, existsPlan("a.txt")),
	})
	if err != nil {
		t.Fatal(err)
	}
	result2 := loop2.Run(ctx, testTask("task-session"))
	if result2.Outcome != agent.OutcomeCompleted {
		t.Fatalf("resumed outcome = %q, want completed (reason %q)", result2.Outcome, result2.StopReason)
	}
	// The new conversation saw the reconstructed context, not the old one.
	if len(newProvider.prompts()) == 0 || !strings.Contains(newProvider.prompts()[0], "RecoveryMarker-CONTINUE") {
		t.Fatal("the new conversation must receive the reconstructed recovery context")
	}
	// The session metadata was actually observed: run 1 stamped the stale
	// session id on every response, and the new provider generation stamps
	// its own id, so the session claim is grounded in the fake's evidence.
	sessions := h.provider.sessionIDs()
	// Only the response step carries metadata (the connection-reset step
	// returns an error with no response); the one response that was produced
	// must carry the stale session id.
	if len(sessions) != 1 || sessions[0] != "stale-session-1" {
		t.Fatalf("run 1 session metadata = %v, want stale-session-1 on the response", sessions)
	}
	if newSessions := newProvider.sessionIDs(); len(newSessions) != 1 || newSessions[0] != "gen-2-session" {
		t.Fatalf("run 2 session metadata = %v, want gen-2-session", newSessions)
	}
	// The old session id is gone from the new conversation's metadata and the
	// task truth (evidence) came from local durable state, not the old session.
	recovery, err := store.LoadRecoverySnapshot(ctx, "task-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.ProviderAttempts) != 3 {
		t.Fatalf("persisted provider attempts = %d, want 3 (two failed in run 1, one resumed)", len(recovery.ProviderAttempts))
	}
	if recovery.ProviderAttempts[0].Outcome != string(governor.OutcomeSuccess) ||
		recovery.ProviderAttempts[1].Outcome != string(governor.OutcomeConnectionReset) {
		t.Fatalf("run 1 provider attempts = %+v, want success then connection_reset", recovery.ProviderAttempts[:2])
	}
	if recovery.ProviderAttempts[2].Outcome != string(governor.OutcomeSuccess) {
		t.Fatalf("resumed provider attempt = %+v, want success under the new conversation", recovery.ProviderAttempts[2])
	}
	// Exactly one read observation exists and it was committed before the
	// interruption: the resumed run did not re-execute it.
	if len(recovery.Evidence) != 1 || recovery.Evidence[0].EvidenceID != "obs-000001" {
		t.Fatalf("persisted evidence = %+v, want exactly obs-000001", recovery.Evidence)
	}
	// The first run's attempts must be durably recorded with their
	// classifications, and the resumed run must have executed under the SAME
	// governor ledger (two failed + one resumed logical attempt).
	snapshot := h.governor.Snapshot()
	if snapshot.Tasks["task-session"].Attempts != 3 {
		t.Fatalf("governor task attempts = %d, want 3 (2 failed + 1 resumed)", snapshot.Tasks["task-session"].Attempts)
	}
}

func responsePtr(response provider.Response) *provider.Response {
	copied := response
	return &copied
}

func providerResponseText(text string) provider.Response {
	return provider.Response{Text: text}
}

func toolsHash(content string) string {
	return tools.HashBytes([]byte(content))
}

// TestProviderChaosTX2PersistenceFailurePausesForRecovery proves the
// provider-side TX 2 contract: the upstream call returned but the classified
// outcome could not be persisted, so the provider attempt stays 'prepared'
// (the upstream may have been reached). The task pauses durably resumable
// instead of finalizing terminally, and recovery reconciles the attempt
// conservatively (debit preserved, never re-issued) (issue #13 review).
func TestProviderChaosTX2PersistenceFailurePausesForRecovery(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")

	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	faulty := &faultyPersistence{Persistence: store}
	faulty.failNext("RecordProviderFinished", 1)
	// The governor persists through the fault wrapper and the LOOP task state
	// lives in the SAME store, so the TX 2 failure is a real injected
	// persistence error at the governor boundary with the task row present.
	h := newChaosHarnessWithStore(t, workspace, false, nil, store, faulty,
		chaosStep{response: responsePtr(actionResponse("read_file", `{"path":"a.txt"}`))},
	)
	loop := h.loop(t, agent.Limits{})
	result := loop.Run(context.Background(), testTask("task-provider-tx2"))
	if result.Outcome != agent.OutcomePersistencePaused {
		t.Fatalf("outcome = %q, want persistence_paused (reason %q)", result.Outcome, result.StopReason)
	}
	if !strings.Contains(result.StopReason, "durable provider outcome could not be persisted") {
		t.Fatalf("stop reason must identify the provider persistence failure: %q", result.StopReason)
	}
	// The provider call happened exactly once (no retry) and the durable
	// provider attempt stays prepared: the upstream may have been reached.
	if h.provider.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want exactly 1", h.provider.Attempts())
	}
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-provider-tx2")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.Status != "running" {
		t.Fatalf("task status = %q, want running (resumable, not finalized)", snapshot.Task.Status)
	}
	if len(snapshot.ProviderAttempts) != 1 || snapshot.ProviderAttempts[0].Status != "prepared" {
		t.Fatalf("provider attempt must stay prepared: %+v", snapshot.ProviderAttempts)
	}
	// The pause fired before the proposed read action could execute: no tool
	// attempt and no evidence exist, and none was invented.
	if len(snapshot.ToolAttempts) != 0 || len(snapshot.Evidence) != 0 {
		t.Fatalf("no tool effect may exist after the provider TX 2 pause: %+v / %+v", snapshot.ToolAttempts, snapshot.Evidence)
	}

	// Recovery reconciles the prepared provider attempt conservatively: the
	// debit stands and the request is never re-issued.
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: "task-provider-tx2"})
	if err != nil {
		t.Fatalf("recovery.Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("recovery decision = %s, want continue (reason %s)", plan.Decision, plan.Reason)
	}
	if plan.UncertainProviderAttempts != 1 || plan.ReconciledProviderAttempts != 1 {
		t.Fatalf("reconciled provider attempts = %d (uncertain %d), want 1/1", plan.ReconciledProviderAttempts, plan.UncertainProviderAttempts)
	}
	reconciled, err := store.LoadRecoverySnapshot(context.Background(), "task-provider-tx2")
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.ProviderAttempts[0].Status != "reconciled" || !reconciled.ProviderAttempts[0].Uncertain {
		t.Fatalf("reconciled provider attempt = %+v, want reconciled+uncertain", reconciled.ProviderAttempts[0])
	}
}
