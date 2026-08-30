package workunit

// Bounded shared/exclusive scheduler tests (issue #109): deterministic
// concurrency proofs through real SQLite + channel/barrier RunFunc seams. No
// assertion depends on wall-clock timing; timeouts exist ONLY as deadlock
// detectors for the test itself.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// schedulerAllowedTools is the parent contract used by scheduler tests:
// every observational tool plus the effectful/unknown surfaces proven
// exclusive below.
var schedulerAllowedTools = []string{
	"read_file", "list_files", "search_text", "git_status", "git_diff",
	"write_file", "apply_patch", "run_recipe", "future_scanner",
}

// newSchedulerDriver builds a driver with real SQLite and the scheduler bound
// under test.
func newSchedulerDriver(t *testing.T, taskID string, concurrency int) (*Driver, *state.Store) {
	t.Helper()
	driver, store := newDriver(t, taskID)
	driver.AllowedTools = append([]string(nil), schedulerAllowedTools...)
	driver.Concurrency = concurrency
	return driver, store
}

// waitFor blocks until ch fires or the deadlock detector expires. It is a
// test-safety net, never a concurrency assertion.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timeout waiting for %s (scheduler deadlocked)", what)
	}
}

// barrier waits for n goroutines to arrive, then releases all of them.
// It is the deterministic synchronization primitive of these tests.
type barrier struct {
	mu       sync.Mutex
	n        int
	arrived  int
	release  chan struct{}
	released bool
}

func newBarrier(n int) *barrier {
	return &barrier{n: n, release: make(chan struct{})}
}

// wait blocks until every one of the n participants has arrived. After the
// first full wave, the release channel stays closed so later waves pass
// immediately (the scheduler never reuses a fixed-count barrier).
func (b *barrier) wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n && !b.released {
		b.released = true
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

// runChain runs the driver chain in a goroutine and returns the error
// channel, so tests can coordinate with mid-run barriers.
func runChain(t *testing.T, driver *Driver, ctx context.Context, run RunFunc) <-chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- driver.RunAll(ctx, run) }()
	return errCh
}

// waitChain waits for the chain result (deadlock detector).
func waitChain(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for the chain to finish (scheduler deadlocked)")
		return nil
	}
}

// readOnlyDefs returns N independent shared-lane definitions (explicit
// read_file envelope).
func readOnlyDefs(prefix string, n int) []Definition {
	defs := make([]Definition, 0, n)
	for i := 0; i < n; i++ {
		defs = append(defs, Definition{
			WorkUnitID: fmt.Sprintf("%s-%d", prefix, i+1),
			Objective:  fmt.Sprintf("read-only unit %d", i+1),
			Tools:      []string{"read_file"},
		})
	}
	return defs
}

// completeRun saves the unit's own passed verification and returns completed,
// mirroring what a REAL loop does after its acceptance plan passes.
func completeRun(t *testing.T, store *state.Store, unit state.WorkUnit) (RunResult, error) {
	t.Helper()
	if err := store.SaveVerificationAttempt(context.Background(), state.VerificationAttemptRecord{
		TaskID: unit.TaskID, WorkUnitID: unit.WorkUnitID, Decision: "passed", Summary: "verified",
	}); err != nil {
		return RunResult{}, err
	}
	return RunResult{Outcome: "completed"}, nil
}

// violation records a deterministic rule breach from inside a RunFunc. The
// breaching run also returns an error so the chain stops and the test fails
// even if the channel check is skipped.
func violation(reason string, state chan<- struct{}) (RunResult, error) {
	state <- struct{}{}
	return RunResult{}, errors.New(reason)
}

// TestSchedulerConcurrencyOnePreservesSerial proves acceptance item 1:
// concurrency=1 keeps the exact Stage A serial contract (deterministic
// order, strictly one unit active at a time).
func TestSchedulerConcurrencyOnePreservesSerial(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-serial", 1)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 3)); err != nil {
		t.Fatal(err)
	}
	var order []string
	var mu sync.Mutex
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
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
			return RunResult{}, errors.New("violated serial execution at concurrency=1")
		}
		mu.Lock()
		order = append(order, unit.WorkUnitID)
		mu.Unlock()
		return completeRun(t, store, unit)
	}
	if err := driver.RunAll(ctx, run); err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	if strings.Join(order, ",") != "wu-1,wu-2,wu-3" {
		t.Fatalf("execution order = %v, want wu-1,wu-2,wu-3", order)
	}
	if err := driver.GateParent(ctx); err != nil {
		t.Fatalf("GateParent(): %v", err)
	}
}

// TestSchedulerTwoReadOnlyUnitsConcurrentlyActive proves acceptance item 2:
// two independent explicitly read-only units are simultaneously ACTIVE under
// concurrency=2. A barrier proves overlap: each unit waits for the other
// before completing. A serial implementation would deadlock and trip the
// detector.
func TestSchedulerTwoReadOnlyUnitsConcurrentlyActive(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-overlap", 2)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 2)); err != nil {
		t.Fatal(err)
	}
	both := newBarrier(2)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		both.wait()
		return completeRun(t, store, unit)
	}
	if err := driver.RunAll(ctx, run); err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	unit1, _ := store.GetWorkUnit(ctx, "wu-overlap", "wu-1")
	unit2, _ := store.GetWorkUnit(ctx, "wu-overlap", "wu-2")
	if unit1.Status != "completed" || unit2.Status != "completed" {
		t.Fatalf("statuses = %s/%s, want completed/completed", unit1.Status, unit2.Status)
	}
}

// TestSchedulerMaxNeverExceeded proves acceptance item 3: the configured
// bound is never surpassed even with more ready units than slots. Every run
// increments a guarded overlap counter and waits for its wave of TWO runs to
// arrive before completing, so a hypothetical over-dispatch is observed
// deterministically (a third concurrent run either raises maxActive above 2
// or deadlocks the wave barrier, both failing the test).
func TestSchedulerMaxNeverExceeded(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-bound", 2)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 4)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var active, maxActive int
	violationCh := make(chan struct{}, 1)
	wave := newBarrier(2)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		exceeded := active > 2
		mu.Unlock()
		if exceeded {
			return violation("bound exceeded: 3 active", violationCh)
		}
		wave.wait()
		if err := store.SaveVerificationAttempt(context.Background(), state.VerificationAttemptRecord{
			TaskID: unit.TaskID, WorkUnitID: unit.WorkUnitID, Decision: "passed", Summary: "verified",
		}); err != nil {
			return RunResult{}, err
		}
		mu.Lock()
		active--
		mu.Unlock()
		return RunResult{Outcome: "completed"}, nil
	}
	err := waitChain(t, runChain(t, driver, ctx, run))
	select {
	case <-violationCh:
		t.Fatal("concurrency bound exceeded")
	default:
	}
	if err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 2 {
		t.Fatalf("maxActive = %d, want 2", maxActive)
	}
}

// TestSchedulerDependenciesPreservedUnderConcurrency proves acceptance item
// 4: a dependent unit is never dispatched before its dependency is durably
// completed, even while independent units run concurrently.
func TestSchedulerDependenciesPreservedUnderConcurrency(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-deps", 2)
	ctx := context.Background()
	definitions := append(readOnlyDefs("wu", 2),
		Definition{WorkUnitID: "wu-3", Objective: "dependent", Tools: []string{"read_file"}, Dependencies: []string{"wu-1"}},
	)
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}
	enteredA := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	wu1Completed := make(chan struct{})
	var wu1DoneOnce sync.Once
	violated := make(chan struct{}, 1)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		switch unit.WorkUnitID {
		case "wu-1":
			enteredA <- struct{}{}
			// Hold wu-1's run open WITHOUT completing: if wu-3 is dispatched
			// during this window, the scheduler ignored durable deps.
			waitFor(t, releaseA, "wu-1 release")
			result, err := completeRun(t, store, unit)
			wu1DoneOnce.Do(func() { close(wu1Completed) })
			return result, err
		case "wu-3":
			select {
			case <-wu1Completed:
				return completeRun(t, store, unit)
			default:
				return violation("dependent unit dispatched before its dependency completed", violated)
			}
		default:
			return completeRun(t, store, unit)
		}
	}
	errCh := runChain(t, driver, ctx, run)
	waitFor(t, enteredA, "wu-1 to enter")
	close(releaseA)
	err := waitChain(t, errCh)
	select {
	case <-violated:
		t.Fatal("dependent unit dispatched before its dependency completed")
	default:
	}
	if err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	unit3, _ := store.GetWorkUnit(ctx, "wu-deps", "wu-3")
	if unit3.Status != "completed" {
		t.Fatalf("wu-3 = %s, want completed", unit3.Status)
	}
}

// exclusiveTrapDriver runs an exclusive unit against shared traps: any shared
// unit entering while the exclusive unit's run is still open is a violation
// (acceptance items 5/6/7/8).
func exclusiveTrapDriver(t *testing.T, taskID string, exclusiveDef Definition, sharedCount int) {
	t.Helper()
	driver, store := newSchedulerDriver(t, taskID, 2)
	ctx := context.Background()
	definitions := append([]Definition{exclusiveDef}, readOnlyDefs("wu", sharedCount)...)
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}
	exclusiveEntered := make(chan struct{}, 1)
	releaseExclusive := make(chan struct{})
	violated := make(chan struct{}, 1)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if unit.WorkUnitID == exclusiveDef.WorkUnitID {
			exclusiveEntered <- struct{}{}
			waitFor(t, releaseExclusive, "exclusive unit release")
			return completeRun(t, store, unit)
		}
		// Shared-trap: entering while the exclusive unit has not settled is
		// an overlap violation.
		select {
		case <-releaseExclusive:
			return completeRun(t, store, unit)
		default:
			return violation("shared unit overlapped an exclusive unit", violated)
		}
	}
	errCh := runChain(t, driver, ctx, run)
	waitFor(t, exclusiveEntered, "exclusive unit to enter")
	close(releaseExclusive)
	err := waitChain(t, errCh)
	select {
	case <-violated:
		t.Fatal("a shared unit overlapped an exclusive unit")
	default:
	}
	if err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
}

// TestSchedulerExclusiveNeverOverlapsReadOnly proves acceptance item 6: an
// exclusive unit never overlaps a read-only unit.
func TestSchedulerExclusiveNeverOverlapsReadOnly(t *testing.T) {
	exclusiveTrapDriver(t, "wu-excl-readonly", Definition{
		WorkUnitID: "wu-x", Objective: "exclusive", Tools: []string{"write_file"},
	}, 2)
}

// TestSchedulerOmittedEnvelopeIsExclusive proves acceptance item 6/7: an
// omitted (nil) envelope is exclusive and never overlaps.
func TestSchedulerOmittedEnvelopeIsExclusive(t *testing.T) {
	exclusiveTrapDriver(t, "wu-excl-nil", Definition{
		WorkUnitID: "wu-nil", Objective: "task default surface",
	}, 2)
}

// TestSchedulerEffectfulAndUnknownToolsAreExclusive proves acceptance items
// 7/8: write_file, apply_patch, run_recipe and a future/unknown tool are all
// exclusive and never overlap read-only units.
func TestSchedulerEffectfulAndUnknownToolsAreExclusive(t *testing.T) {
	for _, tool := range []string{"write_file", "apply_patch", "run_recipe", "future_scanner"} {
		t.Run(tool, func(t *testing.T) {
			exclusiveTrapDriver(t, "wu-excl-"+tool, Definition{
				WorkUnitID: "wu-e", Objective: "effectful/unknown", Tools: []string{tool},
			}, 2)
		})
	}
}

// TestSchedulerExclusiveNeverOverlapsExclusive proves acceptance item 5: an
// exclusive unit never overlaps another exclusive unit.
func TestSchedulerExclusiveNeverOverlapsExclusive(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-excl-excl", 2)
	ctx := context.Background()
	definitions := []Definition{
		{WorkUnitID: "wu-a", Objective: "exclusive a"},
		{WorkUnitID: "wu-b", Objective: "exclusive b"},
	}
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}
	firstEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	violated := make(chan struct{}, 1)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if unit.WorkUnitID == "wu-a" {
			firstEntered <- struct{}{}
			waitFor(t, releaseFirst, "first exclusive unit release")
			return completeRun(t, store, unit)
		}
		select {
		case <-releaseFirst:
			return completeRun(t, store, unit)
		default:
			return violation("exclusive unit overlapped another exclusive unit", violated)
		}
	}
	errCh := runChain(t, driver, ctx, run)
	waitFor(t, firstEntered, "first exclusive unit to enter")
	close(releaseFirst)
	err := waitChain(t, errCh)
	select {
	case <-violated:
		t.Fatal("exclusive units overlapped")
	default:
	}
	if err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
}

// TestSchedulerExclusiveReadyNotStarvedByReaders proves acceptance item 10:
// an exclusive unit that becomes ready while a shared batch is active blocks
// NEW shared dispatch; the exclusive runs before any later read-only unit.
func TestSchedulerExclusiveReadyNotStarvedByReaders(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-starve", 2)
	ctx := context.Background()
	definitions := append(readOnlyDefs("wu", 3),
		Definition{WorkUnitID: "wu-e", Objective: "exclusive"}) // nil envelope = exclusive
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}
	exclusiveDone := make(chan struct{})
	var exclusiveDoneOnce sync.Once
	violated := make(chan struct{}, 1)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if unit.WorkUnitID == "wu-e" {
			if err := store.SaveVerificationAttempt(context.Background(), state.VerificationAttemptRecord{
				TaskID: unit.TaskID, WorkUnitID: unit.WorkUnitID, Decision: "passed", Summary: "verified",
			}); err != nil {
				return RunResult{}, err
			}
			exclusiveDoneOnce.Do(func() { close(exclusiveDone) })
			return RunResult{Outcome: "completed"}, nil
		}
		if unit.WorkUnitID == "wu-3" {
			// wu-3 must not start while wu-e is READY AND WAITING: wu-1 and
			// wu-2 fill the two slots, wu-1 settles first, and the scheduler
			// must dispatch wu-e (after wu-2 drains) BEFORE wu-3.
			select {
			case <-exclusiveDone:
				return completeRun(t, store, unit)
			default:
				return violation("read-only unit dispatched while a ready exclusive waited", violated)
			}
		}
		return completeRun(t, store, unit)
	}
	err := waitChain(t, runChain(t, driver, ctx, run))
	select {
	case <-violated:
		t.Fatal("a read-only unit was dispatched while a ready exclusive waited")
	default:
	}
	if err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
}

// TestSchedulerFailureStopsNewBatchesAfterCurrentSettles proves acceptance
// item 12: a failed/blocked/uncertain sibling stops NEW scheduling, the
// current bounded batch settles to durable states, the scheduler then
// returns ErrWorkUnitBlockedChain and the parent gate stays open.
func TestSchedulerFailureStopsNewBatchesAfterCurrentSettles(t *testing.T) {
	for _, outcome := range []string{"failed", "blocked", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			taskID := "wu-batch-" + outcome
			driver, store := newSchedulerDriver(t, taskID, 2)
			ctx := context.Background()
			if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 4)); err != nil {
				t.Fatal(err)
			}
			firstFailed := make(chan struct{}, 1)
			releaseSecond := make(chan struct{})
			var wu3Entered, wu4Entered int32
			run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
				switch unit.WorkUnitID {
				case "wu-1":
					firstFailed <- struct{}{}
					return RunResult{Outcome: outcome, Reason: "scripted " + outcome}, nil
				case "wu-2":
					// Already dispatched: the batch must be allowed to
					// settle, not cancelled early.
					waitFor(t, releaseSecond, "second unit release")
					return completeRun(t, store, unit)
				case "wu-3":
					atomic.StoreInt32(&wu3Entered, 1)
					return completeRun(t, store, unit)
				default:
					atomic.StoreInt32(&wu4Entered, 1)
					return completeRun(t, store, unit)
				}
			}
			errCh := runChain(t, driver, ctx, run)
			waitFor(t, firstFailed, "first unit to settle "+outcome)
			close(releaseSecond)
			err := waitChain(t, errCh)
			if !errors.Is(err, ErrWorkUnitBlockedChain) {
				t.Fatalf("RunAll() = %v, want ErrWorkUnitBlockedChain", err)
			}
			if wu3Entered != 0 || wu4Entered != 0 {
				t.Fatalf("new batches started after %s: wu-3=%d wu-4=%d", outcome, wu3Entered, wu4Entered)
			}
			statuses := map[string]string{}
			for _, id := range []string{"wu-1", "wu-2", "wu-3", "wu-4"} {
				unit, err := store.GetWorkUnit(ctx, taskID, id)
				if err != nil {
					t.Fatal(err)
				}
				statuses[id] = unit.Status
			}
			if statuses["wu-1"] != outcome {
				t.Fatalf("wu-1 = %s, want %s", statuses["wu-1"], outcome)
			}
			if statuses["wu-2"] != "completed" {
				t.Fatalf("wu-2 = %s, want completed (batch settled)", statuses["wu-2"])
			}
			if statuses["wu-3"] != "created" || statuses["wu-4"] != "created" {
				t.Fatalf("later units must stay untouched: %v", statuses)
			}
			if err := driver.GateParent(ctx); !errors.Is(err, ErrParentCompletionGated) {
				t.Fatalf("GateParent() = %v, want gated", err)
			}
		})
	}
}

// TestSchedulerCancellationPropagatesAndLeaksNothing proves acceptance item
// 13: cancellation propagates to every active unit, no new unit starts, the
// interrupted units stay durably recoverable ('running'), and RunAll does
// not return while worker goroutines are alive.
func TestSchedulerCancellationPropagatesAndLeaksNothing(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-cancel", 2)
	ctx, cancel := context.WithCancel(context.Background())
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 3)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	var wu3Entered int32
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if unit.WorkUnitID == "wu-3" {
			atomic.StoreInt32(&wu3Entered, 1)
			return completeRun(t, store, unit)
		}
		entered <- struct{}{}
		<-ctx.Done()
		return RunResult{Outcome: "canceled", Reason: "context canceled"}, nil
	}
	errCh := runChain(t, driver, ctx, run)
	waitFor(t, entered, "first active unit to enter")
	waitFor(t, entered, "second active unit to enter")
	cancel()
	err := waitChain(t, errCh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll() = %v, want wrapped context.Canceled", err)
	}
	if wu3Entered != 0 {
		t.Fatal("a new unit started after cancellation")
	}
	unit1, err1 := store.GetWorkUnit(context.Background(), "wu-cancel", "wu-1")
	unit2, err2 := store.GetWorkUnit(context.Background(), "wu-cancel", "wu-2")
	if err1 != nil || err2 != nil {
		t.Fatalf("reload interrupted units: %v / %v", err1, err2)
	}
	if unit1.Status != "running" || unit2.Status != "running" {
		t.Fatalf("interrupted units = %s/%s, want running/running (durably recoverable)", unit1.Status, unit2.Status)
	}
	if err := driver.GateParent(context.Background()); !errors.Is(err, ErrParentCompletionGated) {
		t.Fatalf("GateParent() = %v, want gated", err)
	}
}

// TestSchedulerCanceledOutcomeSurvivesBoundary proves the Stage A contract
// preserved under the scheduler: a canceled outcome (not a ctx cancel) stops
// NEW dispatch, the already-dispatched batch settles to durable states (a
// legitimately completing sibling stays completed), the canceled unit stays
// 'running' for recovery reset, and the wrapped context.Canceled error
// survives the boundary.
func TestSchedulerCanceledOutcomeSurvivesBoundary(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-canceled-outcome", 2)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 3)); err != nil {
		t.Fatal(err)
	}
	var wu3Entered int32
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		switch unit.WorkUnitID {
		case "wu-1":
			return RunResult{Outcome: "canceled", Reason: "loop canceled"}, nil
		case "wu-2":
			// Already-dispatched sibling: settles normally (no artificial
			// sibling cancellation).
			return completeRun(t, store, unit)
		default:
			atomic.StoreInt32(&wu3Entered, 1)
			return completeRun(t, store, unit)
		}
	}
	err := driver.RunAll(ctx, run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll() = %v, want wrapped context.Canceled", err)
	}
	if wu3Entered != 0 {
		t.Fatal("a new unit dispatched after a canceled outcome")
	}
	unit1, _ := store.GetWorkUnit(ctx, "wu-canceled-outcome", "wu-1")
	if unit1.Status != "running" {
		t.Fatalf("canceled unit = %s, want running (recovery reset)", unit1.Status)
	}
	unit2, _ := store.GetWorkUnit(ctx, "wu-canceled-outcome", "wu-2")
	if unit2.Status != "completed" {
		t.Fatalf("settled sibling = %s, want completed", unit2.Status)
	}
}

// TestSchedulerOutOfRangeConcurrencyFailsClosed proves invalid bounds fail
// BEFORE any unit executes (acceptance items: invalid values fail before
// executing Work Units).
func TestSchedulerOutOfRangeConcurrencyFailsClosed(t *testing.T) {
	for _, invalid := range []int{-1, 5, 100} {
		driver, store := newSchedulerDriver(t, "wu-invalid", invalid)
		ctx := context.Background()
		if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 1)); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{}, 1)
		err := driver.RunAll(ctx, func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
			entered <- struct{}{}
			return completeRun(t, store, unit)
		})
		if !errors.Is(err, ErrWorkUnitConcurrency) {
			t.Fatalf("RunAll(concurrency=%d) = %v, want ErrWorkUnitConcurrency", invalid, err)
		}
		select {
		case <-entered:
			t.Fatalf("unit executed despite invalid concurrency %d", invalid)
		default:
		}
		unit, _ := store.GetWorkUnit(ctx, "wu-invalid", "wu-1")
		if unit.Status != "created" {
			t.Fatalf("unit = %s, want created (never dispatched)", unit.Status)
		}
	}
}

// TestSchedulerVerificationStillGatesUnderConcurrency proves a shared unit's
// "completed" narrative still requires ITS OWN persisted passed verification:
// a sibling can never satisfy another unit's acceptance (acceptance item:
// sibling cannot satisfy checks by narrative).
func TestSchedulerVerificationStillGatesUnderConcurrency(t *testing.T) {
	driver, store := newSchedulerDriver(t, "wu-verif", 2)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", 2)); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if unit.WorkUnitID == "wu-1" {
			// wu-1 gets its OWN passed verification (a real sibling).
			return completeRun(t, store, unit)
		}
		// wu-2 claims completed with NO verification row of its own.
		return RunResult{Outcome: "completed"}, nil
	}
	err := driver.RunAll(ctx, run)
	if !errors.Is(err, ErrWorkUnitBlockedChain) {
		t.Fatalf("RunAll() = %v, want blocked chain", err)
	}
	unit2, _ := store.GetWorkUnit(ctx, "wu-verif", "wu-2")
	if unit2.Status != "blocked" {
		t.Fatalf("wu-2 = %s, want blocked (verification missing)", unit2.Status)
	}
}
