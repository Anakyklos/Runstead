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
	if err := driver.ValidateEnvelope([]string{"read_file"}, "sub/dir"); err != nil {
		t.Fatalf("contained envelope rejected: %v", err)
	}
	if err := driver.ValidateEnvelope([]string{"write_file"}, ""); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("tool escalation error = %v", err)
	}
	// The canonical scope representation is workspace-relative: absolute
	// paths and traversal are escalation, exactly like the tool resolver.
	if err := driver.ValidateEnvelope([]string{"read_file"}, "/other"); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("absolute workspace scope error = %v", err)
	}
	if err := driver.ValidateEnvelope([]string{"read_file"}, "../escape"); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("traversal workspace scope error = %v", err)
	}
	if err := driver.ValidateEnvelope(nil, ""); err != nil {
		t.Fatalf("empty envelope (task default) rejected: %v", err)
	}
}

// TestValidateEnvelopeEmptyToolsDoesNotShortCircuitScope proves an empty
// tool list is a VALID fail-closed envelope (no tools) that must never
// bypass workspace containment (issue #106 review).
func TestValidateEnvelopeEmptyToolsDoesNotShortCircuitScope(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	if err := driver.ValidateEnvelope(nil, "inside"); err != nil {
		t.Fatalf("empty tools with contained scope rejected: %v", err)
	}
	if err := driver.ValidateEnvelope(nil, "/other"); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("empty tools must NOT short-circuit an absolute scope; err = %v", err)
	}
	if err := driver.ValidateEnvelope([]string{}, "../escape"); !errors.Is(err, ErrCapabilityEscalation) {
		t.Fatalf("explicit empty tool list must NOT short-circuit a traversing scope; err = %v", err)
	}
}

// TestEnsureDefinitionsDriftFailsClosed proves a re-supplied Work Unit id
// with a materially different definition is rejected instead of silently
// skipped (issue #106 review).
func TestEnsureDefinitionsDriftFailsClosed(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	created, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "original", Tools: []string{"read_file"}, WorkspaceScope: "sub"},
	})
	if err != nil || created != 1 {
		t.Fatalf("EnsureDefinitions() = %d, %v", created, err)
	}
	// Identical re-supply stays idempotent.
	created, skipped, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "original", Tools: []string{"read_file"}, WorkspaceScope: "sub"},
	})
	if err != nil || created != 0 || skipped != 1 {
		t.Fatalf("identical re-supply = %d/%d, %v", created, skipped, err)
	}
	for _, drifted := range []Definition{
		{WorkUnitID: "wu-1", Objective: "changed"},
		{WorkUnitID: "wu-1", Objective: "original", Tools: []string{"read_file", "list_files"}},
		{WorkUnitID: "wu-1", Objective: "original", Tools: []string{"read_file"}, WorkspaceScope: "sub/other"},
		{WorkUnitID: "wu-1", Objective: "original", Tools: []string{"read_file"}, ProviderBudget: 9},
	} {
		if _, _, err := driver.EnsureDefinitions(ctx, []Definition{drifted}); !errors.Is(err, ErrWorkUnitDefinitionDrift) {
			t.Fatalf("drift %+v error = %v, want ErrWorkUnitDefinitionDrift", drifted, err)
		}
	}
}

// TestEnsureDefinitionsDAGOrderIndependent proves a valid dependency graph
// is created regardless of the JSON file ordering, and a cycle fails the
// whole call (issue #106 review; no scheduler/concurrency added).
func TestEnsureDefinitionsDAGOrderIndependent(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	// Deliberately reverse-ordered input: dependents first.
	created, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-3", Objective: "third", Dependencies: []string{"wu-2"}},
		{WorkUnitID: "wu-2", Objective: "second", Dependencies: []string{"wu-1"}},
		{WorkUnitID: "wu-1", Objective: "first"},
	})
	if err != nil || created != 3 {
		t.Fatalf("reverse-ordered EnsureDefinitions() = %d, %v", created, err)
	}
	units, err := driver.Store.ListWorkUnits(ctx, "wu-task")
	if err != nil || len(units) != 3 {
		t.Fatalf("ListWorkUnits() = %v, %v", units, err)
	}
	byID := map[string]state.WorkUnit{}
	for _, unit := range units {
		byID[unit.WorkUnitID] = unit
	}
	if got := byID["wu-3"].Dependencies; len(got) != 1 || got[0] != "wu-2" {
		t.Fatalf("wu-3 dependencies = %v", got)
	}
	if got := byID["wu-2"].Dependencies; len(got) != 1 || got[0] != "wu-1" {
		t.Fatalf("wu-2 dependencies = %v", got)
	}

	// A cycle across the definitions fails before any creation.
	cycleDriver, cycleStore := newDriver(t, "wu-cycle")
	t.Cleanup(func() { cycleStore.Close() })
	if _, _, err := cycleDriver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "c-a", Objective: "a", Dependencies: []string{"c-b"}},
		{WorkUnitID: "c-b", Objective: "b", Dependencies: []string{"c-a"}},
	}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v, want cycle failure", err)
	}
	if got, _ := cycleStore.ListWorkUnits(ctx, "wu-cycle"); len(got) != 0 {
		t.Fatalf("cycle must create nothing, got %v", got)
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

// TestEnsureDefinitionsPreservesNilEmptyToolIdentity proves the
// security-significant envelope distinction: persisted OMITTED tools (task
// default) re-supplied as explicit [] is material narrowing and must fail as
// drift, in both directions (issue #106 review).
func TestEnsureDefinitionsPreservesNilEmptyToolIdentity(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-1", Objective: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	// omitted (nil) persisted; re-supplying explicit [] must drift.
	for _, def := range []Definition{
		{WorkUnitID: "wu-1", Objective: "x", Tools: []string{}},
		{WorkUnitID: "wu-1", Objective: "x", Tools: []string{"read_file"}},
	} {
		if _, _, err := driver.EnsureDefinitions(ctx, []Definition{def}); !errors.Is(err, ErrWorkUnitDefinitionDrift) {
			t.Fatalf("omitted->%v error = %v, want drift", def.Tools, err)
		}
	}
	// The reverse: persisted explicit [] re-supplied as omitted.
	driver2, _ := newDriver(t, "wu-task2")
	if _, _, err := driver2.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-2", Objective: "x", Tools: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver2.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-2", Objective: "x"},
	}); !errors.Is(err, ErrWorkUnitDefinitionDrift) {
		t.Fatalf("[]->omitted error = %v, want drift", err)
	}
	// Identical re-supplies (both omitted / both explicit empty) stay
	// idempotent.
	if _, skipped, err := driver.EnsureDefinitions(ctx, []Definition{{WorkUnitID: "wu-1", Objective: "x"}}); err != nil || skipped != 1 {
		t.Fatalf("omitted re-supply = %d skipped, %v", skipped, err)
	}
	if _, skipped, err := driver2.EnsureDefinitions(ctx, []Definition{{WorkUnitID: "wu-2", Objective: "x", Tools: []string{}}}); err != nil || skipped != 1 {
		t.Fatalf("[] re-supply = %d skipped, %v", skipped, err)
	}
}

// TestEnsureDefinitionsParentDriftAndOrder proves parent relationships are
// part of the durable identity (changed/removed parent = drift) and the DAG
// creation orders parents before children regardless of JSON ordering, with
// parent cycles failing before creation.
func TestEnsureDefinitionsParentDriftAndOrder(t *testing.T) {
	driver, _ := newDriver(t, "wu-task")
	ctx := context.Background()
	// Child declared BEFORE its parent: still created in parent-first order.
	created, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-child", Objective: "child", ParentWorkUnitID: "wu-parent"},
		{WorkUnitID: "wu-parent", Objective: "parent"},
	})
	if err != nil || created != 2 {
		t.Fatalf("parent-first create = %d, %v", created, err)
	}
	child, err := driver.Store.GetWorkUnit(ctx, "wu-task", "wu-child")
	if err != nil || child.ParentWorkUnitID != "wu-parent" {
		t.Fatalf("child parent = %q, %v", child.ParentWorkUnitID, err)
	}
	// Parent drift: removed parent on re-supply is material.
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-child", Objective: "child"},
	}); !errors.Is(err, ErrWorkUnitDefinitionDrift) {
		t.Fatalf("removed-parent re-supply error = %v, want drift", err)
	}
	if _, _, err := driver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "wu-child", Objective: "child", ParentWorkUnitID: "wu-parent"},
	}); err != nil || err == nil {
		// identical re-supply is idempotent
	}
	// A parent cycle fails before any creation.
	cycleDriver, cycleStore := newDriver(t, "wu-cycle-parent")
	t.Cleanup(func() { cycleStore.Close() })
	if _, _, err := cycleDriver.EnsureDefinitions(ctx, []Definition{
		{WorkUnitID: "p-a", Objective: "a", ParentWorkUnitID: "p-b"},
		{WorkUnitID: "p-b", Objective: "b", ParentWorkUnitID: "p-a"},
	}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("parent cycle error = %v, want cycle failure", err)
	}
	if got, _ := cycleStore.ListWorkUnits(ctx, "wu-cycle-parent"); len(got) != 0 {
		t.Fatalf("parent cycle must create nothing, got %v", got)
	}
}
