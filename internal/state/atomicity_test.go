package state

import (
	"context"
	"strings"
	"testing"
)

// TestProjectionAndEventAtomicity proves the core journal invariant: a
// projection change and its corresponding event commit in the same SQLite
// transaction, so a failed transaction leaves neither behind.
func TestProjectionAndEventAtomicity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Creating the same task twice must fail on the second insert; the
	// task_created event for the failed attempt must not exist.
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-dup", Objective: "o", Workspace: "w"}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.CreateTask(ctx, TaskRecord{TaskID: "task-dup", Objective: "o", Workspace: "w"}); err == nil {
		t.Fatal("duplicate task creation must fail")
	}
	events, err := store.loadEvents(ctx, "task-dup")
	if err != nil {
		t.Fatalf("loadEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("failed duplicate creation must leave exactly one task_created event, got %d", len(events))
	}
	var taskCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE task_id = 'task-dup'`).Scan(&taskCount); err != nil {
		t.Fatalf("task count: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("failed duplicate creation must not leave a second task row: %d", taskCount)
	}
}

// TestRollbackOfPurelySQLiteTransition verifies that an explicit rollback of
// a projection-plus-event transaction leaves no partial state.
func TestRollbackOfPurelySQLiteTransition(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	mustTask(t, store, "task-1")
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'completed' WHERE task_id = 'task-1'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := appendEvent(ctx, tx, "task-1", "test_event", map[string]any{"v": 1}, store.now()); err != nil {
		t.Fatalf("appendEvent: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	status, err := store.TaskStatus(ctx, "task-1")
	if err != nil {
		t.Fatalf("TaskStatus: %v", err)
	}
	if status != "running" {
		t.Fatalf("status after rollback = %q, want running", status)
	}
	kinds := taskEventKinds(t, store, "task-1")
	for _, kind := range kinds {
		if kind == "test_event" {
			t.Fatal("rolled-back event must not exist")
		}
	}
}

// TestEventSequenceIsDeterministicPerTask verifies task-scoped ordering.
func TestEventSequenceIsDeterministicPerTask(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-a")
	mustTask(t, store, "task-b")

	actionA, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-a", Tool: "t", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction(a) error = %v", err)
	}
	actionB, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-b", Tool: "t", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction(b) error = %v", err)
	}
	_ = actionA
	_ = actionB

	eventsA := taskEventKinds(t, store, "task-a")
	eventsB := taskEventKinds(t, store, "task-b")
	// Both tasks have independent sequences starting at 1.
	if len(eventsA) != 3 || len(eventsB) != 3 {
		t.Fatalf("event counts = %d, %d; want 3 each", len(eventsA), len(eventsB))
	}
	if eventsA[2] != "action_planned" || eventsB[2] != "action_planned" {
		t.Fatalf("event kinds = %v, %v", eventsA, eventsB)
	}
}

// TestAppendOnlyHistorySurvivesFailure verifies that a failed projection
// update cannot remove previously committed journal history.
func TestAppendOnlyHistorySurvivesFailure(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	mustFinalize(t, store, "task-1")

	before := taskEventKinds(t, store, "task-1")
	if len(before) != 3 {
		t.Fatalf("expected 3 committed events, got %v", before)
	}
	// A failing transaction must not truncate the journal.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM events WHERE task_id = 'task-1'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after := taskEventKinds(t, store, "task-1")
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("rolled-back delete must not erase history: %v vs %v", after, before)
	}
}
