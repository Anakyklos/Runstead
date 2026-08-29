package workunit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// mustWUTask creates a task usable by driver tests (real SQLite store).
func mustWUTask(t *testing.T, store *state.Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: "/workspace", Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24}`),
	}); err != nil {
		t.Fatalf("CreateTask(): %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask(): %v", err)
	}
}

func newDriver(t *testing.T, taskID string) (*Driver, *state.Store) {
	t.Helper()
	store, err := state.Open(state.Options{Path: t.TempDir() + "/workunit.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mustWUTask(t, store, taskID)
	return &Driver{
		Store: store, TaskID: taskID,
		AllowedTools:  []string{"read_file", "list_files", "run_recipe"},
		TaskWorkspace: "/workspace",
	}, store
}

// TestValidateEnvelopeEscalation proves capability containment fails before
// any effect (issue #106 test class 4).
func TestValidateEnvelopeEscalation(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	if err := driver.ValidateEnvelope([]string{"read_file"}, "/workspace/sub"); err != nil {
		t.Fatalf("contained envelope rejected: %v", err)
	}
	if err := driver.ValidateEnvelope([]string{"write_file"}, ""); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("tool escalation error = %v", err)
	}
	if err := driver.ValidateEnvelope([]string{"read_file"}, "/other"); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("workspace escalation error = %v", err)
	}
	if err := driver.ValidateEnvelope(nil, ""); err != nil {
		t.Fatalf("empty envelope (task default) rejected: %v", err)
	}
}

// TestDriverSerialDependencyOrder proves multiple units execute in dependency
// order, strictly one at a time, through a real store (issue #106 test
// classes 2/6).
func TestDriverSerialDependencyOrder(t *testing.T) {
	driver, store := newDriver(t, "wu-task")
	ctx := context.Background()
	created, skipped, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first"},
		{WorkUnitID: "wu-2", Objective: "second", Dependencies: []string{"wu-1"}},
		{WorkUnitID: "wu-3", Objective: "third", Dependencies: []string{"wu-2"}},
	})
	if err != nil || created != 3 || skipped != 0 {
		t.Fatalf("EnsureDefinitions() = %d/%d, %v", created, skipped, err)
	}
	// Idempotent re-supply.
	created, skipped, err = driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first"},
		{WorkUnitID: "wu-2", Objective: "second", Dependencies: []string{"wu-1"}},
	})
	if err != nil || created != 0 || skipped != 2 {
		t.Fatalf("re-supply = %d/%d, %v", created, skipped, err)
	}

	var order []string
	var mu sync.Mutex
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		// Strict serial proof: while this unit runs, exactly one unit may be
		// in 'running'.
		units, err := store.ListWorkUnits(ctx, unit.TaskID)
		if err != nil {
			return RunResult{}, err
		}
		running := 0
		for _, candidate := range units {
			if candidate.Status == "running" {
				running++
			}
		}
		if running != 1 {
			return RunResult{}, errors.New("violated strict serial execution")
		}
		// A verified completion requires a persisted passed verification.
		if err := store.SaveVerificationAttempt(ctx, state.VerificationAttemptRecord{
			TaskID: unit.TaskID, WorkUnitID: unit.WorkUnitID, Decision: "passed", Summary: "verified",
		}); err != nil {
			return RunResult{}, err
		}
		mu.Lock()
		order = append(order, unit.WorkUnitID)
		mu.Unlock()
		return RunResult{Outcome: "completed"}, nil
	}
	if err := driver.RunAll(ctx, run); err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	if strings.Join(order, ",") != "wu-1,wu-2,wu-3" {
		t.Fatalf("execution order = %v, want wu-1,wu-2,wu-3", order)
	}
	if err := driver.GateParent(ctx); err != nil {
		t.Fatalf("GateParent() after all completed: %v", err)
	}
}

// TestDriverVerificationRequiredForCompletion proves narrative without a
// passed verification decision can never complete a unit; the chain stops
// blocked and the parent gate stays open (issue #106 test class 9).
func TestDriverVerificationRequiredForCompletion(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first"},
	}); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		// Loop says "completed" but NO verification row exists: evidence-less.
		return RunResult{Outcome: "completed"}, nil
	}
	err := driver.RunAll(ctx, run)
	if !errors.Is(err, ErrWorkUnitBlockedChain) {
		t.Fatalf("RunAll() = %v, want blocked chain", err)
	}
	unit, _ := driver.Store.GetWorkUnit(ctx, "wu-task", "wu-1")
	if unit.Status != "blocked" || unit.BlockingReason == "" {
		t.Fatalf("unit = %s (%s), want blocked with reason", unit.Status, unit.BlockingReason)
	}
	if err := driver.GateParent(ctx); !errors.Is(err, ErrParentCompletionGated) {
		t.Fatalf("GateParent() = %v, want gated", err)
	}
}

// TestDriverFailureStopsChain proves a failed unit is terminal and stops the
// serial chain (issue #106 test class 8).
func TestDriverFailureStopsChain(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first"},
		{WorkUnitID: "wu-2", Objective: "second"},
	}); err != nil {
		t.Fatal(err)
	}
	mu := new(sync.Mutex)
	var order []string
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		mu.Lock()
		order = append(order, unit.WorkUnitID)
		mu.Unlock()
		if unit.WorkUnitID == "wu-1" {
			return RunResult{Outcome: "failed", Reason: "reproduced the failure"}, nil
		}
		return RunResult{Outcome: "unreachable"}, nil
	}
	err := driver.RunAll(ctx, run)
	if !errors.Is(err, ErrWorkUnitBlockedChain) {
		t.Fatalf("RunAll() = %v, want blocked chain", err)
	}
	if strings.Join(order, ",") != "wu-1" {
		t.Fatalf("chain did not stop after failure: %v", order)
	}
}

// TestDriverEscalationAtRunTime proves a unit whose envelope no longer fits
// the parent contract fails before running (defense against drift), and that
// creation of an escalated unit is already rejected.
func TestDriverEscalationAtRunTime(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	// Creation-time escalation fails.
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first", Tools: []string{"write_file"}},
	}); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("EnsureDefinitions() = %v, want capability escalation", err)
	}
	// Drift after creation: the parent contract narrows (a stricter driver)
	// while the persisted unit still carries an allowed-at-create envelope.
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "first", Tools: []string{"read_file"}},
	}); err != nil {
		t.Fatal(err)
	}
	narrowed := &Driver{
		Store: driver.Store, TaskID: driver.TaskID,
		AllowedTools: []string{"list_files"}, TaskWorkspace: "/workspace",
	}
	err := narrowed.RunAll(ctx, func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		t.Fatal("unit executed under a narrowed parent contract")
		return RunResult{}, nil
	})
	if !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("RunAll() = %v, want capability escalation", err)
	}
}
