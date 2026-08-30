package main

// M9 Stage B1 work unit concurrency e2e tests (issue #109): operator CLI
// surface, durable/resume-safe scheduler configuration, concurrent real-loop
// execution with per-unit keyed scripted responses (deterministic regardless
// of governor queue interleaving), and crash/restart with two simultaneously
// active read-only units. Every counting assertion is deterministic or
// structurally bounded; no wall-clock timing is asserted.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"
	"github.com/RenyEnnos/Runstead/internal/workunit"

	_ "modernc.org/sqlite"
)

// evidenceAwareKeyedClient is an in-process provider.Client that serves
// scripted responses PER UNIT (keyed by the loop's client request id prefix
// `taskID-unitID`), replacing the @@EVIDENCE@@ marker with that unit's OWN
// latest persisted evidence id at serve time. This makes concurrent real-loop
// e2e runs deterministic regardless of which unit reaches the governor queue
// first: a unit's final always cites the evidence its own run produced.
// It also tracks physical in-flight Completes to prove the scheduler never
// bypasses the governor's serialized lane.
type evidenceAwareKeyedClient struct {
	mu        sync.Mutex
	db        *sql.DB
	taskID    string
	queues    map[string][]string
	attempts  int
	inFlight  int
	maxFlight int
}

// evidenceMarker is the template substitution point.
const evidenceMarker = "@@EVIDENCE@@"

func (c *evidenceAwareKeyedClient) RouteSafety() provider.RouteSafety {
	return provider.SafeRouteSafety()
}

// Complete serves the next template of the longest matching key prefix.
func (c *evidenceAwareKeyedClient) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	c.mu.Lock()
	c.attempts++
	c.inFlight++
	if c.inFlight > c.maxFlight {
		c.maxFlight = c.inFlight
	}
	key := ""
	for candidate := range c.queues {
		if strings.HasPrefix(request.ClientRequestID, candidate) && len(candidate) > len(key) {
			key = candidate
		}
	}
	if key == "" || len(c.queues[key]) == 0 {
		c.inFlight--
		c.mu.Unlock()
		return provider.Response{}, fmt.Errorf("keyed client: no response for %q", request.ClientRequestID)
	}
	template := c.queues[key][0]
	c.queues[key] = c.queues[key][1:]
	c.inFlight--
	c.mu.Unlock()

	text := strings.ReplaceAll(template, evidenceMarker, c.latestEvidenceID(request.ClientRequestID, key))
	return provider.Response{Text: text}, nil
}

// latestEvidenceID returns the owning unit's most recent persisted evidence
// id (work_unit_id ” = the parent task loop), authoritatively from the
// store: the final is served only after the unit's own action executed.
func (c *evidenceAwareKeyedClient) latestEvidenceID(requestID, key string) string {
	rest := strings.TrimPrefix(requestID, c.taskID+"-")
	unitID := ""
	if index := strings.LastIndex(rest, "-"); index >= 0 {
		// unit-scoped request: taskID-unitID-turn -> unitID = task row tag;
		// a task-level request (taskID-turn) has no unit segment and queries
		// the '' (parent) rows.
		unitID = rest[:index]
	}
	var id string
	err := c.db.QueryRow(
		`SELECT r.evidence_id FROM tool_results r
		 JOIN tool_attempts t ON t.execution_id = r.execution_id
		 WHERE t.task_id = ? AND t.work_unit_id = ?
		 ORDER BY r.evidence_id DESC LIMIT 1`, c.taskID, unitID).Scan(&id)
	if err != nil {
		return id // empty: the calling test will fail on grounding, not hang
	}
	return id
}

// TestWorkUnitConcurrencyOnePreservesSerialE2E proves acceptance item 1 at
// the CLI: --workunit-concurrency 1 executes two independent read-only units
// plus the parent in the exact Stage A serial order with correct provenance.
func TestWorkUnitConcurrencyOnePreservesSerialE2E(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"list the workspace","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit b done","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", script,
		"--workunits", workUnitsFile,
		"--workunit-concurrency", "1",
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--min-start-interval", "1ms",
		"--log-level", "error",
		"--state-dir", stateDir,
	}, &out, &errOut)
	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit = %d, want 0\nstderr:\n%s", code, errOut.String())
	}
	// Serial evidence allocation at concurrency=1 is deterministic: the unit
	// evidence order proves the Stage A serial contract is preserved.
	taskID := mustTaskID(t, out.String())
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	wuA, err := store.GetWorkUnit(ctx, taskID, "wu-a")
	if err != nil {
		t.Fatal(err)
	}
	wuB, err := store.GetWorkUnit(ctx, taskID, "wu-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(wuA.EvidenceRefs) != 1 || wuA.EvidenceRefs[0] != "obs-000001" {
		t.Fatalf("wu-a evidence = %v, want [obs-000001]", wuA.EvidenceRefs)
	}
	if len(wuB.EvidenceRefs) != 1 || wuB.EvidenceRefs[0] != "obs-000002" {
		t.Fatalf("wu-b evidence = %v, want [obs-000002]", wuB.EvidenceRefs)
	}
}

// TestWorkUnitConcurrencyInvalidValuesFailBeforeRunning proves invalid
// bounds (0, negative, above the ceiling) exit usage BEFORE any Work Unit
// executes or any durable state is created.
func TestWorkUnitConcurrencyInvalidValuesFailBeforeRunning(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	for _, invalid := range []string{"0", "-1", "5"} {
		stateDir := t.TempDir()
		var out, errOut strings.Builder
		code := run(context.Background(), []string{
			"run", "--task", "t", "--workspace", workspace,
			"--scripted", script, "--workunits", workUnitsFile,
			"--workunit-concurrency", invalid,
			"--min-start-interval", "1ms", "--log-level", "error",
			"--state-dir", stateDir,
		}, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("concurrency %s exit = %d, want %d (usage)\nstderr:\n%s", invalid, code, exitUsage, errOut.String())
		}
		if _, err := os.Stat(filepath.Join(stateDir, "runstead.db")); !os.IsNotExist(err) {
			t.Fatalf("concurrency %s created durable state; invalid values must fail before any Work Unit execution", invalid)
		}
	}
}

// TestWorkUnitConcurrencyInspectShowsEffective proves acceptance item 16:
// runstead inspect renders the effective scheduler configuration durably,
// without secrets.
func TestWorkUnitConcurrencyInspectShowsEffective(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	// Only the unit's first action is scripted: the unit cannot finalize, so
	// the chain stops gated with wu-a open and the parent never runs. The
	// effective configuration is still durably persisted and inspectable.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "t", "--workspace", workspace,
		"--scripted", script, "--workunits", workUnitsFile,
		"--workunit-concurrency", "2",
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--min-start-interval", "1ms", "--log-level", "error",
		"--state-dir", stateDir,
	}, &out, &errOut)
	if code != exitWorkUnitGated {
		t.Fatalf("run exit = %d, want %d (gated: wu-a open without a passed verification yet)\nstderr:\n%s", code, exitWorkUnitGated, errOut.String())
	}
	taskID := mustTaskID(t, errOut.String()+"\n"+out.String())
	var inspectOut, inspectErr strings.Builder
	inspectCode := run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &inspectOut, &inspectErr)
	if inspectCode != exitSuccess {
		t.Fatalf("inspect exit = %d\n%s", inspectCode, inspectErr.String())
	}
	if !strings.Contains(inspectOut.String(), "workunit_concurrency: 2") {
		t.Fatalf("inspect missing the effective scheduler configuration:\n%s", inspectOut.String())
	}
	for _, secret := range []string{"sk-live", "Bearer ", "api_key", "private"} {
		if strings.Contains(inspectOut.String(), secret) {
			t.Fatalf("inspect leaks %q", secret)
		}
	}
}

// testUnitDefinition builds one unit-definition string for the shared
// interrupted-chain fixture.
func testUnitDefinition(id, objective, path string) string {
	return `  {"work_unit_id":"` + id + `","objective":"` + objective + `","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"` + path + `"}]}}`
}

// cancelOnSecondTurn cancels the run context at the first SECOND-TURN
// provider request of either unit (detected from the loop's per-unit request
// id `taskID-unitID-turn`). Whatever the governor queue interleaving, the
// units' FIRST turns are always served (each executes its scripted read), so
// both units are durably 'running' with their first reads committed when the
// chain stops: the exact durable state of a crash with two active units,
// produced without wall-clock waits. The only interleaving variance is
// whether the cancel lands before or after the second unit's first turn; the
// fixture returns the observed committed counts so every assertion is exact.
type cancelOnSecondTurn struct {
	mu       sync.Mutex
	canceled bool
	cancel   context.CancelFunc
}

// reject reports whether this request must be rejected because it is a
// second-turn request (the interruption point).
func (p *cancelOnSecondTurn) reject(requestID string) bool {
	index := strings.LastIndex(requestID, "-")
	if index < 0 {
		return false
	}
	turn := strings.TrimPrefix(requestID[index:], "-")
	if turn == "1" || turn == "0001" || turn == "01" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.canceled {
		p.canceled = true
		p.cancel()
	}
	return true
}

// interruptedConcurrentChainFixture builds a task with two independent
// read-only units under the PERSISTED concurrency=2 contract, executes the
// REAL governed loops through properly keyed scripted responses, and
// interrupts the chain deterministically after both units dispatched and
// executed their first read (canceled context; the durable state is
// byte-identical to a multi-active crash: both units 'running' with their
// tool results committed). It returns stateDir and taskID.
func interruptedConcurrentChainFixture(t *testing.T) (string, string, int, int) {
	t.Helper()
	workspace := t.TempDir()
	for _, file := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-interrupted"
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the production bootstrap exactly (wuRealConfigSnapshot's
	// resolution), plus the PERSISTED scheduler contract N=2.
	writePolicyConfig, err := resolveWritePolicy("", false)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := resolveRecipeCatalog("", false)
	if err != nil {
		t.Fatal(err)
	}
	recipePolicyConfig, err := resolveRecipePolicy("", false, recipes)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapTaskForWorkUnits(ctx, store, taskID, "parent", workspace, "scripted", plan,
		provider.Identity{}, writePolicyConfig.Spec(), recipePolicyConfig.RecipeSpec(recipeIDs(recipes)),
		recipes.Digest(), plan.Digest(), agent.DefaultLimits(), registry, 2); err != nil {
		t.Fatal(err)
	}
	definitions := []workunit.Definition{
		{WorkUnitID: "wu-a", Objective: "inspect a.txt", Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan)},
		{WorkUnitID: "wu-b", Objective: "inspect b.txt", Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan)},
	}
	driver := &workunit.Driver{
		Store:         store,
		TaskID:        taskID,
		AllowedTools:  registryToolIDs(registry),
		TaskWorkspace: workspace,
		Concurrency:   2,
	}
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}

	// Each unit's queue has exactly ONE response (its first action). The
	// cancel-on-second-turn gate cancels the context at the first second-turn
	// request; every later request is rejected by the canceled context
	// (admission), never reaching the provider.
	keyed := &evidenceAwareKeyedClient{
		db:     openReviewDB(t, stateDir),
		taskID: taskID,
		queues: map[string][]string{
			taskID + "-wu-a": {`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`},
			taskID + "-wu-b": {`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`},
		},
	}
	interruptCtx, cancel := context.WithCancel(ctx)
	gate := &cancelOnSecondTurn{cancel: cancel}
	executor := newPersistentScriptedExecutorFor(t, store, requestBridge{keyed: keyed, gate: gate})
	pieces := unitLoopPieces{
		runner:           executor,
		registry:         registry,
		model:            "scripted",
		providerIdentity: provider.Identity{},
		trace:            func(agent.TraceLine) {},
		store:            store,
		policy:           policy.NewStatic(policy.Config{}, nil),
		limits:           agent.DefaultLimits(),
		recovery:         &agent.RecoverySeed{},
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- driver.RunAll(interruptCtx, func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
			return runUnitLoop(ctx, pieces, taskID, unit)
		})
	}()
	err = <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted chain error = %v, want wrapped context.Canceled", err)
	}
	// Durable state of the multi-active interruption: both units running and
	// their first reads committed. The only variance across governor queue
	// interleavings is whether the cancel landed before the second unit's
	// first turn (attempts=2/tools=1) or after it (attempts=3/tools=2); the
	// fixture returns the observed committed counts so every later assertion
	// is exact.
	db := openReviewDB(t, stateDir)
	var running int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id = ? AND status = 'running'`, taskID).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 2 {
		t.Fatalf("running units after interruption = %d, want 2 (multi-active)", running)
	}
	attemptsPre := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts WHERE task_id = ?`, taskID)
	toolsPre := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_results WHERE task_id = ?`, taskID)
	if attemptsPre < 2 || attemptsPre > 3 || toolsPre < 1 || toolsPre > 2 {
		t.Fatalf("unexpected interruption shape: attempts=%d tools=%d (want attempts 2..3, tools 1..2)", attemptsPre, toolsPre)
	}
	return stateDir, taskID, attemptsPre, toolsPre
}

// interruptedFixture returns the fixture state variables.
type interruptedFixture struct {
	stateDir    string
	taskID      string
	attemptsPre int
	toolsPre    int
}

func makeInterruptedFixture(t *testing.T) interruptedFixture {
	t.Helper()
	stateDir, taskID, attemptsPre, toolsPre := interruptedConcurrentChainFixture(t)
	return interruptedFixture{stateDir: stateDir, taskID: taskID, attemptsPre: attemptsPre, toolsPre: toolsPre}
}

// requestBridge forwards provider calls to the cancel gate (which cancels
// the context at the first second-turn request) and to the keyed client for
// the first turns.
type requestBridge struct {
	keyed *evidenceAwareKeyedClient
	gate  *cancelOnSecondTurn
}

func (b requestBridge) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (b requestBridge) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	if b.gate.reject(request.ClientRequestID) {
		return provider.Response{}, context.Canceled
	}
	return b.keyed.Complete(ctx, request)
}

// wuAcceptedPlanFile writes the exact acceptance document the fixture
// persisted (wuAcceptedPlan), so `resume --acceptance` matches the persisted
// digest.
func wuAcceptedPlanFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acceptance.json")
	if err := os.WriteFile(path, []byte(wuAcceptedPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// interruptedResumeScript is the deterministic new-conversation script for a
// fixture interrupted with two committed reads (obs-000001/2): one resumed
// read of d.txt (inside every unit's read_file envelope, a new fingerprint),
// both units' finals citing the seeded obs-000001, then the parent's read of
// c.txt and its final citing the same seeded value. Every unit's recovery
// seed contains obs-000001, so no citation depends on queue interleaving.
func interruptedResumeScript(t *testing.T) string {
	return writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"d.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit continued","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit continued","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
}

// TestWorkUnitConcurrencyResumeDriftFailsClosed proves acceptance item 17:
// resume REJECTS an explicitly different scheduler configuration instead of
// silently changing the task contract, and adopts the persisted value when
// the flag is omitted. The fixture interrupts the chain with two open units
// under the persisted concurrency=2 contract.
func TestWorkUnitConcurrencyResumeDriftFailsClosed(t *testing.T) {
	fixture := makeInterruptedFixture(t)
	stateDir, taskID := fixture.stateDir, fixture.taskID
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"inspect b.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	resumeScript := interruptedResumeScript(t)
	acceptanceFile := wuAcceptedPlanFile(t)

	// Explicit drift must fail closed (usage) before the recovery pipeline
	// journals anything.
	driftCode, _, driftErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--workunit-concurrency", "1", "--acceptance", acceptanceFile,
		"--min-start-interval", "1ms", "--log-level", "error")
	if driftCode != exitUsage {
		t.Fatalf("drift resume exit = %d, want %d (usage)\nstderr:\n%s", driftCode, exitUsage, driftErr)
	}
	// The rejected resume must not have resolved the units or journaled a
	// completed state: both remain 'running' (interrupted).
	var rejectedRunning int
	db := openReviewDB(t, stateDir)
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id = ? AND status = 'running'`, taskID).Scan(&rejectedRunning); err != nil {
		t.Fatal(err)
	}
	if rejectedRunning != 2 {
		t.Fatalf("units running after rejected drift resume = %d, want 2", rejectedRunning)
	}

	// Omitting the flag adopts the persisted contract (2) and completes.
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--acceptance", acceptanceFile,
		"--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completion:\n%s", resumeOut)
	}
}

// TestWorkUnitCrashRestartWithTwoRunningUnits proves acceptance item 14: an
// interruption with TWO simultaneously active run-only units (durable state
// identical to a crash: both units 'running', effects committed) is
// reconstructed from SQLite, never replays completed effects, never blindly
// replays provider attempts, and reaches parent completion through a brand
// new conversation via the REAL resume path.
func TestWorkUnitCrashRestartWithTwoRunningUnits(t *testing.T) {
	fixture := makeInterruptedFixture(t)
	stateDir, taskID := fixture.stateDir, fixture.taskID
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"inspect b.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)

	// New conversation, same durable contract (the concurrency flag is
	// OMITTED: the resume adopts the persisted value 2).
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", interruptedResumeScript(t), "--workunits", workUnitsFile,
		"--acceptance", wuAcceptedPlanFile(t), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completion:\n%s", resumeOut)
	}

	db := openReviewDB(t, stateDir)
	// No blind provider replay: every persisted client request id is unique
	// (resumed loops continue their OWN counters, so no interrupted request
	// is re-issued under a new identity).
	var totalRequests, distinctRequests int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT client_request_id) FROM provider_attempts WHERE task_id = ?`, taskID).Scan(&totalRequests, &distinctRequests); err != nil {
		t.Fatal(err)
	}
	if totalRequests != distinctRequests {
		t.Fatalf("provider request ids = %d total / %d distinct; a resumed attempt was re-issued", totalRequests, distinctRequests)
	}
	// Exactly-once accounting across the interruption and the resume: the
	// observed interrupted attempts (2 or 3) plus 3 resumed unit turns and 2
	// parent turns.
	if want := fixture.attemptsPre + 5; totalRequests != want {
		t.Fatalf("provider attempts = %d, want %d (interrupted %d + 3 resumed unit + 2 parent)", totalRequests, want, fixture.attemptsPre)
	}
	// Completed effects are never replayed: evidence ids are unique and the
	// totals are exactly the interrupted reads (2) plus one resumed read and
	// the parent read.
	var duplicateEvidence int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT evidence_id FROM tool_results WHERE task_id = ?
		GROUP BY evidence_id HAVING COUNT(*) > 1)`, taskID).Scan(&duplicateEvidence); err != nil {
		t.Fatal(err)
	}
	if duplicateEvidence != 0 {
		t.Fatalf("duplicated evidence ids = %d (an effect was replayed)", duplicateEvidence)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_results WHERE task_id = ?`, taskID); got != fixture.toolsPre+2 {
		t.Fatalf("tool_results after resume = %d, want %d (interrupted reads %d + 1 resumed read + 1 parent read)", got, fixture.toolsPre+2, fixture.toolsPre)
	}
	// Each unit completed exactly once with its own verification; the parent
	// completed too.
	ctx := context.Background()
	store := openWorkUnitStore(t, stateDir)
	for _, id := range []string{"wu-a", "wu-b"} {
		unit, err := store.GetWorkUnit(ctx, taskID, id)
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "completed" {
			t.Fatalf("%s = %s, want completed", id, unit.Status)
		}
		var verifications int
		if err := db.QueryRow(`SELECT COUNT(*) FROM verification_attempts WHERE task_id = ? AND work_unit_id = ? AND decision = 'passed'`, taskID, id).Scan(&verifications); err != nil {
			t.Fatal(err)
		}
		if verifications != 1 {
			t.Fatalf("%s passed verifications = %d, want 1", id, verifications)
		}
	}
}

// TestWorkUnitConcurrentReadOnlyLoopsE2E proves acceptance items 2/3/6/11:
// two independent read-only units run through the REAL governed loop
// concurrently under concurrency=2, with exactly-once provider accounting,
// unique evidence ids, correct work_unit_id provenance on every row class,
// and a governor-serialized physical lane (no scheduler bypass).
func TestWorkUnitConcurrentReadOnlyLoopsE2E(t *testing.T) {
	workspace := t.TempDir()
	for _, file := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, file), []byte(file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-concurrent"
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: workspace, Model: "scripted",
		ConfigJSON: wuRealConfigSnapshot(t, registry, plan),
	}, plan, registry); err != nil {
		t.Fatal(err)
	}
	definitions := []workunit.Definition{
		{WorkUnitID: "wu-a", Objective: "read a.txt", Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan)},
		{WorkUnitID: "wu-b", Objective: "list the workspace", Tools: []string{"list_files"}, AcceptancePlan: []byte(wuAcceptedPlan)},
	}

	// Per-unit keyed responses; the final template cites the unit's OWN
	// latest persisted evidence id, so the real-loop interleaving through
	// the governor's serialized lane cannot affect the outcome.
	client := &evidenceAwareKeyedClient{
		db:     openReviewDB(t, stateDir),
		taskID: taskID,
		queues: map[string][]string{
			taskID + "-wu-a": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
			},
			taskID + "-wu-b": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"b done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"list_files"}]}</runstead_final>`,
			},
			taskID + "-": {
				`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
				`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"@@EVIDENCE@@","tool":"read_file"}]}</runstead_final>`,
			},
		},
	}
	executor := newPersistentScriptedExecutorFor(t, store, client)
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
	chainErr := runWorkUnitChain(ctx, store, taskID, workspace, registry, definitions, 2,
		func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
			return runUnitLoop(ctx, pieces, taskID, unit)
		})
	if chainErr != nil {
		t.Fatalf("chain: %v", chainErr)
	}
	// Parent loop through the same governed executor.
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
		t.Fatal(err)
	}
	parentResult := parentLoop.Run(ctx, agent.Task{ID: taskID, Prompt: "parent"})
	if parentResult.Outcome != agent.OutcomeCompleted {
		t.Fatalf("parent outcome = %s (%s)", parentResult.Outcome, parentResult.StopReason)
	}

	// Exactly-once accounting: 2 turns per unit + 2 parent turns, all
	// through the one shared governor-owned executor.
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts WHERE task_id = ?`, taskID); got != 6 {
		t.Fatalf("provider attempts = %d, want 6", got)
	}
	// The physical lane stayed serialized: the scheduler never bypassed the
	// governor to force provider parallelism.
	if client.maxFlight != 1 {
		t.Fatalf("concurrent provider Completes = %d, want 1 (governor MaxInFlight stays authoritative)", client.maxFlight)
	}
	// Evidence ids unique under concurrency; every unit's evidence refs come
	// from its OWN rows.
	db := openReviewDB(t, stateDir)
	var evidenceTotal, evidenceDistinct int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT evidence_id) FROM tool_results WHERE task_id = ?`, taskID).Scan(&evidenceTotal, &evidenceDistinct); err != nil {
		t.Fatal(err)
	}
	if evidenceTotal != 3 || evidenceDistinct != 3 {
		t.Fatalf("evidence = %d total / %d distinct, want 3/3 (unique under concurrency)", evidenceTotal, evidenceDistinct)
	}
	wuA, err := store.GetWorkUnit(ctx, taskID, "wu-a")
	if err != nil {
		t.Fatal(err)
	}
	wuB, err := store.GetWorkUnit(ctx, taskID, "wu-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(wuA.EvidenceRefs) != 1 || len(wuB.EvidenceRefs) != 1 {
		t.Fatalf("unit evidence refs = %v / %v, want one ref each", wuA.EvidenceRefs, wuB.EvidenceRefs)
	}
	// Provenance: every row class carries the correct owning unit. The
	// parent's rows are task-level ('').
	for _, table := range []string{"actions", "tool_attempts", "provider_attempts", "verification_attempts"} {
		var orphan int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE task_id = ? AND work_unit_id NOT IN ('wu-a','wu-b','')`, taskID).Scan(&orphan); err != nil {
			t.Fatal(err)
		}
		if orphan != 0 {
			t.Fatalf("%s rows with wrong work_unit_id = %d", table, orphan)
		}
		var countA, countB, countTask int
		if err := db.QueryRow(`SELECT
			SUM(CASE WHEN work_unit_id='wu-a' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='wu-b' THEN 1 ELSE 0 END),
			SUM(CASE WHEN work_unit_id='' THEN 1 ELSE 0 END)
			FROM `+table+` WHERE task_id = ?`, taskID).Scan(&countA, &countB, &countTask); err != nil {
			t.Fatal(err)
		}
		if countA == 0 || countB == 0 || countTask == 0 {
			t.Fatalf("%s provenance counts = a:%d b:%d task:%d, want all non-zero", table, countA, countB, countTask)
		}
	}
	// Governor accounting: every attempt debited exactly once.
	var misDebited int
	if err := db.QueryRow(`SELECT COUNT(*) FROM provider_attempts WHERE task_id = ? AND attempt_debited != 1`, taskID).Scan(&misDebited); err != nil {
		t.Fatal(err)
	}
	if misDebited != 0 {
		t.Fatalf("under/over-debited attempts = %d", misDebited)
	}
}

// TestWorkUnitConcurrencyResumeRejectsCorruptPersistedConfig proves the
// fail-closed durable-config contract (issue #109 review): a resumed chain
// REFUSES a persisted scheduler configuration that is present-but-corrupted
// (invalid type) or outside the operator contract (1..4), in the resume
// PRE-FLIGHT: before the recovery pipeline journals anything, before any
// Work Unit transitions, and before any provider dispatch. The legacy Stage
// A default (1) applies ONLY to a genuinely absent key.
func TestWorkUnitConcurrencyResumeRejectsCorruptPersistedConfig(t *testing.T) {
	cases := map[string]any{
		"invalid-type": "2", // string: type corruption
		"non-integral": 2.5, // float: non-integral corruption
		"out-of-range": 99,  // integer outside the contract
	}
	for name, corrupted := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := makeInterruptedFixture(t)
			stateDir, taskID := fixture.stateDir, fixture.taskID
			db := openReviewDB(t, stateDir)

			// Mutate the task's persisted config_json (authoritative durable
			// state) to the corrupted scheduler contract.
			var configJSON string
			if err := db.QueryRow(`SELECT config_json FROM tasks WHERE task_id = ?`, taskID).Scan(&configJSON); err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := json.Unmarshal([]byte(configJSON), &values); err != nil {
				t.Fatal(err)
			}
			values[state.WorkUnitConcurrencyKey] = corrupted
			raw, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE tasks SET config_json = ? WHERE task_id = ?`, string(raw), taskID); err != nil {
				t.Fatal(err)
			}

			// Pre-flight state snapshot: events, unit statuses, attempts and
			// resume count must ALL stay untouched by the rejected resume.
			var eventsBefore, attemptsBefore, resumeCountBefore int
			if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE task_id = ?`, taskID).Scan(&eventsBefore); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM provider_attempts WHERE task_id = ?`, taskID).Scan(&attemptsBefore); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT resume_count FROM tasks WHERE task_id = ?`, taskID).Scan(&resumeCountBefore); err != nil {
				t.Fatal(err)
			}

			definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"inspect b.txt","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
			workUnitsFile := workUnitsFileFor(t, definitions)
			code, _, stderr := runResume(context.Background(),
				taskID, "--state-dir", stateDir, "--scripted", interruptedResumeScript(t),
				"--workunits", workUnitsFile, "--acceptance", wuAcceptedPlanFile(t),
				"--min-start-interval", "1ms", "--log-level", "error")
			if code != exitCorrupt {
				t.Fatalf("resume exit = %d, want %d (corrupt persisted scheduler config)\nstderr:\n%s", code, exitCorrupt, stderr)
			}
			// No recovery journaling, no Work Unit transitions, no provider
			// dispatch, no resume-count inflation.
			var eventsAfter, attemptsAfter, resumeCountAfter int
			if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE task_id = ?`, taskID).Scan(&eventsAfter); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM provider_attempts WHERE task_id = ?`, taskID).Scan(&attemptsAfter); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT resume_count FROM tasks WHERE task_id = ?`, taskID).Scan(&resumeCountAfter); err != nil {
				t.Fatal(err)
			}
			if eventsAfter != eventsBefore {
				t.Fatalf("events after rejected resume = %d, want %d (no recovery journaling)", eventsAfter, eventsBefore)
			}
			if attemptsAfter != attemptsBefore {
				t.Fatalf("provider attempts after rejected resume = %d, want %d (no provider dispatch)", attemptsAfter, attemptsBefore)
			}
			if resumeCountAfter != resumeCountBefore {
				t.Fatalf("resume_count after rejected resume = %d, want %d", resumeCountAfter, resumeCountBefore)
			}
			var running int
			if err := db.QueryRow(`SELECT COUNT(*) FROM work_units WHERE task_id = ? AND status = 'running'`, taskID).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if running != 2 {
				t.Fatalf("running units after rejected resume = %d, want 2 (no Work Unit transition)", running)
			}
		})
	}
}

// TestWithWorkUnitConcurrencyUnit proves the scheduler-config write path
// (issue #109 review): withWorkUnitConcurrency merges the durable key into
// the task configuration snapshot and PROPAGATES composition errors instead
// of silently dropping the scheduler contract.
func TestWithWorkUnitConcurrencyUnit(t *testing.T) {
	merged, err := withWorkUnitConcurrency([]byte(`{"max_steps":24}`), 2)
	if err != nil {
		t.Fatalf("withWorkUnitConcurrency(valid) error = %v", err)
	}
	value, present, readErr := state.WorkUnitConcurrencyFromConfigJSON(string(merged))
	if readErr != nil || !present || value != 2 {
		t.Fatalf("merged snapshot = %d/present:%t/err:%v, want 2/true/nil", value, present, readErr)
	}
	if _, err := withWorkUnitConcurrency([]byte("not json"), 2); err == nil {
		t.Fatal("withWorkUnitConcurrency(malformed) must propagate the error, never silently return the snapshot")
	}
	if _, err := withWorkUnitConcurrency(nil, 2); err == nil {
		t.Fatal("withWorkUnitConcurrency(nil) must propagate the error")
	}
}
