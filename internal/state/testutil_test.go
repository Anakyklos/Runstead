package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// fixedClock is a deterministic store time seam.
type fixedClock struct {
	now time.Time
}

func newFixedClock() *fixedClock {
	return &fixedClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) Now() time.Time { return c.now }

// openTestStore opens a store in a fresh temp directory with a fixed clock.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(Options{
		Path:  filepath.Join(t.TempDir(), "runstead.db"),
		Clock: newFixedClock(),
	})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// mustTask creates and starts a task, failing the test on error.
func mustTask(t *testing.T, store *Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, TaskRecord{TaskID: taskID, Objective: "inspect the workspace", Workspace: "/tmp/ws", Model: "scripted", ConfigJSON: []byte(`{"max_steps":24}`)}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
}

// mustToolAttempt records a successful read_file attempt with a canned
// observation, returning the execution id.
func mustToolAttempt(t *testing.T, store *Store, taskID, actionID string) string {
	t.Helper()
	ctx := context.Background()
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: taskID, ActionID: actionID, Tool: "read_file",
		Arguments: []byte(`{"path":"a.txt"}`), RecoveryClass: 1,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	observation := tools.Observation{
		ID:      "obs-000001",
		Tool:    "read_file",
		Success: true,
		Data:    map[string]any{"path": "a.txt", "content": "alpha\n"},
		Metadata: tools.Metadata{
			Source:    "read_file",
			Untrusted: true,
			Path:      "a.txt",
			ExitCode:  0,
		},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: taskID, ExecutionID: executionID, Status: "completed",
		EvidenceID: observation.ID, DurationNanos: 1000, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
	return executionID
}

// mustProviderAttempt records one governed provider completion with a
// success outcome through the store's governor.Persistence implementation.
func mustProviderAttempt(t *testing.T, store *Store, taskID, clientRequestID string, sequence int) {
	t.Helper()
	ctx := context.Background()
	state := governor.PersistedState{
		AccountPolicyID:  "policy-test",
		ProviderID:       "scripted",
		ModelPool:        "instant",
		Model:            "scripted",
		AllowanceProfile: governor.ProfileInstant,
		NextAttempt:      sequence + 1,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{
			Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2,
		},
		RollingEvents: []governor.LedgerEvent{{At: newFixedClock().Now(), TaskID: taskID}},
		TaskStates:    []governor.TaskStateRecord{{TaskID: taskID, Attempts: sequence, Retries: 0, LastTouched: newFixedClock().Now()}},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: taskID, ClientRequestID: clientRequestID, ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: sequence, StartedAt: newFixedClock().Now(), State: state,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	state.TaskStates[0].Attempts = sequence + 1
	state.NextAttempt = sequence + 2
	if err := store.RecordProviderFinished(ctx, governor.ProviderFinished{
		TaskID: taskID, ClientRequestID: clientRequestID, Outcome: governor.OutcomeSuccess,
		UpstreamReached: true, AttemptDebited: 1, Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
		State: state,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
}

// mustPassVerification records a passed control-plane verification attempt for
// the task, satisfying the issue #11 completion gate before a test finalizes
// as completed.
func mustPassVerification(t *testing.T, store *Store, taskID string) {
	t.Helper()
	report, err := json.Marshal(map[string]any{
		"task_id": taskID, "decision": "passed", "summary": "test verification passed",
	})
	if err != nil {
		t.Fatalf("marshal verification report: %v", err)
	}
	if err := store.SaveVerificationAttempt(context.Background(), VerificationAttemptRecord{
		TaskID: taskID, Decision: "passed", Summary: "test verification passed", ReportJSON: report,
		Checks: []VerificationCheckRecord{{
			CheckID: "evidence_grounded", Type: "structural", Status: "passed",
		}},
	}); err != nil {
		t.Fatalf("SaveVerificationAttempt() error = %v", err)
	}
}

// mustFinalize marks the task terminal.
func mustFinalize(t *testing.T, store *Store, taskID string) {
	t.Helper()
	mustPassVerification(t, store, taskID)
	if err := store.FinalizeTask(context.Background(), TaskFinalize{
		TaskID: taskID, Outcome: "completed", StopReason: "completion verified by the control plane",
		Summary: "done", Evidence: []string{"obs-000001"},
		Turns: 2, Attempts: 2, Observations: 1,
	}); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
}

// countRows returns the row count for a table.
func countRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// taskEventKinds returns the ordered journal kinds for a task.
func taskEventKinds(t *testing.T, store *Store, taskID string) []string {
	t.Helper()
	events, err := store.loadEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("loadEvents() error = %v", err)
	}
	kinds := make([]string, len(events))
	for index, event := range events {
		kinds[index] = event.Kind
	}
	return kinds
}

// governorFakeClock is a deterministic governor.Clock for state tests: it
// never sleeps in real time, so rate-event and cooldown scenarios run fast
// and deterministically.
type governorFakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*governorFakeTimer
}

type governorFakeTimer struct {
	clock *governorFakeClock
	at    time.Time
	c     chan time.Time
	mu    sync.Mutex
	stop  bool
	fired bool
}

func (t *governorFakeTimer) C() <-chan time.Time { return t.c }

func (t *governorFakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop || t.fired {
		return false
	}
	t.stop = true
	return true
}

func (c *governorFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *governorFakeClock) NewTimer(delay time.Duration) governor.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &governorFakeTimer{clock: c, at: c.now.Add(delay), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}

// Advance moves the clock forward and fires due timers.
func (c *governorFakeClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	timers := append([]*governorFakeTimer(nil), c.timers...)
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
