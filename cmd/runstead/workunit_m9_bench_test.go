package main

// M9 evidence-gate benchmark harness (issue #53): wall-clock benefit
// evaluation of the Stage B1 bounded scheduler at the REAL composition-root
// level (real driver + real SQLite + real governor-owned executor + real
// agent loops + real tools), with a scripted provider whose per-attempt
// latency is configurable.
//
// IMPORTANT: this file is a REPRODUCTION HARNESS, not CI correctness
// assertions. It never runs under `go test` (benchmarks require -bench), its
// numbers are environment-dependent wall-clock measurements, and nothing here
// asserts a timing threshold. The deterministic correctness proofs live in
// internal/workunit/m9_evidence_test.go; this file only measures the benefit
// question and reports min/median/max over repetitions, separating:
//
//   - scheduler concurrency (observed maxActive / waves);
//   - local work + scheduler overhead (total minus provider time);
//   - provider-attempt time (sum of measured per-attempt durations; the real
//     governor serializes attempts, so the sum is the serialized provider
//     component);
//   - governor-imposed serialization (observed maxFlight == 1 at every cell).
//
// Results are emitted as one `M9CELL <scenario> <concurrency> ...` line per
// cell (min/median/max of total task duration over b.N repetitions plus the
// provider/local decomposition and the structural counters), which
// experiments/m9-workunit-concurrency/run.sh captures for the versioned M9
// report.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
	"github.com/RenyEnnos/Runstead/internal/workunit"
)

// m9BenchProvider wraps the keyed scripted client with a configurable
// per-attempt latency and the physical-accounting counters of the harness:
// attempts, in-flight maximum (must stay 1: the real governor serializes the
// lane) and the summed provider time (the serialized provider component).
type m9BenchProvider struct {
	keyed *evidenceAwareKeyedClient
	delay time.Duration

	mu          sync.Mutex
	attempts    int
	inFlight    int
	maxFlight   int
	providerSum time.Duration
}

func (p *m9BenchProvider) RouteSafety() provider.RouteSafety {
	return provider.SafeRouteSafety()
}

func (p *m9BenchProvider) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.attempts++
	p.inFlight++
	if p.inFlight > p.maxFlight {
		p.maxFlight = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	start := time.Now()
	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
	}
	response, err := p.keyed.Complete(ctx, request)
	p.mu.Lock()
	p.providerSum += time.Since(start)
	p.mu.Unlock()
	return response, err
}

// m9Workload describes one benchmark scenario cell.
type m9Workload struct {
	name        string
	concurrency int
	units       int  // number of read-only units
	chain       bool // chain dependencies between the read-only units
	exclusive   bool // add one exclusive (omitted-envelope) unit
	unitTurns   int  // provider+tool turns per unit before its final
	provider    time.Duration
}

// m9CellStats is the reproducible measurement of one scenario run.
type m9CellStats struct {
	total     time.Duration
	provider  time.Duration
	attempts  int
	maxFlight int
	maxActive int
}

var m9TaskSeq atomic.Int64

// m9definitions builds the scenario definitions.
func m9definitions(wl m9Workload) []workunit.Definition {
	var defs []workunit.Definition
	units := wl.units
	if wl.exclusive {
		defs = append(defs, workunit.Definition{
			WorkUnitID: "wu-x", Objective: "exclusive unit (omitted envelope)",
			AcceptancePlan: []byte(wuAcceptedPlan),
		})
	}
	for i := 1; i <= units; i++ {
		def := workunit.Definition{
			WorkUnitID:     fmt.Sprintf("wu-%d", i),
			Objective:      fmt.Sprintf("inspect file %d", i),
			Tools:          []string{"read_file"},
			AcceptancePlan: []byte(wuAcceptedPlan),
		}
		if wl.chain && i > 1 {
			def.Dependencies = []string{fmt.Sprintf("wu-%d", i-1)}
		}
		defs = append(defs, def)
	}
	return defs
}

// m9actionTurn renders the j-th read_file action of unit i (distinct files
// per turn so the repeat guard never conflates turns).
func m9actionTurn(turn int) string {
	file := fmt.Sprintf("%c.txt", 'a'+(turn%8))
	return `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"` + file + `"}}</runstead_action>`
}

// runM9Workload executes one complete task (chain + parent loop) for the
// workload and returns the measured cell stats.
func runM9Workload(b *testing.B, wl m9Workload) m9CellStats {
	b.Helper()
	workspace := b.TempDir()
	for i := 0; i < 8; i++ {
		file := fmt.Sprintf("%c.txt", 'a'+i)
		if err := os.WriteFile(filepath.Join(workspace, file), []byte(file+"\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		b.Fatal(err)
	}
	stateDir := b.TempDir()
	store := m9BenchStore(b, stateDir)
	ctx := context.Background()
	taskID := fmt.Sprintf("m9-bench-%d", m9TaskSeq.Add(1))
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		b.Fatal(err)
	}
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: workspace, Model: "scripted",
		ConfigJSON: m9BenchSnapshot(b, registry, plan),
	}, plan, registry); err != nil {
		b.Fatal(err)
	}

	// Per-unit keyed queues: unitTurns actions + one final per unit; parent
	// turns for the parent loop.
	queues := make(map[string][]string, wl.units+2)
	unitIDs := make([]string, 0, wl.units+1)
	if wl.exclusive {
		unitIDs = append(unitIDs, "wu-x")
	}
	for i := 1; i <= wl.units; i++ {
		unitIDs = append(unitIDs, fmt.Sprintf("wu-%d", i))
	}
	for _, unitID := range unitIDs {
		queue := make([]string, 0, wl.unitTurns+1)
		for turn := 1; turn <= wl.unitTurns; turn++ {
			queue = append(queue, m9actionTurn(turn))
		}
		queue = append(queue, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`)
		queues[taskID+"-"+unitID] = queue
	}
	queues[taskID+"-"] = []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
	}
	keyed := &evidenceAwareKeyedClient{db: m9BenchReviewDB(b, stateDir), taskID: taskID, queues: queues}
	providerClient := &m9BenchProvider{keyed: keyed, delay: wl.provider}
	executor := m9BenchExecutor(b, store, providerClient)
	emptySeed := &agent.RecoverySeed{}
	pieces := unitLoopPieces{
		runner:           executor,
		registry:         registry,
		model:            "scripted",
		providerIdentity: provider.Identity{},
		trace:            func(agent.TraceLine) {},
		store:            store,
		policy:           policy.NewStatic(policy.Config{}, nil),
		limits:           agent.DefaultLimits(),
		recovery:         emptySeed,
	}
	stats := m9CellStats{}
	var statsMu sync.Mutex
	var activeUnits int
	started := time.Now()
	chainErr := runWorkUnitChain(ctx, store, taskID, workspace, registry, m9definitions(wl), wl.concurrency,
		func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
			// Observe the scheduler's simultaneous-active count: unit loops
			// in flight right now (dispatched and not yet settled).
			statsMu.Lock()
			activeUnits++
			if activeUnits > stats.maxActive {
				stats.maxActive = activeUnits
			}
			statsMu.Unlock()
			defer func() {
				statsMu.Lock()
				activeUnits--
				statsMu.Unlock()
			}()
			return runUnitLoop(ctx, pieces, taskID, unit)
		})
	if chainErr != nil {
		b.Fatalf("chain: %v", chainErr)
	}
	parentLoop, err := agent.NewLoop(agent.Config{
		Runner:               executor,
		Registry:             registry,
		Limits:               agent.DefaultLimits(),
		Model:                "scripted",
		ProviderIdentity:     provider.Identity{},
		Trace:                func(agent.TraceLine) {},
		State:                store,
		Policy:               policy.NewStatic(policy.Config{}, nil),
		Verifier:             verifier.New(registry, plan),
		AcceptancePlanDigest: plan.Digest(),
		Recovery:             emptySeed,
	})
	if err != nil {
		b.Fatal(err)
	}
	parentResult := parentLoop.Run(ctx, agent.Task{ID: taskID, Prompt: "parent"})
	if parentResult.Outcome != agent.OutcomeCompleted {
		b.Fatalf("parent outcome = %s (%s)", parentResult.Outcome, parentResult.StopReason)
	}
	stats.total = time.Since(started)
	providerClient.mu.Lock()
	stats.provider = providerClient.providerSum
	stats.attempts = providerClient.attempts
	stats.maxFlight = providerClient.maxFlight
	providerClient.mu.Unlock()
	return stats
}

// m9BenchmarScenarioCell runs one (scenario, concurrency) cell with b.N
// repetitions and logs the canonical M9CELL evidence line.
func m9BenchmarScenarioCell(b *testing.B, wl m9Workload) {
	totals := make([]float64, 0, b.N)
	providers := make([]float64, 0, b.N)
	locals := make([]float64, 0, b.N)
	var attempts, maxFlight, maxActive int
	for i := 0; i < b.N; i++ {
		stats := runM9Workload(b, wl)
		totals = append(totals, float64(stats.total.Microseconds()))
		providers = append(providers, float64(stats.provider.Microseconds()))
		locals = append(locals, float64((stats.total - stats.provider).Microseconds()))
		attempts = stats.attempts
		if stats.maxFlight > maxFlight {
			maxFlight = stats.maxFlight
		}
		if stats.maxActive > maxActive {
			maxActive = stats.maxActive
		}
	}
	min, med, max := m9Summary(totals)
	_, providerMed, _ := m9Summary(providers)
	_, localMed, _ := m9Summary(locals)
	b.ReportMetric(med, "total-median-us")
	b.ReportMetric(providerMed, "provider-median-us")
	b.ReportMetric(localMed, "local-median-us")
	b.Logf("M9CELL %s concurrency=%d reps=%d total_us min=%.0f med=%.0f max=%.0f provider_us med=%.0f local_us med=%.0f attempts=%d maxFlight=%d maxActive=%d",
		wl.name, wl.concurrency, b.N, min, med, max, providerMed, localMed, attempts, maxFlight, maxActive)
}

// m9Summary returns min, median, max of the samples.
func m9Summary(samples []float64) (min, median, max float64) {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	min, max = sorted[0], sorted[len(sorted)-1]
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		median = sorted[middle]
	} else {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	return min, median, max
}

// Scenario A — read-only fan-out at three bounds and two provider latencies:
// the benefit of scheduler concurrency depends on how much of the task time
// the governor-serialized provider lane occupies. At provider=5ms the local
// work dominates (overlap has room to help); at provider=50ms the serialized
// provider lane dominates (overlap can only hide the local remainder).
func BenchmarkM9ScenarioFanOut(b *testing.B) {
	for _, delay := range []time.Duration{5 * time.Millisecond, 50 * time.Millisecond} {
		for _, c := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("provider=%dms/concurrency=%d", delay.Milliseconds(), c), func(b *testing.B) {
				m9BenchmarScenarioCell(b, m9Workload{
					name: "fanout", concurrency: c, units: 4,
					unitTurns: 2, provider: delay,
				})
			})
		}
	}
}

// Scenario B — dependent chain: raising the bound must not invent parallelism.
func BenchmarkM9ScenarioDependencyChain(b *testing.B) {
	for _, c := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			m9BenchmarScenarioCell(b, m9Workload{
				name: "chain", concurrency: c, units: 4, chain: true,
				unitTurns: 2, provider: 15 * time.Millisecond,
			})
		})
	}
}

// Scenario C — mixed shared/exclusive: the exclusive-LANE barrier erases
// part of the possible gain. NOTE ON FIDELITY: in this wall-clock harness the
// exclusive unit (wu-x) has an OMITTED (nil) tool envelope - it is exclusive
// because an omitted envelope means the task default surface - and every
// scripted unit turn, including wu-x's, executes read_file. The harness
// therefore measures the cost of the scheduler's exclusive-lane serialization
// itself, NOT a write/effectful workload (the driver-level deterministic
// corpus declares a write_file envelope; no write path is executed anywhere
// in this benchmark).
func BenchmarkM9ScenarioMixedExclusive(b *testing.B) {
	for _, c := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			m9BenchmarScenarioCell(b, m9Workload{
				name: "mixed", concurrency: c, units: 3, exclusive: true,
				unitTurns: 2, provider: 15 * time.Millisecond,
			})
		})
	}
}

// Scenario D — governor-constrained: provider-bound workload where the
// real governor serializes every physical attempt (maxFlight must stay 1).
func BenchmarkM9ScenarioGovernorConstrained(b *testing.B) {
	for _, c := range []int{1, 2, 4} {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			m9BenchmarScenarioCell(b, m9Workload{
				name: "governed", concurrency: c, units: 4,
				unitTurns: 2, provider: 50 * time.Millisecond,
			})
		})
	}
}

// ---------------------------------------------------------------------------
// testing.B-typed local helpers (the shared helpers are *testing.T-typed;
// benchmarks must not depend on them).
// ---------------------------------------------------------------------------

func m9BenchStore(b *testing.B, stateDir string) *state.Store {
	b.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { store.Close() })
	return store
}

func m9BenchSnapshot(b *testing.B, registry *tools.Registry, plan *verifier.Plan) []byte {
	b.Helper()
	writePolicyConfig, err := resolveWritePolicy("", false)
	if err != nil {
		b.Fatal(err)
	}
	recipes, err := resolveRecipeCatalog("", false)
	if err != nil {
		b.Fatal(err)
	}
	recipePolicyConfig, err := resolveRecipePolicy("", false, recipes)
	if err != nil {
		b.Fatal(err)
	}
	return agent.ConfigSnapshot(registry, "scripted", provider.Identity{},
		writePolicyConfig.Spec(),
		recipePolicyConfig.RecipeSpec(recipeIDs(recipes)),
		recipes.Digest(), plan.Digest(), agent.DefaultLimits())
}

func m9BenchReviewDB(b *testing.B, stateDir string) *sql.DB {
	b.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func m9BenchExecutor(b *testing.B, store *state.Store, client provider.Client) agent.AttemptRunner {
	b.Helper()
	config := governor.DefaultInstantConfig("m9-bench-policy", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Persistence: store})
	if err != nil {
		b.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		b.Fatal(err)
	}
	return executor
}
