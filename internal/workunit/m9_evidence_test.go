package workunit

// M9 evidence-gate corpus (issue #53): deterministic, reproducible evaluation
// of the Stage B1 bounded shared/exclusive scheduler (issue #109) at driver
// level with REAL SQLite, channel/barrier RunFunc seams and (scenario D) the
// REAL account governor. The corpus answers the M9 exit-gate question
// "does opt-in read-only Work Unit concurrency produce measurable task-level
// benefit without weakening governor, durability, evidence, recovery or
// verifier" with four scenarios:
//
//   A fan-out:        independent explicit read-only units; concurrency=2/4
//                     must shorten the structural critical path (wave depth
//                     ceil(N/C) vs N serial waves) with no bound violation.
//   B dependency:     a read-only chain; raising the bound must NOT invent
//                     parallelism where the DAG forbids it (chain depth
//                     stays N at every concurrency).
//   C mixed:          read-only fan-out + one exclusive unit; exclusivity is
//                     never violated and the exclusive-lane barrier (here a
//                     declared write_file envelope; the fake RunFunc executes
//                     no write) caps the achievable depth (1 + ceil(shared/C)).
//   D governor:       concurrent read-only units whose provider attempts all
//                     flow through ONE real governor instance; the scheduler
//                     overlaps local work while the governor serializes
//                     physical attempts (MaxInFlight=1, exactly-once).
//
// Every assertion is deterministic: barriers and channels prove overlap and
// exclusion; counting proves bounds, ordering, accounting and provenance;
// the only timeouts are deadlock detectors for the test itself. No wall-clock
// timing is asserted anywhere (the wall-clock benefit evaluation lives in the
// separate benchmark harness under cmd/runstead, executed and reported
// separately).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// m9Tracker records the deterministic concurrency facts of one scenario run:
// simultaneous active count (with its observed maximum), entry order, the
// wave generation of every unit, and the total work-tick accounting. It is
// the measurement instrument of this corpus.
type m9Tracker struct {
	mu          sync.Mutex
	active      int
	maxActive   int
	order       []string
	generations map[string]int
	ticks       int64
}

func newM9Tracker() *m9Tracker {
	return &m9Tracker{generations: make(map[string]int)}
}

// enter marks a unit active and records its entry order. It returns false
// when the active count would exceed bound (a deterministic violation).
func (t *m9Tracker) enter(unitID string, bound int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active++
	if t.active > t.maxActive {
		t.maxActive = t.active
	}
	t.order = append(t.order, unitID)
	return t.active <= bound
}

func (t *m9Tracker) leave() {
	t.mu.Lock()
	t.active--
	t.mu.Unlock()
}

func (t *m9Tracker) noteGeneration(unitID string, generation int) {
	t.mu.Lock()
	t.generations[unitID] = generation
	t.mu.Unlock()
}

// tick adds exactly n work ticks to the shared accounting counter. Every run
// performs its declared number of ticks regardless of interleaving, so the
// total is deterministic: no work is lost or duplicated under concurrency.
func (t *m9Tracker) tick(n int) {
	t.mu.Lock()
	t.ticks += int64(n)
	t.mu.Unlock()
}

func (t *m9Tracker) snapshot() m9Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := m9Snapshot{
		maxActive:   t.maxActive,
		order:       append([]string(nil), t.order...),
		generations: make(map[string]int, len(t.generations)),
		ticks:       t.ticks,
	}
	for id, gen := range t.generations {
		snap.generations[id] = gen
	}
	return snap
}

type m9Snapshot struct {
	maxActive   int
	order       []string
	generations map[string]int
	ticks       int64
}

// m9WaveGate forces the canonical wave schedule of a scenario without any
// wall-clock dependency: units of one wave arrive at the gate and are only
// released when the whole wave has arrived, so a full wave of `perWave` units
// is PROVEN simultaneously active before any of them completes. A unit that
// arrives after a wave is already full starts the next wave; the generation
// it joins is returned. The FINAL partial wave (fewer than perWave units
// remain) is released immediately on its last arrival, so a workload of 3
// units at wave size 2 completes in waves of 2+1 without deadlock. An arrival
// count inconsistent with the dispatch bound blocks forever, tripping the
// test's deadlock detector.
type m9WaveGate struct {
	mu           sync.Mutex
	perWave      int
	total        int
	arrivedTotal int
	arrived      int
	closed       int
	gate         chan struct{}
}

func newM9WaveGate(perWave, total int) *m9WaveGate {
	return &m9WaveGate{perWave: perWave, total: total, gate: make(chan struct{})}
}

// arrive joins the current wave and blocks until the wave is released. It
// returns the deterministic generation (1-based wave number) the unit joined.
func (g *m9WaveGate) arrive() int {
	g.mu.Lock()
	g.arrivedTotal++
	if g.arrived == g.perWave {
		g.arrived = 0
		g.gate = make(chan struct{})
	}
	g.arrived++
	generation := g.closed + 1
	ch := g.gate
	// A full wave releases; the FINAL partial wave releases on its last
	// arrival so the scheduler is never left waiting for a phantom unit.
	if g.arrived == g.perWave || g.arrivedTotal == g.total {
		g.closed++
		close(ch)
	}
	g.mu.Unlock()
	<-ch
	return generation
}

// maxGeneration returns the highest wave generation any unit joined.
func (g *m9WaveGate) maxGeneration() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// chainDefs returns a read-only dependency chain wu-1 -> wu-2 -> ... -> wu-n.
func chainDefs(prefix string, n int) []Definition {
	defs := make([]Definition, 0, n)
	for i := 1; i <= n; i++ {
		def := Definition{
			WorkUnitID: fmt.Sprintf("%s-%d", prefix, i),
			Objective:  fmt.Sprintf("chain unit %d", i),
			Tools:      []string{"read_file"},
		}
		if i > 1 {
			def.Dependencies = []string{fmt.Sprintf("%s-%d", prefix, i-1)}
		}
		defs = append(defs, def)
	}
	return defs
}

// m9Violation is a deterministic rule breach observed inside a RunFunc. The
// breaching run also returns an error so the chain stops even if the channel
// check is skipped.
func m9Violation(reason string, violations chan<- string) (RunResult, error) {
	violations <- reason
	return RunResult{}, errors.New(reason)
}

// m9Completed completes a unit exactly like the real loop after its own
// acceptance plan passes.
func m9Completed(t *testing.T, store *state.Store, unit state.WorkUnit) (RunResult, error) {
	t.Helper()
	return completeRun(t, store, unit)
}

// ---------------------------------------------------------------------------
// Scenario A — read-only fan-out.
//
// Structural critical path: at concurrency=1 the fan-out needs N sequential
// waves; at concurrency=C it must fit in ceil(N/C) waves, with every wave
// PROVEN fully occupied (all C units simultaneously active before any
// completes) and the bound never exceeded. Work-tick accounting stays exact
// (N*K ticks total) at every concurrency: no lost or duplicated work.
// ---------------------------------------------------------------------------

func TestM9EvidenceScenarioAFanOut(t *testing.T) {
	const (
		units     = 4
		workTicks = 500
	)
	for _, concurrency := range []int{1, 2, 4} {
		concurrency := concurrency
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			taskID := fmt.Sprintf("m9-a-%d", concurrency)
			driver, store := newSchedulerDriver(t, taskID, concurrency)
			ctx := context.Background()
			if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", units)); err != nil {
				t.Fatal(err)
			}
			tracker := newM9Tracker()
			gate := newM9WaveGate(concurrency, units)
			violations := make(chan string, 1)
			run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
				if !tracker.enter(unit.WorkUnitID, concurrency) {
					return m9Violation(fmt.Sprintf("bound exceeded: %d active at concurrency=%d", tracker.maxActive, concurrency), violations)
				}
				defer tracker.leave()
				tracker.noteGeneration(unit.WorkUnitID, gate.arrive())
				tracker.tick(workTicks)
				return m9Completed(t, store, unit)
			}
			err := waitChain(t, runChain(t, driver, ctx, run))
			select {
			case reason := <-violations:
				t.Fatalf("scenario A violation: %s", reason)
			default:
			}
			if err != nil {
				t.Fatalf("RunAll(): %v", err)
			}
			snap := tracker.snapshot()
			wantMaxActive := concurrency
			if concurrency > units {
				wantMaxActive = units
			}
			if snap.maxActive != wantMaxActive {
				t.Fatalf("maxActive = %d, want %d (full wave occupancy)", snap.maxActive, wantMaxActive)
			}
			// Sequential depth: N waves at 1, ceil(N/C) at C>1.
			wantWaves := (units + concurrency - 1) / concurrency
			if got := gate.maxGeneration(); got != wantWaves {
				t.Fatalf("wave depth = %d, want %d (=ceil(%d/%d))", got, wantWaves, units, concurrency)
			}
			if snap.ticks != units*workTicks {
				t.Fatalf("work ticks = %d, want %d (exact accounting)", snap.ticks, units*workTicks)
			}
			// At concurrency=1 the fan-out is ALSO exactly the Stage A serial
			// order.
			if concurrency == 1 && strings.Join(snap.order, ",") != "wu-1,wu-2,wu-3,wu-4" {
				t.Fatalf("serial order = %v, want wu-1..wu-4", snap.order)
			}
			for i := 1; i <= units; i++ {
				unit, err := store.GetWorkUnit(ctx, taskID, fmt.Sprintf("wu-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if unit.Status != "completed" {
					t.Fatalf("wu-%d = %s, want completed", i, unit.Status)
				}
			}
			if err := driver.GateParent(ctx); err != nil {
				t.Fatalf("GateParent(): %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario B — dependent chain (pure chain, then chain + independent fan).
//
// Raising the bound must not invent parallelism where the DAG forbids it: a
// pure read-only chain has depth exactly N at every concurrency (maxActive
// stays 1, order stays exact, a dependent never enters before its dependency
// is durably completed). When an independent fan-out coexists with the chain,
// the scheduler still overlaps the independent units while preserving the
// chain order and the bound.
// ---------------------------------------------------------------------------

func TestM9EvidenceScenarioBDependencyChain(t *testing.T) {
	t.Run("pure-chain-depth-preserved", func(t *testing.T) {
		const chain = 5
		taskID := "m9-b-chain"
		driver, store := newSchedulerDriver(t, taskID, 4)
		ctx := context.Background()
		if _, _, err := driver.EnsureDefinitions(ctx, chainDefs("ch", chain)); err != nil {
			t.Fatal(err)
		}
		tracker := newM9Tracker()
		violations := make(chan string, 1)
		run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
			for _, dep := range unit.Dependencies {
				depUnit, err := store.GetWorkUnit(ctx, unit.TaskID, dep)
				if err != nil {
					return RunResult{}, err
				}
				if depUnit.Status != "completed" {
					return m9Violation("dependent dispatched before dependency "+dep+" completed", violations)
				}
			}
			if !tracker.enter(unit.WorkUnitID, 1) {
				return m9Violation("chain invented parallelism: >1 chain unit active", violations)
			}
			defer tracker.leave()
			return m9Completed(t, store, unit)
		}
		err := waitChain(t, runChain(t, driver, ctx, run))
		select {
		case reason := <-violations:
			t.Fatalf("scenario B violation: %s", reason)
		default:
		}
		if err != nil {
			t.Fatalf("RunAll(): %v", err)
		}
		snap := tracker.snapshot()
		if snap.maxActive != 1 {
			t.Fatalf("maxActive = %d, want 1: the DAG forbids parallelism", snap.maxActive)
		}
		if strings.Join(snap.order, ",") != "ch-1,ch-2,ch-3,ch-4,ch-5" {
			t.Fatalf("chain order = %v, want ch-1..ch-5", snap.order)
		}
		for i := 1; i <= chain; i++ {
			unit, err := store.GetWorkUnit(ctx, taskID, fmt.Sprintf("ch-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			if unit.Status != "completed" {
				t.Fatalf("ch-%d = %s, want completed", i, unit.Status)
			}
		}
		if err := driver.GateParent(ctx); err != nil {
			t.Fatalf("GateParent(): %v", err)
		}
	})

	t.Run("chain-plus-independent-fan", func(t *testing.T) {
		taskID := "m9-b-mixed"
		driver, store := newSchedulerDriver(t, taskID, 4)
		ctx := context.Background()
		definitions := append(chainDefs("ch", 3), readOnlyDefs("ind", 3)...)
		if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
			t.Fatal(err)
		}
		tracker := newM9Tracker()
		independentWave := newM9WaveGate(3, 3)
		violations := make(chan string, 1)
		run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
			if strings.HasPrefix(unit.WorkUnitID, "ch-") {
				for _, dep := range unit.Dependencies {
					depUnit, err := store.GetWorkUnit(ctx, unit.TaskID, dep)
					if err != nil {
						return RunResult{}, err
					}
					if depUnit.Status != "completed" {
						return m9Violation("chain unit dispatched before dependency "+dep+" completed", violations)
					}
				}
				if !tracker.enter(unit.WorkUnitID, 4) {
					return m9Violation("bound exceeded: more than 4 active", violations)
				}
				defer tracker.leave()
				return m9Completed(t, store, unit)
			}
			if !tracker.enter(unit.WorkUnitID, 4) {
				return m9Violation("bound exceeded: more than 4 active", violations)
			}
			defer tracker.leave()
			// The three INDEPENDENT units must all be simultaneously active in
			// one wave while the chain progresses: the scheduler reuses the
			// lane for provably independent work without disturbing chain
			// order.
			tracker.noteGeneration(unit.WorkUnitID, independentWave.arrive())
			return m9Completed(t, store, unit)
		}
		err := waitChain(t, runChain(t, driver, ctx, run))
		select {
		case reason := <-violations:
			t.Fatalf("scenario B violation: %s", reason)
		default:
		}
		if err != nil {
			t.Fatalf("RunAll(): %v", err)
		}
		snap := tracker.snapshot()
		if snap.maxActive > 4 {
			t.Fatalf("maxActive = %d, want <= 4", snap.maxActive)
		}
		if independentWave.maxGeneration() != 1 {
			t.Fatalf("independent wave depth = %d, want 1 (all three overlapping)", independentWave.maxGeneration())
		}
		for _, id := range []string{"ind-1", "ind-2", "ind-3"} {
			if snap.generations[id] != 1 {
				t.Fatalf("%s generation = %d, want 1", id, snap.generations[id])
			}
		}
		// Chain order is preserved exactly even while independents overlap.
		chainOrder := make([]string, 0, 3)
		for _, id := range snap.order {
			if strings.HasPrefix(id, "ch-") {
				chainOrder = append(chainOrder, id)
			}
		}
		if strings.Join(chainOrder, ",") != "ch-1,ch-2,ch-3" {
			t.Fatalf("chain order = %v, want ch-1,ch-2,ch-3", chainOrder)
		}
		if err := driver.GateParent(ctx); err != nil {
			t.Fatalf("GateParent(): %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Scenario C — mixed shared/exclusive.
//
// An exclusive unit with a declared write_file envelope (lane classification
// is what is under test; the fake RunFunc executes no write) is READY from
// the start, so the scheduler's exclusive-first rule dispatches it before any
// shared unit; the exclusive unit PROVABLY never overlaps anything, and the
// shared fan-out can only start afterwards. Sequential depth therefore
// degrades from the pure fan-out ideal: 1 + ceil(N/C) waves (N=3 shared
// units), i.e. 4 / 3 / 2 at C=1 / 2 / 4. This is the deterministic
// demonstration that the exclusive-lane barrier erases part of the possible
// gain.
// ---------------------------------------------------------------------------

func TestM9EvidenceScenarioCMixedSharedExclusive(t *testing.T) {
	const shared = 3
	for _, concurrency := range []int{1, 2, 4} {
		concurrency := concurrency
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			taskID := fmt.Sprintf("m9-c-%d", concurrency)
			driver, store := newSchedulerDriver(t, taskID, concurrency)
			ctx := context.Background()
			definitions := append([]Definition{
				{WorkUnitID: "wu-x", Objective: "exclusive write_file-envelope unit", Tools: []string{"write_file"}},
			}, readOnlyDefs("wu", shared)...)
			if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
				t.Fatal(err)
			}
			tracker := newM9Tracker()
			sharedWave := newM9WaveGate(concurrency, shared)
			exclusiveEntered := make(chan struct{}, 1)
			releaseExclusive := make(chan struct{})
			violations := make(chan string, 1)
			run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
				if unit.WorkUnitID == "wu-x" {
					if !tracker.enter(unit.WorkUnitID, concurrency) {
						return m9Violation("bound exceeded", violations)
					}
					defer tracker.leave()
					// The exclusive unit must run with NOTHING else active.
					if tracker.maxActive != 1 {
						return m9Violation("exclusive unit overlapped another unit", violations)
					}
					tracker.noteGeneration(unit.WorkUnitID, 1)
					exclusiveEntered <- struct{}{}
					waitFor(t, releaseExclusive, "exclusive unit release")
					return m9Completed(t, store, unit)
				}
				// Shared unit: may only run AFTER the exclusive settled, and
				// never overlapping it. Store-authoritative check: the
				// exclusive row must already be durably completed
				// (deterministic, independent of channel timing).
				exclusiveUnit, err := store.GetWorkUnit(ctx, unit.TaskID, "wu-x")
				if err != nil {
					return RunResult{}, err
				}
				if exclusiveUnit.Status != "completed" {
					return m9Violation("shared unit overlapped the exclusive unit", violations)
				}
				if !tracker.enter(unit.WorkUnitID, concurrency) {
					return m9Violation("bound exceeded", violations)
				}
				defer tracker.leave()
				tracker.noteGeneration(unit.WorkUnitID, sharedWave.arrive())
				return m9Completed(t, store, unit)
			}
			errCh := runChain(t, driver, ctx, run)
			waitFor(t, exclusiveEntered, "exclusive unit to enter")
			close(releaseExclusive)
			err := waitChain(t, errCh)
			select {
			case reason := <-violations:
				t.Fatalf("scenario C violation: %s", reason)
			default:
			}
			if err != nil {
				t.Fatalf("RunAll(): %v", err)
			}
			snap := tracker.snapshot()
			// Peak simultaneity = the shared wave size (the exclusive wave is
			// exactly 1). With the exclusive-isolation fix, maxActive is
			// min(concurrency, shared): 3 at concurrency=4, 2 at
			// concurrency=2, 1 at concurrency=1. A value of 4 at
			// concurrency=4 would prove the exclusive unit overlapped the
			// shared wave (the #53 corpus defect).
			wantMaxActive := concurrency
			if shared < wantMaxActive {
				wantMaxActive = shared
			}
			if snap.maxActive != wantMaxActive {
				t.Fatalf("mixed maxActive = %d, want %d (exclusive wave 1, shared wave %d; exclusive must never overlap)", snap.maxActive, wantMaxActive, wantMaxActive)
			}
			// Sequential depth with the exclusive-lane barrier: 1 exclusive wave +
			// ceil(shared/concurrency) shared waves.
			wantWaves := 1 + (shared+concurrency-1)/concurrency
			if got := sharedWave.maxGeneration() + 1; got != wantWaves {
				t.Fatalf("total wave depth = %d, want %d (1 exclusive + ceil(%d/%d) shared)", got, wantWaves, shared, concurrency)
			}
			if snap.generations["wu-x"] != 1 {
				t.Fatalf("exclusive generation = %d, want 1 (runs first, alone)", snap.generations["wu-x"])
			}
			for i := 1; i <= shared; i++ {
				unit, err := store.GetWorkUnit(ctx, taskID, fmt.Sprintf("wu-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if unit.Status != "completed" {
					t.Fatalf("wu-%d = %s, want completed", i, unit.Status)
				}
			}
			exclusive, err := store.GetWorkUnit(ctx, taskID, "wu-x")
			if err != nil {
				t.Fatal(err)
			}
			if exclusive.Status != "completed" {
				t.Fatalf("wu-x = %s, want completed", exclusive.Status)
			}
			if err := driver.GateParent(ctx); err != nil {
				t.Fatalf("GateParent(): %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario D — governor-constrained provider lane.
//
// Four read-only units run concurrently at the scheduler level, but every
// provider attempt flows through ONE real governor instance (MaxInFlight=1,
// the same single-attempt route the production composition uses). The corpus
// proves the separation the M9 question demands: scheduler concurrency
// overlaps LOCAL work (all four units reach the local-work barrier before any
// attempt), while the governor serializes PHYSICAL attempts (maxFlight==1),
// admits exactly N attempts (one per unit), never loses or duplicates an
// attempt, tags every attempt with its owning Work Unit id and never lets a
// unit skip the governor.
// ---------------------------------------------------------------------------

func TestM9EvidenceScenarioDGovernorConstrained(t *testing.T) {
	const (
		units        = 4
		concurrency  = 4
		localTicks   = 300
		attemptTicks = 200
	)
	taskID := "m9-d-governor"
	driver, store := newSchedulerDriver(t, taskID, concurrency)
	ctx := context.Background()
	if _, _, err := driver.EnsureDefinitions(ctx, readOnlyDefs("wu", units)); err != nil {
		t.Fatal(err)
	}
	config := governor.DefaultLunaUnlimitedTextConfig("m9-account", "m9-provider", "m9-pool", provider.SafeRouteSafety())
	config.TaskBudget = 64
	config.QueueCapacity = 16
	config.MinimumStartInterval = time.Nanosecond
	config.MaxInFlight = 1
	gov, err := governor.New(config, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gov.Close)

	tracker := newM9Tracker()
	localWave := newM9WaveGate(units, units)
	violations := make(chan string, 1)
	var (
		attemptMu  sync.Mutex
		attempts   = map[string]string{} // unit id -> client request id
		requestIDs = map[string]bool{}
		inFlight   int
		maxFlight  int
	)
	run := func(ctx context.Context, unit state.WorkUnit) (RunResult, error) {
		if !tracker.enter(unit.WorkUnitID, concurrency) {
			return m9Violation("bound exceeded: more than 4 active", violations)
		}
		defer tracker.leave()
		// Local work MAY overlap: all four units must reach this barrier
		// before any of them may attempt the provider lane.
		tracker.noteGeneration(unit.WorkUnitID, localWave.arrive())
		tracker.tick(localTicks)

		// The single provider attempt of this unit goes through the SAME
		// governor instance as every other unit's attempt. There is no second
		// admission path.
		requestID := taskID + "-" + unit.WorkUnitID + "-attempt"
		admission := gov.Admit(ctx, governor.AttemptRequest{
			TaskID:          unit.TaskID,
			WorkUnitID:      unit.WorkUnitID,
			ClientRequestID: requestID,
			ModelPool:       "m9-pool",
			ProviderRequest: provider.Request{Prompt: "m9"},
		})
		if !admission.Admitted() {
			return m9Violation("governor refused a legitimately scheduled attempt: "+string(admission.Code), violations)
		}
		permit := admission.Permit
		if err := permit.Start(); err != nil {
			return RunResult{}, err
		}
		attemptMu.Lock()
		inFlight++
		if inFlight > maxFlight {
			maxFlight = inFlight
		}
		if _, dup := requestIDs[requestID]; dup {
			attemptMu.Unlock()
			return m9Violation("duplicate client request id "+requestID, violations)
		}
		requestIDs[requestID] = true
		attempts[unit.WorkUnitID] = requestID
		attemptMu.Unlock()

		tracker.tick(attemptTicks) // the physical attempt window

		attemptMu.Lock()
		inFlight--
		attemptMu.Unlock()
		finish := permit.Finish(governor.Outcome{
			Class:           governor.OutcomeSuccess,
			UpstreamReached: true,
			DeliveryState:   provider.DeliveryCompleted,
		})
		if finish.Err != nil {
			return RunResult{}, finish.Err
		}
		if finish.AttemptDebited != 1 {
			return m9Violation(fmt.Sprintf("attempt %s debited %d times, want 1", requestID, finish.AttemptDebited), violations)
		}
		tracker.tick(localTicks)
		return m9Completed(t, store, unit)
	}
	if err = waitChain(t, runChain(t, driver, ctx, run)); err != nil {
		t.Fatalf("RunAll(): %v", err)
	}
	select {
	case reason := <-violations:
		t.Fatalf("scenario D violation: %s", reason)
	default:
	}
	snap := tracker.snapshot()
	if localWave.maxGeneration() != 1 {
		t.Fatalf("local wave depth = %d, want 1 (all units overlapped local work)", localWave.maxGeneration())
	}
	if snap.maxActive != units {
		t.Fatalf("maxActive = %d, want %d (scheduler overlapped all units)", snap.maxActive, units)
	}
	attemptMu.Lock()
	defer attemptMu.Unlock()
	if len(attempts) != units {
		t.Fatalf("governor attempts = %d, want %d (exactly one per unit)", len(attempts), units)
	}
	for i := 1; i <= units; i++ {
		id := fmt.Sprintf("wu-%d", i)
		if _, ok := attempts[id]; !ok {
			t.Fatalf("unit %s has no governed attempt", id)
		}
	}
	if maxFlight != 1 {
		t.Fatalf("concurrent physical attempts = %d, want 1 (governor MaxInFlight authoritative)", maxFlight)
	}
	if len(requestIDs) != units {
		t.Fatalf("unique client request ids = %d, want %d", len(requestIDs), units)
	}
	if snap.ticks != units*(localTicks*2+attemptTicks) {
		t.Fatalf("work ticks = %d, want %d (exact accounting)", snap.ticks, units*(localTicks*2+attemptTicks))
	}
	for i := 1; i <= units; i++ {
		unit, err := store.GetWorkUnit(ctx, taskID, fmt.Sprintf("wu-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "completed" {
			t.Fatalf("wu-%d = %s, want completed", i, unit.Status)
		}
	}
	if err := driver.GateParent(ctx); err != nil {
		t.Fatalf("GateParent(): %v", err)
	}
}
