package main

// Issue #106 review gates: real-loop capability enforcement, full WorkUnitID
// provenance, #51 context reaching the resumed session, the authoritative
// parent completion gate and the configured-provider config continuity. These
// tests exercise the trust boundaries the blocking review demanded; none of
// them fabricate state with manual SQL.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
	"github.com/RenyEnnos/Runstead/internal/verifier"

	_ "modernc.org/sqlite"
)

const wuAcceptedPlan = `{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}`

func openWorkUnitStore(t *testing.T, stateDir string) *state.Store {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func workUnitRowCount(t *testing.T, stateDir, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return count
}

// TestWorkUnitCapabilityEnforcedAtRuntime is the real-loop negative gate: a
// Work Unit declared with ONLY read_file cannot execute write_file or
// run_recipe. The proposals are rejected deterministically (protocol
// correction, unknown tool for this unit) BEFORE any action record, policy
// decision, tool attempt or effect.
func TestWorkUnitCapabilityEnforcedAtRuntime(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-ro","objective":"read only unit","tools":["read_file"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	// The model first proposes a write and a recipe (both outside the unit's
	// envelope), then performs the allowed read and finishes grounded.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"created.txt","content":"owned","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"read only","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", script,
		"--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--max-corrections", "4",
		"--min-start-interval", "1ms",
		"--state-dir", stateDir,
		"--log-level", "error",
	}, &out, &errOut)
	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", code, errOut.String(), out.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("write_file effect happened: created.txt exists")
	}

	// Exactly ONE tool attempt exists, for read_file only: no write_file and
	// no run_recipe ever reached execution.
	if got := workUnitRowCount(t, stateDir,
		`SELECT COUNT(*) FROM tool_attempts WHERE tool IN ('write_file','run_recipe')`); got != 0 {
		t.Fatalf("forbidden tool attempts = %d, want 0", got)
	}
	// The single accepted action of the UNIT is the allowed read, tagged
	// with the unit; the parent's own read is the second read_file attempt
	// (obs-000002).
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var actionTool, actionUnit string
	if err := db.QueryRow(`SELECT tool, work_unit_id FROM actions ORDER BY created_at`).Scan(&actionTool, &actionUnit); err != nil {
		t.Fatal(err)
	}
	if actionTool != "read_file" || actionUnit != "wu-ro" {
		t.Fatalf("action = %s/%s, want read_file/wu-ro (capability attempt never recorded)", actionTool, actionUnit)
	}
	// The two rejected proposals still consumed provider turns (deterministic
	// rejections), but nothing else happened.
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts`); got != 6 {
		t.Fatalf("provider_attempts = %d, want 6 (2 rejected proposals + read + final + parent read + parent final)", got)
	}
	// The parent's read is the ONLY second attempt: obs-000002.
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts WHERE tool = 'read_file'`); got != 2 {
		t.Fatalf("read_file tool attempts = %d, want 2 (unit + parent)", got)
	}
}

// TestWorkUnitResumedSessionReceivesReconstructedContext proves the #51
// recovery context reaches the REAL model-facing input of the resumed unit
// session: the compiled context is the initial prompt of the new conversation
// and carries the reconstructed work unit state (ready, never the stale
// pre-reset running) and the prior evidence ids. Nothing is fabricated: the
// loop runs through the real governor-owned executor and the fake provider
// records every request it receives.
func TestWorkUnitResumedSessionReceivesReconstructedContext(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-ctx-task"
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent task", Workspace: workspace, Model: "scripted",
		ConfigJSON: agent.ConfigSnapshot(registry, "scripted", provider.Identity{}, "", "", "", plan.Digest(), agent.DefaultLimits()),
	}, plan, registry); err != nil {
		t.Fatal(err)
	}
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-a", Objective: "inspect a.txt",
		Tools: []string{"read_file"}, AcceptancePlan: []byte(wuAcceptedPlan), AcceptanceDigest: plan.Digest(),
	})
	// wu-a completed with PRODUCTION-style evidence rows (threaded work_unit_id).
	actionID, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: taskID, WorkUnitID: "wu-a", Tool: "read_file", Arguments: []byte(`{"path":"a.txt"}`),
		Fingerprint: "fp-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: taskID, WorkUnitID: "wu-a", ActionID: actionID, Tool: "read_file", Arguments: []byte(`{"path":"a.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteToolAttempt(ctx, state.ToolAttemptCompleted{
		TaskID: taskID, ExecutionID: executionID, Status: "completed", EvidenceID: "obs-000001",
		DurationNanos: 1, Observation: tools.Observation{ID: "obs-000001", Tool: "read_file", Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-a", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-a", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-a", "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	// wu-b depends on wu-a and was interrupted mid-run: persisted 'running'.
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-b", Objective: "finish the scan",
		Dependencies: []string{"wu-a"}, Tools: []string{"read_file", "list_files"},
		AcceptancePlan: []byte(wuAcceptedPlan), AcceptanceDigest: plan.Digest(),
	})
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-b", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-b", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}

	resumed, err := recovery.Resume(ctx, store, recovery.Options{
		TaskID:             taskID,
		WorkspaceSignature: agent.WorkspaceSignature,
	})
	if err != nil {
		t.Fatalf("recovery.Resume: %v", err)
	}
	if resumed.Decision != recovery.DecisionContinue || resumed.Seed == nil {
		t.Fatalf("decision = %s, want continue with seed", resumed.Decision)
	}
	// The compiled context reflects the POST-reset state: wu-b is ready, and
	// the prior evidence id is present.
	for _, want := range []string{"work units: wu-a(completed), wu-b(ready)", "obs-000001"} {
		if !strings.Contains(resumed.Seed.Context, want) {
			t.Fatalf("reconstructed context missing %q:\n%s", want, resumed.Seed.Context)
		}
	}
	if strings.Contains(resumed.Seed.Context, "wu-b(running)") {
		t.Fatalf("context projects stale pre-reset state (running) after Resume:\n%s", resumed.Seed.Context)
	}

	// The resumed unit session: real governor executor + fake provider, same
	// as the production composition root. The evidence counter continues
	// from the persisted maximum.
	resumedRegistry, err := tools.NewRegistry(tools.Options{Workspace: workspace, NextEvidenceSequence: resumed.NextEvidenceSequence})
	if err != nil {
		t.Fatal(err)
	}
	unitRegistry, err := resumedRegistry.Restricted([]string{"read_file", "list_files"}, "")
	if err != nil {
		t.Fatal(err)
	}
	config := governor.DefaultInstantConfig("wu-ctx-policy", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(
		provider.Response{Text: `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`},
		provider.Response{Text: `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"continued","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`},
	)
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := agent.NewLoop(agent.Config{
		Runner:               executor,
		Registry:             unitRegistry,
		Limits:               agent.DefaultLimits(),
		Model:                "scripted",
		ProviderIdentity:     provider.Identity{},
		Trace:                func(agent.TraceLine) {},
		State:                store,
		Policy:               policy.NewStatic(policy.Config{}, nil),
		WritePolicy:          "",
		RecipePolicy:         "",
		RecipeCatalogDigest:  "",
		Verifier:             verifier.New(unitRegistry, plan),
		AcceptancePlanDigest: plan.Digest(),
		Recovery:             resumed.Seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := loop.Run(ctx, agent.Task{ID: taskID, Prompt: "finish the scan", WorkUnitID: "wu-b", SkipTaskFinalize: true})
	if result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("resumed unit outcome = %s (%s)", result.Outcome, result.StopReason)
	}
	requests := client.Requests()
	if len(requests) == 0 {
		t.Fatal("no provider request recorded")
	}
	// The VERY FIRST request of the new conversation carries the
	// reconstructed model context (the #51 projection), not just the unit
	// objective.
	for _, want := range []string{"work units: wu-a(completed), wu-b(ready)", "obs-000001", "wu-b"} {
		if !strings.Contains(requests[0].Prompt, want) {
			t.Fatalf("first request of the new session missing %q:\n%s", want, requests[0].Prompt)
		}
	}
	// Provenance of the resumed unit's rows is threaded by the real loop.
	if got := workUnitRowCount(t, stateDir,
		`SELECT COUNT(*) FROM tool_attempts WHERE work_unit_id = 'wu-b'`); got != 1 {
		t.Fatalf("wu-b tool attempts = %d, want 1 (threaded by the loop)", got)
	}
}

// TestResumeWithoutWorkUnitsCannotRunParent is the authoritative gate
// regression: a task with open persisted Work Units resumed WITHOUT
// --workunits must not run the parent loop or finalize; the task stays
// durably running and the later resume WITH --workunits completes it.
func TestResumeWithoutWorkUnitsCannotRunParent(t *testing.T) {
	workspace := crashWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "c.txt"), []byte("gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"finish the workspace scan","dependencies":["wu-a"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	scriptA := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t,
		append(crashRunArgs(scriptA, workspace, stateDir), "--workunits", workUnitsFile,
			"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms"),
		"tool_tx2_after", 2)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	attemptsBefore := countRowsFor(t, stateDir, taskID, "provider_attempts")

	// Resume WITHOUT --workunits: gated before the recovery pipeline; the
	// parent loop must not run and the task must stay resumable.
	parentScript := crashScript(t,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	gatedCode, gatedOut, gatedErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", parentScript,
		"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if gatedCode != exitWorkUnitGated {
		t.Fatalf("resume without --workunits exit = %d, want %d\nstderr:\n%s", gatedCode, exitWorkUnitGated, gatedErr)
	}
	if !strings.Contains(gatedErr, "open work units") {
		t.Fatalf("gated resume stderr missing open-units reason:\n%s", gatedErr)
	}
	if strings.Contains(gatedOut, "outcome: completed") {
		t.Fatal("gated resume must not complete the parent")
	}
	if got := countRowsFor(t, stateDir, taskID, "provider_attempts"); got != attemptsBefore {
		t.Fatalf("provider_attempts = %d after gated resume, want %d (no parent dispatch)", got, attemptsBefore)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE task_id = ?`, taskID).Scan(&status); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if status != "running" {
		t.Fatalf("task status = %q after gated resume, want running", status)
	}

	// The SAME task resumes WITH --workunits and reaches parent completion.
	scriptB := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit b continued","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"read_file"},{"evidence_id":"obs-000003","tool":"list_files"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000004","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", scriptB, "--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume with --workunits exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
}

// TestWorkUnitConfiguredProviderContinuity is the configured-provider gate:
// a Work Unit task run through a configured compatible endpoint persists the
// FULL execution configuration; resume through the same declarations proves
// provider/model/config identity continuity, and a diverged configuration is
// rejected fail-closed.
func TestWorkUnitConfiguredProviderContinuity(t *testing.T) {
	workspace := crashWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "c.txt"), []byte("gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"finish the workspace scan","dependencies":["wu-a"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	// The wire serves the whole lifecycle: run A (3 turns, interrupted in
	// wu-b) followed by the resumed conversation (4 turns).
	wireResponses := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"unit b continued","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"read_file"},{"evidence_id":"obs-000003","tool":"list_files"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000004","tool":"read_file"}]}</runstead_final>`,
	}
	wire := newE2EWire(provider.FamilyOpenAICompatible, wireResponses...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	baseURL := server.URL + "/v1"
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "wu-provider", baseURL, 0)
	acceptance := acceptanceFor(t, "a.txt")

	stateDir := t.TempDir()
	runArgs := []string{
		"run", "--task", "Inspect the workspace.",
		"--workspace", workspace,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
		"--workunits", workUnitsFile,
		"--acceptance", acceptance,
		"--providers", providersFile, "--provider-id", "wu-provider",
	}
	code, output := runCrashedRunAfter(t, runArgs, "tool_tx2_after", 2)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The persisted configuration snapshot carries the provider identity,
	// protocol/config identity and exact model, like a normal task.
	store := openWorkUnitStore(t, stateDir)
	preload, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	persistedIdentity := state.ProviderIdentityFromConfigSnapshot(preload.Task.ConfigJSON)
	if persistedIdentity.ProviderID != "wu-provider" || persistedIdentity.Model == "" || persistedIdentity.ConfigIdentity == "" {
		t.Fatalf("persisted provider identity = %+v, want wu-provider with model and config identity", persistedIdentity)
	}

	// Drift: the SAME provider id and endpoint but a DIFFERENT exact model is
	// rejected fail-closed while the task is still interrupted, before any
	// recovery or execution side effect and with zero physical requests.
	driftProviders := wuProviderFileWithModel(t, "wu-provider", baseURL, "drifted-model")
	driftCode, _, driftErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--workunits", workUnitsFile,
		"--acceptance", acceptance, "--min-start-interval", "1ms",
		"--providers", driftProviders, "--provider-id", "wu-provider", "--log-level", "error")
	if driftCode == exitSuccess {
		t.Fatalf("drifted resume must fail, stderr:\n%s", driftErr)
	}
	if !strings.Contains(driftErr, "divergence") {
		t.Fatalf("drifted resume stderr missing divergence reason:\n%s", driftErr)
	}
	if got := wire.count(); got != 3 {
		t.Fatalf("physical requests after drifted resume = %d, want 3 (drift fails before dispatch)", got)
	}

	// Resume through the SAME provider declarations (same endpoint, same
	// model) reaches completion; the provider/model/config identity
	// continuity is enforced by the resume pipeline. The shared wire keeps
	// serving the remaining responses of the lifecycle.
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--workunits", workUnitsFile,
		"--acceptance", acceptance, "--min-start-interval", "1ms",
		"--providers", providersFile, "--provider-id", "wu-provider", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	if got := wire.count(); got != 7 {
		t.Fatalf("physical requests = %d, want 7 (3 run A + 4 resumed)", got)
	}
}

// wuProviderFileWithModel writes a single-provider declarations document for
// the Work Unit configured-provider continuity test with an explicit model.
func wuProviderFileWithModel(t *testing.T, providerID, baseURL, model string) string {
	t.Helper()
	document := `{
		"version": 1,
		"providers": [
			{
				"provider_id": "` + providerID + `",
				"protocol_family": "openai_compatible",
				"base_url": "` + baseURL + `",
				"model": "` + model + `",
				"auth_requirement": "none",
				"options": null,
				"config_version": "v1",
				"profile": {
					"profile_version": "v1",
					"capabilities": ["text_turn", "runstead_protocol"],
					"route_safety": {
						"attempt_accounting": "single",
						"single_attempt": "guaranteed",
						"internal_retries": "disabled",
						"cooldown_replay": "disabled",
						"account_pooling": "disabled",
						"automatic_fallback": "disabled",
						"combo_routing": "disabled"
					},
					"max_request_bytes": 262144,
					"max_response_bytes": 1048576
				}
			}
		]
	}`
	path := filepath.Join(t.TempDir(), "wu-providers.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustCreateUnit creates one work unit for the in-process recovery tests.
func mustCreateUnit(t *testing.T, store *state.Store, create state.WorkUnitCreate) {
	t.Helper()
	if _, err := store.CreateWorkUnit(context.Background(), create); err != nil {
		t.Fatalf("CreateWorkUnit(%s): %v", create.WorkUnitID, err)
	}
}
