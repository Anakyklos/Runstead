package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// wuTaskID is the task id used by work unit store tests.
const wuTaskID = "wu-task-1"

// TestFindWorkUnitCycle is the white-box deterministic cycle detector check.
func TestFindWorkUnitCycle(t *testing.T) {
	if cycle := findWorkUnitCycle(map[string][]string{"a": {"b"}, "b": {"a"}}); cycle == "" {
		t.Fatal("a<->b cycle not detected")
	}
	if cycle := findWorkUnitCycle(map[string][]string{"a": {"a"}}); cycle == "" {
		t.Fatal("self loop not detected")
	}
	if cycle := findWorkUnitCycle(map[string][]string{"a": {"b"}, "b": {"c"}}); cycle != "" {
		t.Fatalf("acyclic graph reported cycle %q", cycle)
	}
	// Deterministic regardless of map insertion order.
	if cycle := findWorkUnitCycle(map[string][]string{"b": {"a"}, "a": {"b"}}); cycle == "" {
		t.Fatal("cycle detection depends on map order")
	}
}

// mustWorkUnitTask creates a task usable by work unit tests.
func mustWorkUnitTask(t *testing.T, store *Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, TaskRecord{
		TaskID: taskID, Objective: "worker", Workspace: "/ws", Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24}`),
	}); err != nil {
		t.Fatalf("CreateTask(): %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask(): %v", err)
	}
}

const samplePlan = `{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}`
const sampleDigest = "digest-workunit-1"

func mustWorkUnit(t *testing.T, store *Store, create WorkUnitCreate) *WorkUnit {
	t.Helper()
	id, err := store.CreateWorkUnit(context.Background(), create)
	if err != nil {
		t.Fatalf("CreateWorkUnit(%s): %v", create.WorkUnitID, err)
	}
	unit, err := store.GetWorkUnit(context.Background(), create.TaskID, id)
	if err != nil {
		t.Fatalf("GetWorkUnit(): %v", err)
	}
	return unit
}

// TestWorkUnitsStoreRoundTrip proves create/get/list and the deterministic
// persisted fields (issue #106 test class 1).
func TestWorkUnitsStoreRoundTrip(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	unit := mustWorkUnit(t, store, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-1",
		Objective:      "read the workspace",
		Tools:          []string{"read_file", "list_files"},
		AcceptancePlan: []byte(samplePlan), AcceptanceDigest: sampleDigest,
		ContextBudget: 8192, ProviderBudget: 10, StepBudget: 5,
	})
	if unit.Status != "created" || unit.Version != 1 {
		t.Fatalf("unit = %+v, want created v1", unit)
	}
	if len(unit.Tools) != 2 || unit.AcceptanceDigest != sampleDigest || unit.ContextBudget != 8192 {
		t.Fatalf("unit fields = %+v", unit)
	}
	units, err := store.ListWorkUnits(context.Background(), wuTaskID)
	if err != nil || len(units) != 1 {
		t.Fatalf("ListWorkUnits() = %v, %v", units, err)
	}
	// Deterministic creation order.
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-2", Objective: "second"})
	units, _ = store.ListWorkUnits(context.Background(), wuTaskID)
	if units[0].WorkUnitID != "wu-1" || units[1].WorkUnitID != "wu-2" {
		t.Fatalf("creation order = %s,%s", units[0].WorkUnitID, units[1].WorkUnitID)
	}
	// Unknown unit fails closed.
	if _, err := store.GetWorkUnit(context.Background(), wuTaskID, "nope"); !errors.Is(err, ErrWorkUnitNotFound) {
		t.Fatalf("GetWorkUnit(nope) = %v", err)
	}
}

// TestWorkUnitsLifecycleMatrix proves the deterministic transition map and
// the rejection of invalid edges (issue #106 test class 2).
func TestWorkUnitsLifecycleMatrix(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-1", Objective: "x"})
	ctx := context.Background()

	valid := [][2]string{{"created", "ready"}, {"ready", "running"}, {"running", "failed"},
		{"running", "blocked"}, {"running", "uncertain"}, {"running", "ready"}, {"blocked", "ready"}}
	ids := []string{"wu-v1", "wu-v2", "wu-v3", "wu-v4", "wu-v5", "wu-v6", "wu-v7"}
	for index, edge := range valid {
		id := ids[index]
		mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: id, Objective: "y"})
		bringTo(t, store, id, edge[0])
		if err := store.TransitionWorkUnit(ctx, wuTaskID, id, edge[0], edge[1], "test"); err != nil {
			t.Fatalf("valid transition %v: %v", edge, err)
		}
	}
	// completed is the terminal success edge.
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-c", Objective: "c"})
	bringTo(t, store, "wu-c", "running")
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-c", "running", "completed", ""); err != nil {
		t.Fatalf("running->completed: %v", err)
	}
	// Invalid edges rejected.
	invalid := [][2]string{{"created", "running"}, {"completed", "ready"}, {"failed", "ready"},
		{"blocked", "running"}, {"uncertain", "completed"}, {"ready", "completed"}, {"created", "completed"}}
	for _, edge := range invalid {
		if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-c", edge[0], edge[1], "x"); err == nil {
			t.Fatalf("invalid transition %v accepted", edge)
		}
	}
	// Stale-state transition (from mismatch) fails closed.
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-c", "created", "ready", "x"); err == nil {
		t.Fatal("stale from-status transition accepted")
	}
}

// bringTo walks a unit through the deterministic path up to the requested
// status (created is the birth state).
func bringTo(t *testing.T, store *Store, id, target string) {
	t.Helper()
	ctx := context.Background()
	unit, err := store.GetWorkUnit(ctx, wuTaskID, id)
	if err != nil {
		t.Fatal(err)
	}
	if unit.Status == target {
		return
	}
	path, ok := map[string][]string{
		"created->ready":     {"ready"},
		"created->running":   {"ready", "running"},
		"created->blocked":   {"ready", "running", "blocked"},
		"created->uncertain": {"ready", "running", "uncertain"},
		"ready->running":     {"running"},
		"running->blocked":   {"blocked"},
	}[unit.Status+"->"+target]
	if !ok {
		t.Fatalf("bringTo: unsupported path %s->%s", unit.Status, target)
	}
	current := unit.Status
	for _, step := range path {
		if err := store.TransitionWorkUnit(ctx, wuTaskID, id, current, step, "bring-to"); err != nil {
			t.Fatalf("bringTo %s step %s: %v", id, step, err)
		}
		current = step
	}
}

// TestWorkUnitsMissingDependencyAndCycle proves fail-closed graph validation
// (issue #106 test classes 3/5): missing/foreign deps and cycles.
func TestWorkUnitsMissingDependencyAndCycle(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()

	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-a", Objective: "a",
		Dependencies: []string{"missing"},
	}); !errors.Is(err, ErrWorkUnitMissingDependency) {
		t.Fatalf("missing dependency error = %v", err)
	}
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-a", Objective: "a"})
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-b", Objective: "b",
		Dependencies: []string{"wu-a"}})
	// Adding the reverse edge creates a cycle and must fail.
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-c", Objective: "c",
		Dependencies: []string{"wu-b"},
	}); err != nil {
		t.Fatalf("wu-c create: %v", err)
	}
	// A self-dependency references a not-yet-existing unit: the missing
	// dependency check is the honest fail-closed result.
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-d", Objective: "d",
		Dependencies: []string{"wu-d"},
	}); !errors.Is(err, ErrWorkUnitMissingDependency) {
		t.Fatalf("self-dependency error = %v", err)
	}
	// Corrupt graph (reverse edge inserted directly, simulating drift): any
	// subsequent create must fail with ErrWorkUnitCycle.
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO work_unit_dependencies (work_unit_id, depends_on_work_unit_id) VALUES ('wu-a', 'wu-b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-e", Objective: "e",
	}); !errors.Is(err, ErrWorkUnitCycle) {
		t.Fatalf("corrupt graph error = %v", err)
	}
}

// TestWorkUnitsReadyOrderingAndParentGates proves ready selection from
// completed dependencies (never from claims) plus the parent completion gate
// (issue #106 test classes 6/10).
func TestWorkUnitsReadyOrderingAndParentGates(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-1", Objective: "first"})
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-2", Objective: "second",
		Dependencies: []string{"wu-1"}})
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-3", Objective: "third",
		Dependencies: []string{"wu-2"}})

	ready, err := store.ReadyWorkUnits(ctx, wuTaskID)
	if err != nil || len(ready) != 1 || ready[0].WorkUnitID != "wu-1" {
		t.Fatalf("ready = %+v, %v; want exactly wu-1", ready, err)
	}
	open, err := store.HasOpenWorkUnits(ctx, wuTaskID)
	if err != nil || !open {
		t.Fatalf("HasOpenWorkUnits() = %v, %v; want true", open, err)
	}
	// Complete wu-1: only wu-2 becomes ready.
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-1", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-1", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-1", "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	ready, _ = store.ReadyWorkUnits(ctx, wuTaskID)
	if len(ready) != 1 || ready[0].WorkUnitID != "wu-2" {
		t.Fatalf("after wu-1 completion ready = %+v; want exactly wu-2", ready)
	}
	// Complete wu-2; wu-3 becomes ready; then all completed -> gate closed.
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-2", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-2", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-2", "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-3", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-3", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-3", "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	open, _ = store.HasOpenWorkUnits(ctx, wuTaskID)
	if open {
		t.Fatal("HasOpenWorkUnits() = true after all units completed")
	}
}

// TestWorkUnitsEvidenceRefsDerivedFromTaggedRows proves completion snapshots
// durable evidence references from rows tagged with the unit, never from
// narrative (issue #106 test classes 11/14).
func TestWorkUnitsEvidenceRefsDerivedFromTaggedRows(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()
	unit := mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-1", Objective: "x"})

	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID:     wuTaskID,
		WorkUnitID: unit.WorkUnitID,
		Tool:       "read_file", Arguments: []byte(`{"path":"a.txt"}`),
		Fingerprint: "fp-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: wuTaskID, WorkUnitID: unit.WorkUnitID, ActionID: actionID,
		Tool: "read_file", Arguments: []byte(`{"path":"a.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: wuTaskID, ExecutionID: executionID, Status: "completed", EvidenceID: "obs-000001",
		DurationNanos: 1, Observation: tools.Observation{ID: "obs-000001", Tool: "read_file", Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, unit.WorkUnitID, "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, unit.WorkUnitID, "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, unit.WorkUnitID, "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	completed, err := store.GetWorkUnit(ctx, wuTaskID, unit.WorkUnitID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.EvidenceRefs) != 1 || completed.EvidenceRefs[0] != "obs-000001" {
		t.Fatalf("evidence refs = %v, want [obs-000001]", completed.EvidenceRefs)
	}

	// The provenance rows themselves carry the unit id (production-style
	// threading through the record structs, never manual SQL mutation).
	var actionUnit, attemptUnit string
	if err := store.db.QueryRowContext(ctx,
		`SELECT work_unit_id FROM actions WHERE action_id = ?`, actionID).Scan(&actionUnit); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT work_unit_id FROM tool_attempts WHERE execution_id = ?`, executionID).Scan(&attemptUnit); err != nil {
		t.Fatal(err)
	}
	if actionUnit != unit.WorkUnitID || attemptUnit != unit.WorkUnitID {
		t.Fatalf("provenance = action %q attempt %q, want %q", actionUnit, attemptUnit, unit.WorkUnitID)
	}
}

// TestWorkUnitsResetInterrupted proves the recovery-only running->ready reset
// never touches completed units (issue #106 test class 6).
func TestWorkUnitsResetInterrupted(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-1", Objective: "a"})
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-2", Objective: "b"})
	completeUnit(t, store, "wu-1")
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-2", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-2", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	reset, err := store.ResetInterruptedWorkUnits(ctx, wuTaskID, "interrupted")
	if err != nil || reset != 1 {
		t.Fatalf("ResetInterruptedWorkUnits() = %d, %v", reset, err)
	}
	w1, _ := store.GetWorkUnit(ctx, wuTaskID, "wu-1")
	if w1.Status != "completed" {
		t.Fatalf("completed unit was touched: %s", w1.Status)
	}
	w2, _ := store.GetWorkUnit(ctx, wuTaskID, "wu-2")
	if w2.Status != "ready" {
		t.Fatalf("interrupted unit = %s, want ready", w2.Status)
	}
}

// TestWorkUnitsObjectiveAndPlanBounds proves conservative create validation.
func TestWorkUnitsObjectiveAndPlanBounds(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-1", Objective: strings.Repeat("x", 4097),
	}); err == nil {
		t.Fatal("oversized objective accepted")
	}
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-2", Objective: "ok",
		AcceptancePlan: []byte(samplePlan), // digest missing
	}); err == nil {
		t.Fatal("plan without digest accepted")
	}
	if _, err := store.CreateWorkUnit(ctx, WorkUnitCreate{
		TaskID: wuTaskID, WorkUnitID: "wu-3", Objective: "ok",
		AcceptancePlan: []byte(`{"version":9,"checks":[]}`), AcceptanceDigest: "d",
	}); err == nil {
		t.Fatal("unsupported plan version accepted")
	}
}

func completeUnit(t *testing.T, store *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := store.TransitionWorkUnit(ctx, wuTaskID, id, "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, id, "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, wuTaskID, id, "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
}

// TestFinalizeTaskGatedByOpenWorkUnits proves the authoritative completion
// boundary: a task with persisted open Work Units can NEVER be finalized as
// 'completed', and once every unit is completed the same finalize succeeds
// (issue #106 review).
func TestFinalizeTaskGatedByOpenWorkUnits(t *testing.T) {
	store := openTestStore(t)
	mustWorkUnitTask(t, store, wuTaskID)
	ctx := context.Background()
	mustWorkUnit(t, store, WorkUnitCreate{TaskID: wuTaskID, WorkUnitID: "wu-open", Objective: "x"})
	bringTo(t, store, "wu-open", "running") // interrupted mid-run
	mustPassVerification(t, store, wuTaskID)
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: wuTaskID, Outcome: "completed", StopReason: "done"}); !errors.Is(err, ErrOpenWorkUnitsBlockFinalize) {
		t.Fatalf("FinalizeTask with open work unit error = %v, want ErrOpenWorkUnitsBlockFinalize", err)
	}
	status, err := store.TaskStatus(ctx, wuTaskID)
	if err != nil || status != "running" {
		t.Fatalf("task status = %q, %v; must stay running", status, err)
	}

	// Resolve the open unit; the same finalize now succeeds.
	if err := store.TransitionWorkUnit(ctx, wuTaskID, "wu-open", "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeTask(ctx, TaskFinalize{TaskID: wuTaskID, Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("FinalizeTask after all units completed: %v", err)
	}
	status, _ = store.TaskStatus(ctx, wuTaskID)
	if status != "completed" {
		t.Fatalf("task status after finalize = %q, want completed", status)
	}

	// A task WITHOUT any work unit is unaffected.
	store2 := openTestStore(t)
	if err := store2.CreateTask(ctx, TaskRecord{TaskID: "plain", Objective: "x", Workspace: "/ws"}); err != nil {
		t.Fatal(err)
	}
	if err := store2.StartTask(ctx, "plain"); err != nil {
		t.Fatal(err)
	}
	mustPassVerification(t, store2, "plain")
	if err := store2.FinalizeTask(ctx, TaskFinalize{TaskID: "plain", Outcome: "completed", StopReason: "done"}); err != nil {
		t.Fatalf("plain task finalize: %v", err)
	}
}
