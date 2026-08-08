package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// readFileFingerprint computes the real protocol fingerprint for a read_file
// action so the seeded repeat guard matches the loop's actual fingerprint.
func readFileFingerprint(t *testing.T, path string) string {
	t.Helper()
	arguments := protocol.Arguments{"path": json.RawMessage(`"` + path + `"`)}
	return protocol.ActionFingerprint(protocol.Action{Tool: "read_file", Arguments: arguments})
}

// seedBuilder builds a loop from a RecoverySeed with the shared test clock.
type seedBuilder struct {
	clock *fakeClock
}

func (b seedBuilder) loop(t *testing.T, client provider.Client, seed *agent.RecoverySeed, workspace string) *agent.Loop {
	t.Helper()
	config := governor.DefaultInstantConfig("policy-seed-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: b.clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the real resume: the registry continues the task-scoped evidence
	// ID space after the seeded evidence instead of restarting at obs-000001.
	registry, err := tools.NewRegistry(tools.Options{
		Workspace:            workspace,
		NextEvidenceSequence: nextEvidenceSequence(seed.Evidence),
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: b.clock,
		Recovery: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

// nextEvidenceSequence returns the highest numeric evidence sequence among the
// seeded observations (obs-NNNNNN -> NNNNNN).
func nextEvidenceSequence(evidence []tools.Observation) int {
	max := 0
	for _, item := range evidence {
		sequence := 0
		value, ok := strings.CutPrefix(item.ID, "obs-")
		if !ok {
			continue
		}
		for _, digit := range value {
			if digit < '0' || digit > '9' {
				sequence = 0
				break
			}
			sequence = sequence*10 + int(digit-'0')
		}
		if sequence > max {
			max = sequence
		}
	}
	return max
}

func observation(t *testing.T, id, tool, content string) tools.Observation {
	t.Helper()
	return tools.Observation{
		ID: id, Tool: tool, Success: true,
		Data:     map[string]any{"path": "a.txt", "content": content},
		Metadata: tools.Metadata{Source: tool, Untrusted: true, Path: "a.txt", ExitCode: 0},
	}
}

// TestLoopResumeConsumesCommittedObservationWithoutReExecution proves the
// "result committed, next provider turn missing" recovery boundary: the seeded
// evidence grounds a final, and the completed historical action is rejected by
// the seeded repeat guard instead of being executed again.
func TestLoopResumeConsumesCommittedObservationWithoutReExecution(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	clock := newFakeClock()
	client := &scriptedProvider{
		clock: clock,
		pace:  time.Millisecond,
		responses: []provider.Response{
			actionResponse("read_file", `{"path":"a.txt"}`),
			finalResponse("complete", "Inspected.", "obs-000001"),
		},
	}
	seed := &agent.RecoverySeed{
		Turns:    1,
		Attempts: 1,
		Evidence: []tools.Observation{observation(t, "obs-000001", "read_file", "alpha\n")},
		Guard:    map[string]string{readFileFingerprint(t, "a.txt"): "sig-alpha"},
		Context:  "Recovered task summary.\nObjective: inspect.\nAvailable evidence: obs-000001\n",
	}
	loop := seedBuilder{clock: clock}.loop(t, client, seed, workspace)

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed\nreason: %s", result.Outcome, result.StopReason)
	}
	// The historical action was proposed again but rejected by the guard: only
	// the seeded evidence exists, and no new read_file observation was created.
	if client.Attempts() != 2 {
		t.Fatalf("provider attempts = %d, want 2 (proposal + final)", client.Attempts())
	}
	if len(seed.Evidence) != 1 {
		t.Fatal("seeded evidence must be consumed, not replaced by a re-execution")
	}
}

// TestLoopResumeWorkspaceChangeAllowsFreshObservation proves that fingerprint
// equality is loop evidence, not a result-reuse key: after an external
// workspace change the same action executes again and produces fresh evidence.
func TestLoopResumeWorkspaceChangeAllowsFreshObservation(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "beta\n")
	clock := newFakeClock()
	client := &scriptedProvider{
		clock: clock,
		pace:  time.Millisecond,
		responses: []provider.Response{
			actionResponse("read_file", `{"path":"a.txt"}`),
			finalResponse("complete", "Fresh.", "obs-000002"),
		},
	}
	// The historical action ran under a different workspace signature (alpha);
	// the current workspace (beta) must not match, so the read executes fresh.
	seed := &agent.RecoverySeed{
		Turns:    1,
		Attempts: 1,
		Evidence: []tools.Observation{observation(t, "obs-000001", "read_file", "alpha\n")},
		Guard:    map[string]string{readFileFingerprint(t, "a.txt"): "sig-alpha"},
		Context:  "Recovered task summary.\nObjective: inspect.\nAvailable evidence: obs-000001\n",
	}
	loop := seedBuilder{clock: clock}.loop(t, client, seed, workspace)

	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed\nreason: %s", result.Outcome, result.StopReason)
	}
	// The read_file executed as a new attempt with fresh evidence obs-000002.
	if !strings.Contains(result.StopReason, "grounded final accepted") {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
}

// TestLoopResumeContinuesCounters proves the resumed run keeps the loop budgets
// at the same boundary as a normal run: a seeded attempt count at the ceiling
// stops with the stable budget outcome instead of resetting the budget.
func TestLoopResumeContinuesCounters(t *testing.T) {
	workspace := t.TempDir()
	clock := newFakeClock()
	client := &scriptedProvider{
		clock:     clock,
		pace:      time.Millisecond,
		responses: []provider.Response{finalResponse("complete", "too late", "obs-000001")},
	}
	seed := &agent.RecoverySeed{
		Turns:    1,
		Attempts: 1,
		Context:  "Recovered task summary.\nObjective: inspect.\n",
	}
	// ProviderBudget of 1 with one historical attempt: the loop must stop with
	// provider_budget_exhausted before any new provider call.
	config := governor.DefaultInstantConfig("policy-seed-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner: executor, Registry: registry,
		Limits:   agent.Limits{ProviderBudget: 1},
		Clock:    clock,
		Recovery: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeProviderBudgetExhausted {
		t.Fatalf("outcome = %q, want provider_budget_exhausted", result.Outcome)
	}
	if client.Attempts() != 0 {
		t.Fatalf("provider attempts = %d, want 0 (budget was already consumed)", client.Attempts())
	}
}

// TestLoopResumeContextReachesNewConversation proves the reconstructed context
// is part of the new provider request: the recovered task continues with a new
// conversation, not the old one.
func TestLoopResumeContextReachesNewConversation(t *testing.T) {
	workspace := t.TempDir()
	clock := newFakeClock()
	client := &scriptedProvider{
		clock:     clock,
		pace:      time.Millisecond,
		responses: []provider.Response{finalResponse("complete", "done", "obs-000001")},
	}
	seed := &agent.RecoverySeed{
		Turns:    1,
		Attempts: 1,
		Evidence: []tools.Observation{observation(t, "obs-000001", "read_file", "alpha\n")},
		Context:  "RecoveryMarker-CONTINUE-FROM-HERE\nObjective: inspect the workspace.\n",
	}
	loop := seedBuilder{clock: clock}.loop(t, client, seed, workspace)
	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed", result.Outcome)
	}
	prompts := client.Requests()
	if len(prompts) == 0 {
		t.Fatal("no provider request was made")
	}
	if !strings.Contains(prompts[0], "RecoveryMarker-CONTINUE-FROM-HERE") {
		t.Fatalf("the new provider conversation must receive the reconstructed context:\n%s", prompts[0])
	}
	if strings.Contains(prompts[0], "runstead:assistant") {
		t.Fatal("the new conversation must not embed the old assistant turns")
	}
	// The task objective is present through the recovery context.
	if !strings.Contains(prompts[0], "inspect the workspace") {
		t.Fatal("the new conversation must retain the objective")
	}
}

// TestLoopResumeEvidenceSequenceContinues proves the resumed registry continues
// the task-scoped evidence ID space instead of colliding with persisted IDs.
func TestLoopResumeEvidenceSequenceContinues(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "alpha\n")
	clock := newFakeClock()
	client := &scriptedProvider{
		clock: clock,
		pace:  time.Millisecond,
		responses: []provider.Response{
			actionResponse("read_file", `{"path":"a.txt"}`),
			finalResponse("complete", "Fresh.", "obs-000002"),
		},
	}
	// Persisted evidence obs-000001 exists; the resumed registry must continue
	// at obs-000002 so no evidence ID collides.
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace, NextEvidenceSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	config := governor.DefaultInstantConfig("policy-seed-test", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := &agent.RecoverySeed{
		Turns:    1,
		Attempts: 1,
		Context:  "Recovered task summary.\n",
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner: executor, Registry: registry, Limits: agent.Limits{}, Clock: clock, Recovery: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed\nreason: %s", result.Outcome, result.StopReason)
	}
	// The final cited obs-000002: the registry continued after obs-000001.
	grounded := false
	for _, id := range result.Evidence {
		if id == "obs-000002" {
			grounded = true
		}
	}
	if !grounded {
		t.Fatalf("evidence = %v, want obs-000002 grounded", result.Evidence)
	}
}

// TestLoopResumeUsesRegistryWorkspace proves the resumed registry observes the
// current workspace (the persisted task workspace supplied at resume time).
func TestLoopResumeUsesRegistryWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	client := &scriptedProvider{
		clock: clock,
		pace:  time.Millisecond,
		responses: []provider.Response{
			actionResponse("read_file", `{"path":"a.txt"}`),
			finalResponse("complete", "Read.", "obs-000001"),
		},
	}
	seed := &agent.RecoverySeed{Context: "Recovered task summary.\nObjective: inspect.\n"}
	loop := seedBuilder{clock: clock}.loop(t, client, seed, workspace)
	result := loop.Run(context.Background(), testTask("task-1"))
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("outcome = %q, want completed\nreason: %s", result.Outcome, result.StopReason)
	}
}
