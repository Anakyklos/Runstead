package main

// Issue #106 re-review round 2: approval-paused Work Units must unblock from
// authoritative resolution state, uncertain effects must reconcile to a ready
// unit, workspace scopes use ONE canonical relative coordinate system, a
// zero-attempt resumed unit keeps its own counters, and the capability
// contract (omitted tools = task default; explicit [] = no tools) is
// intentional and enforced. Entirely Stage A; no workers/concurrency.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"github.com/RenyEnnos/Runstead/internal/workunit"

	_ "modernc.org/sqlite"
)

// TestWorkUnitApprovalBlockedUnblocksAfterDecide is the approval-pause E2E:
// a Work Unit proposing an approval-required write pauses blocked, the
// operator decides approved, and resume --workunits returns the SAME unit to
// an executable state and completes it with the write effect occurring at
// most once.
func TestWorkUnitApprovalBlockedUnblocksAfterDecide(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-apr","objective":"write and verify","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	stateDir := t.TempDir()
	acceptance := acceptanceFor(t, "a.txt")

	// Run A: the unit proposes an approval-required write and pauses.
	runScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"approved.txt","content":"owned\n","expected_before_hash":"absent"}}</runstead_action>`,
	)
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", runScript,
		"--workunits", workUnitsFile,
		"--acceptance", acceptance,
		"--write-policy", "write_file=approval_required",
		"--min-start-interval", "1ms",
		"--log-level", "error",
		"--state-dir", stateDir,
	}, &out, &errOut)
	if code != exitWorkUnitGated {
		t.Fatalf("run exit = %d, want %d (gated)\nstderr:\n%s", code, exitWorkUnitGated, errOut.String())
	}
	taskID := mustTaskID(t, errOut.String()+"\n"+out.String())
	if _, err := os.Stat(filepath.Join(workspace, "approved.txt")); !os.IsNotExist(err) {
		t.Fatal("approval-required write must not execute before the operator decision")
	}

	db := openReviewDB(t, stateDir)
	unit, err := openWorkUnitStore(t, stateDir).GetWorkUnit(context.Background(), taskID, "wu-apr")
	if err != nil {
		t.Fatal(err)
	}
	if unit.Status != "blocked" || !strings.Contains(unit.BlockingReason, "approval") {
		t.Fatalf("unit = %s (%q), want blocked by approval", unit.Status, unit.BlockingReason)
	}
	var actionID string
	if err := db.QueryRow(`SELECT action_id FROM actions ORDER BY created_at LIMIT 1`).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts WHERE tool = 'write_file'`); got != 0 {
		t.Fatalf("write tool attempts before decide = %d, want 0", got)
	}

	if code, output := runDecide(t, stateDir, taskID, actionID, "approved", "operator reviewed"); code != exitSuccess {
		t.Fatalf("decide exit = %d\n%s", code, output)
	}

	// Resume: the SAME unit unblocks (zero pending approvals, authoritative)
	// and completes; the write executes exactly once.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"approved.txt","content":"owned\n","expected_before_hash":"absent"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"approved write done","evidence":[{"evidence_id":"obs-000001","tool":"write_file"},{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--acceptance", acceptance, "--write-policy", "write_file=approval_required",
		"--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "approved.txt"))
	if err != nil || string(data) != "owned\n" {
		t.Fatalf("approved write effect = %q, %v; want exactly one effect", string(data), err)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts WHERE tool = 'write_file'`); got != 1 {
		t.Fatalf("write tool attempts = %d, want 1 (effect at most once)", got)
	}
	unit, err = openWorkUnitStore(t, stateDir).GetWorkUnit(context.Background(), taskID, "wu-apr")
	if err != nil {
		t.Fatal(err)
	}
	if unit.Status != "completed" {
		t.Fatalf("unit status = %s after resume, want completed", unit.Status)
	}
}

// TestWorkUnitUncertainReconcilesToReady proves uncertain units block until
// recovery reconciles their effects, then return to a safe executable state
// WITHOUT replay; unreconcilable effects stay blocking.
func TestWorkUnitUncertainReconcilesToReady(t *testing.T) {
	// (a) class-2 write interrupted after the effect: filesystem matches the
	// expected after-state -> reconciled with new evidence -> WU ready.
	t.Run("write effect reconciled to ready", func(t *testing.T) {
		workspace := t.TempDir()
		content := "written once\n"
		afterHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		store, taskID, stateDir := uncertainWriteFixture(t, workspace, afterHash, true)
		ctx := context.Background()

		resumed, err := recovery.Resume(ctx, store, recovery.Options{
			TaskID:             taskID,
			WorkspaceSignature: agent.WorkspaceSignature,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if resumed.Decision != recovery.DecisionContinue {
			t.Fatalf("decision = %s, want continue", resumed.Decision)
		}
		unit, err := store.GetWorkUnit(ctx, taskID, "wu-u")
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "ready" {
			t.Fatalf("unit = %s, want ready after successful effect reconciliation", unit.Status)
		}
		// The reconciliation produced citable evidence, and no replay: the
		// prepared tool attempt is exactly one row, now reconciled.
		found := false
		for _, item := range resumed.Seed.Evidence {
			if item.Tool == "write_file" {
				found = true
			}
		}
		if !found {
			t.Fatalf("reconciled write evidence missing from seed: %+v", resumed.Seed.Evidence)
		}
		if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts`); got != 1 {
			t.Fatalf("tool attempts = %d, want 1 (no replay)", got)
		}
	})

	// (b) unreconcilable effect: the filesystem matches neither the recorded
	// precondition nor the expected after-state, so reconciliation escalates
	// to human review; the unit stays uncertain and blocks.
	t.Run("unreconcilable effect stays blocking", func(t *testing.T) {
		workspace := t.TempDir()
		// The target exists with content that matches NEITHER the recorded
		// before-state (absent) NOR the expected after-state.
		if err := os.WriteFile(filepath.Join(workspace, "target.txt"), []byte("something else\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		content := "will not match\n"
		afterHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
		store, taskID, _ := uncertainWriteFixture(t, workspace, afterHash, false)
		ctx := context.Background()
		resumed, err := recovery.Resume(ctx, store, recovery.Options{
			TaskID:             taskID,
			WorkspaceSignature: agent.WorkspaceSignature,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if resumed.Decision != recovery.DecisionHumanReview {
			t.Fatalf("decision = %s, want human_review_required", resumed.Decision)
		}
		unit, err := store.GetWorkUnit(ctx, taskID, "wu-u")
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "uncertain" {
			t.Fatalf("unit = %s, want uncertain (unreconcilable effect blocks)", unit.Status)
		}
	})

	// (c) replay-safe class-1 observation: reconciles to ready without replay.
	t.Run("replay-safe observation reconciles to ready", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		store, taskID, stateDir := uncertainReadFixture(t, workspace)
		ctx := context.Background()
		resumed, err := recovery.Resume(ctx, store, recovery.Options{
			TaskID:             taskID,
			WorkspaceSignature: agent.WorkspaceSignature,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if resumed.Decision != recovery.DecisionContinue {
			t.Fatalf("decision = %s, want continue", resumed.Decision)
		}
		unit, err := store.GetWorkUnit(ctx, taskID, "wu-u")
		if err != nil {
			t.Fatal(err)
		}
		if unit.Status != "ready" {
			t.Fatalf("unit = %s, want ready after replay-safe reconciliation", unit.Status)
		}
		if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts`); got != 1 {
			t.Fatalf("tool attempts = %d, want 1 (no replay)", got)
		}
	})
}

// TestWorkUnitWorkspaceScopeRelativeEnforced is the real-loop scope gate: a
// Work Unit scoped to sub/ (canonical workspace-RELATIVE coordinate) can
// operate inside sub/ while ../ traversal and sibling reads fail closed with
// typed failures and zero content leakage.
func TestWorkUnitWorkspaceScopeRelativeEnforced(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sub", "a.txt"), []byte("inside sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte("TOP SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-scope","objective":"operate inside sub","tools":["read_file"],"workspace_scope":"sub","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"../secret.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"secret.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"scoped","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"sub/a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", script,
		"--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "sub/a.txt"),
		"--min-start-interval", "1ms",
		"--log-level", "error",
		"--state-dir", stateDir,
	}, &out, &errOut)
	if code != agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("run exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", code, errOut.String(), out.String())
	}
	taskID := mustTaskID(t, out.String())
	unit, err := openWorkUnitStore(t, stateDir).GetWorkUnit(context.Background(), taskID, "wu-scope")
	if err != nil {
		t.Fatal(err)
	}
	if unit.Status != "completed" {
		t.Fatalf("unit = %s, want completed", unit.Status)
	}
	// The sibling read failed closed with a typed path_not_found attempt; the
	// traversal proposal was rejected before any attempt (deterministic
	// invalid_arguments correction), and every tool attempt of the unit is a
	// read_file inside the scope.
	classifications := map[string]bool{}
	db := openReviewDB(t, stateDir)
	rows, err := db.Query(`SELECT classification FROM tool_attempts WHERE work_unit_id = 'wu-scope'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var class string
		if err := rows.Scan(&class); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		classifications[class] = true
	}
	rows.Close()
	if !classifications["path_not_found"] {
		t.Fatalf("sibling read must fail closed with a typed failure, got %v", classifications)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts WHERE work_unit_id = 'wu-scope'`); got != 2 {
		t.Fatalf("scoped tool attempts = %d, want 2 (scoped read + failed sibling read)", got)
	}
	// Zero content leakage: exactly ONE observation (the scoped read).
	var data string
	if err := db.QueryRow(`SELECT tr.data_json FROM tool_results tr JOIN tool_attempts ta ON ta.execution_id = tr.execution_id AND ta.work_unit_id = 'wu-scope'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "TOP SECRET") {
		t.Fatal("sibling/traversal read leaked workspace content")
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_results tr JOIN tool_attempts ta ON ta.execution_id = tr.execution_id WHERE ta.work_unit_id = 'wu-scope'`); got != 1 {
		t.Fatalf("scoped tool_results = %d, want 1 (only the scoped read)", got)
	}
}

// TestResumedUnitZeroAttemptsKeepsOwnCounters is the production-path
// regression: a unit that never dispatched before restart (completed sibling
// consumed turns) resumes through the REAL CLI chain with its OWN counters
// (zero), so its provider budget is not exhausted before its first dispatch.
func TestResumedUnitZeroAttemptsKeepsOwnCounters(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "c.txt"), []byte("gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-zero-task"
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
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-a", Objective: "first",
		AcceptancePlan: []byte(wuAcceptedPlan), AcceptanceDigest: plan.Digest(),
	})
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-b", Objective: "second", Dependencies: []string{"wu-a"},
		AcceptancePlan: []byte(wuAcceptedPlan), AcceptanceDigest: plan.Digest(),
		ProviderBudget: 2,
	})

	// wu-a completes through the REAL loop: two provider attempts (action +
	// final), both persisted under work_unit_id wu-a.
	wuAClient := newScriptedClient(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	wuAExecutor := newScriptedExecutorFor(t, wuAClient)
	wuALoop, err := agent.NewLoop(agent.Config{
		Runner: wuAExecutor, Registry: registry, Limits: agent.DefaultLimits(),
		Model: "scripted", ProviderIdentity: provider.Identity{},
		Trace: func(agent.TraceLine) {}, State: store,
		Policy:   policy.NewStatic(policy.Config{}, nil),
		Verifier: verifier.New(registry, plan), AcceptancePlanDigest: plan.Digest(),
		Recovery: &agent.RecoverySeed{},
	})
	if err != nil {
		t.Fatal(err)
	}
	wuAResult := wuALoop.Run(ctx, agent.Task{ID: taskID, Prompt: "first", WorkUnitID: "wu-a", SkipTaskFinalize: true})
	if wuAResult.Outcome != agent.OutcomeCompleted {
		t.Fatalf("wu-a outcome = %s (%s)", wuAResult.Outcome, wuAResult.StopReason)
	}
	for _, edge := range [][2]string{{"created", "ready"}, {"ready", "running"}, {"running", "completed"}} {
		if err := store.TransitionWorkUnit(ctx, taskID, "wu-a", edge[0], edge[1], ""); err != nil {
			t.Fatal(err)
		}
	}
	// wu-b was marked running but NEVER dispatched (zero provider attempts).
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-b", "created", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionWorkUnit(ctx, taskID, "wu-b", "ready", "running", ""); err != nil {
		t.Fatal(err)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts WHERE work_unit_id = 'wu-b'`); got != 0 {
		t.Fatalf("wu-b provider attempts = %d, want 0", got)
	}

	// Resume through the REAL CLI chain. wu-b declares provider_budget=2:
	// with its OWN zero counters, its two turns fit the budget. If it had
	// inherited the sibling/task counters (2), the budget would be exhausted
	// before its first dispatch and wu-b would never run.
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"first","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"second","dependencies":["wu-a"],"provider_budget":2,"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"b","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	// No --acceptance: resume continues under the plan persisted at task
	// start (the same wuAcceptedPlan the bootstrap saved).
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", resumeCode, resumeErr, resumeOut)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts WHERE work_unit_id = 'wu-b'`); got != 2 {
		t.Fatalf("wu-b provider attempts = %d, want 2 (zero-attempt unit ran with its OWN counters and budget)", got)
	}
	unit, err := openWorkUnitStore(t, stateDir).GetWorkUnit(context.Background(), taskID, "wu-b")
	if err != nil {
		t.Fatal(err)
	}
	if unit.Status != "completed" {
		t.Fatalf("wu-b status = %s, want completed", unit.Status)
	}
}

// TestWorkUnitExplicitEmptyToolsNoTools proves the intentional capability
// contract: tools:[] is a fail-closed no-tools envelope, so even read_file
// proposals are rejected deterministically with zero effects.
func TestWorkUnitExplicitEmptyToolsNoTools(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-notools","objective":"no tools","tools":[],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	// The model proposes read_file (rejected as unknown tool for this unit)
	// and then a final that cannot be grounded: the chain fails closed.
	script := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"no tools","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "complete the parent task",
		"--workspace", workspace,
		"--scripted", script,
		"--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"),
		"--min-start-interval", "1ms",
		"--log-level", "error",
		"--state-dir", stateDir,
	}, &out, &errOut)
	if code == agent.OutcomeCompleted.ExitCode() {
		t.Fatalf("no-tools unit must not complete\nstdout:\n%s", out.String())
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts`); got != 0 {
		t.Fatalf("tool attempts = %d, want 0 (explicit empty envelope is no tools)", got)
	}
}

// TestWorkUnitResumedLogicalTurnsExcludeRetryChildren is the configured-
// provider regression: ONE logical Work Unit turn that incurred a bounded
// governor retry (2 physical attempts) is, after a process restart, resumed
// as exactly ONE logical turn consumed. Every physical retry stays fully
// governor-accounted, and the unit's provider budget is counted in logical
// model-turn terms (base request ids only).
func TestWorkUnitResumedLogicalTurnsExcludeRetryChildren(t *testing.T) {
	workspace := crashWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "c.txt"), []byte("gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-a","objective":"inspect a.txt","tools":["read_file"],"provider_budget":2,"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	acceptance := acceptanceFor(t, "a.txt")

	// Run A: the unit's FIRST logical turn incurs a bounded governor retry:
	// physical request 1 (429) + retry child (200, the read). Crash after the
	// tool result TX2 leaves the unit running with ONE logical turn consumed
	// but TWO physical attempts on the wire. One wire serves the whole
	// lifecycle (requests 1-2 = run A; 3-5 = resume), so the re-supplied
	// endpoint keeps an identical config identity.
	wire := &e2eRetryWire{
		family: provider.FamilyOpenAICompatible,
		responses: []string{
			`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
			`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
			`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
			`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent done","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
		},
		firstFails: 1, status: 429, retryAfter: "1",
	}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "wu-provider", server.URL+"/v1", 0)

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
		"--retry-policy", "bounded",
	}
	code, output := runCrashedRunAfter(t, runArgs, "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	if got := wire.count(); got != 2 {
		t.Fatalf("run A physical requests = %d, want 2 (base + governor retry child)", got)
	}
	taskID := taskIDFromOutput(t, output)

	// Resume: the unit continues under provider_budget=2. Only logical turns
	// count: 1 consumed + 1 more (its final) fit. If the retry child had been
	// counted as a turn, the budget would be exhausted before the final and
	// the unit could never complete.
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--workunits", workUnitsFile,
		"--acceptance", acceptance, "--min-start-interval", "1ms",
		"--providers", providersFile, "--provider-id", "wu-provider",
		"--retry-policy", "bounded", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s\nstdout:\n%s", resumeCode, resumeErr, resumeOut)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	if got := wire.count(); got != 5 {
		t.Fatalf("physical requests = %d, want 5 (2 run A + 3 resumed)", got)
	}

	// The unit's logical turn count is 2 (base read + final), NOT 3 (the
	// retry child must not count).
	if got := workUnitRowCount(t, stateDir,
		`SELECT COUNT(DISTINCT client_request_id) FROM provider_attempts
		 WHERE work_unit_id = 'wu-a' AND client_request_id NOT LIKE '%-r%'`); got != 2 {
		t.Fatalf("wu-a logical request ids = %d, want 2 (base read + final)", got)
	}
}

// --- fixtures/helpers -------------------------------------------------------

// uncertainWriteFixture builds a task with a single 'uncertain' work unit
// holding one prepared class-2 write attempt. When effectPresent is true the
// target file already carries the expected after-state (the effect happened);
// the recovery pipeline then reconciles it as completed.
func uncertainWriteFixture(t *testing.T, workspace, afterHash string, effectPresent bool) (*state.Store, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-unc-task"
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: workspace, Model: "scripted",
		ConfigJSON: agent.ConfigSnapshot(registry, "scripted", provider.Identity{}, "", "", "", "", agent.DefaultLimits()),
	}, nil, registry); err != nil {
		t.Fatal(err)
	}
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-u", Objective: "uncertain unit", Tools: []string{"write_file", "read_file"},
	})
	for _, edge := range [][2]string{{"created", "ready"}, {"ready", "running"}, {"running", "uncertain"}} {
		if err := store.TransitionWorkUnit(ctx, taskID, "wu-u", edge[0], edge[1], "test"); err != nil {
			t.Fatal(err)
		}
	}
	actionID, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: taskID, WorkUnitID: "wu-u", Tool: "write_file",
		Arguments:   []byte(`{"path":"target.txt","content":"written once\n","expected_before_hash":"absent"}`),
		Fingerprint: "fp-wu-u",
	})
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: taskID, WorkUnitID: "wu-u", ActionID: actionID, Tool: "write_file",
		Arguments:       []byte(`{"path":"target.txt","content":"written once\n","expected_before_hash":"absent"}`),
		RecoveryClass:   2,
		EffectAfterHash: afterHash,
		PlannedEffect:   tools.PlannedEffect{},
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt: %v", err)
	}
	_ = executionID
	if effectPresent {
		if err := os.WriteFile(filepath.Join(workspace, "target.txt"), []byte("written once\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store, taskID, stateDir
}

// uncertainReadFixture builds a task with an 'uncertain' unit holding one
// prepared class-1 (replay-safe) read attempt.
func uncertainReadFixture(t *testing.T, workspace string) (*state.Store, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-unc-read"
	registry, err := tools.NewRegistry(tools.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.BootstrapTask(ctx, store, state.TaskRecord{
		TaskID: taskID, Objective: "parent", Workspace: workspace, Model: "scripted",
		ConfigJSON: agent.ConfigSnapshot(registry, "scripted", provider.Identity{}, "", "", "", "", agent.DefaultLimits()),
	}, nil, registry); err != nil {
		t.Fatal(err)
	}
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-u", Objective: "uncertain unit", Tools: []string{"read_file"},
	})
	for _, edge := range [][2]string{{"created", "ready"}, {"ready", "running"}, {"running", "uncertain"}} {
		if err := store.TransitionWorkUnit(ctx, taskID, "wu-u", edge[0], edge[1], "test"); err != nil {
			t.Fatal(err)
		}
	}
	actionID, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: taskID, WorkUnitID: "wu-u", Tool: "read_file",
		Arguments:   []byte(`{"path":"a.txt"}`),
		Fingerprint: "fp-wu-u-read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: taskID, WorkUnitID: "wu-u", ActionID: actionID, Tool: "read_file",
		Arguments:     []byte(`{"path":"a.txt"}`),
		RecoveryClass: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return store, taskID, stateDir
}

// wuRealConfigSnapshot renders the FULL task configuration snapshot exactly
// like a normal run (write policy, recipe policy, catalog digest, acceptance
// digest), so the resume pipeline can validate continuity.
func wuRealConfigSnapshot(t *testing.T, registry *tools.Registry, plan *verifier.Plan) []byte {
	t.Helper()
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
	return agent.ConfigSnapshot(registry, "scripted", provider.Identity{},
		writePolicyConfig.Spec(),
		recipePolicyConfig.RecipeSpec(recipeIDs(recipes)),
		recipes.Digest(), plan.Digest(), agent.DefaultLimits())
}

// openReviewDB opens the runstead SQLite database for assertion queries.
func openReviewDB(t *testing.T, stateDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newScriptedClient builds a fake runstead client for one in-process unit
// loop; it records every request (model-facing input + request ids).
func newScriptedClient(t *testing.T, responses ...string) *provider.Fake {
	t.Helper()
	wrapped := make([]provider.Response, 0, len(responses))
	for _, text := range responses {
		wrapped = append(wrapped, provider.Response{Text: text})
	}
	return provider.NewFake(wrapped...)
}

// newScriptedExecutorFor builds the governor-owned executor over the fake
// client, exactly like the production composition root.
func newScriptedExecutorFor(t *testing.T, client provider.Client) agent.AttemptRunner {
	t.Helper()
	config := governor.DefaultInstantConfig("wu-review-policy", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

// TestWorkUnitCancellationNoDispatchNoReplay is the real cancellation gate:
// the run context is canceled DURING the first unit's first model turn
// (blocking provider). The cancellation signal survives the driver boundary
// (wrapped context.Canceled, stable 130 semantics), no next unit or parent
// dispatches, no tool effect runs, the interrupted unit stays durably
// recoverable, and the real CLI resume reconciles and completes the chain
// without replay (exactly six new provider attempts, three effects).
func TestWorkUnitCancellationNoDispatchNoReplay(t *testing.T) {
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
	taskID := "wu-cancel-task"
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
		{WorkUnitID: "wu-a", Objective: "first", AcceptancePlan: json.RawMessage(wuAcceptedPlan)},
		{WorkUnitID: "wu-b", Objective: "second", Dependencies: []string{"wu-a"}, AcceptancePlan: json.RawMessage(wuAcceptedPlan)},
	}
	driver := &workunit.Driver{
		Store: store, TaskID: taskID,
		AllowedTools:  registryToolIDs(registry),
		TaskWorkspace: workspace,
	}
	if _, _, err := driver.EnsureDefinitions(ctx, definitions); err != nil {
		t.Fatal(err)
	}

	// The first unit's first MODEL TURN reaches the provider, which cancels
	// the run context deterministically at that moment and then blocks until
	// the cancellation propagates: the unit is mid-turn with an admit
	// persisted, before any effect.
	cancelCtx, cancel := context.WithCancel(context.Background())
	blockingExecutor := newPersistentScriptedExecutorFor(t, store, &cancelOnFirstCall{cancel: cancel})
	pieces := unitLoopPieces{
		runner:              blockingExecutor,
		registry:            registry,
		model:               "scripted",
		providerIdentity:    provider.Identity{},
		trace:               func(agent.TraceLine) {},
		store:               store,
		policy:              policy.NewStatic(policy.Config{}, nil),
		writePolicy:         "",
		recipePolicy:        "",
		recipeCatalogDigest: "",
		limits:              agent.DefaultLimits(),
		recovery:            &agent.RecoverySeed{},
	}
	chainErr := driver.RunAll(cancelCtx, func(ctx context.Context, unit state.WorkUnit) (workunit.RunResult, error) {
		return runUnitLoop(ctx, pieces, taskID, unit)
	})
	if !errors.Is(chainErr, context.Canceled) {
		t.Fatalf("canceled chain error = %v, want wrapped context.Canceled", chainErr)
	}
	// The canceled turn admitted exactly ONE provider attempt; no tool effect
	// ever ran; no next unit dispatched.
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts`); got != 1 {
		t.Fatalf("provider attempts = %d, want 1 (only the canceled turn's admission)", got)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts`); got != 0 {
		t.Fatalf("tool attempts = %d, want 0 (no effect before cancellation)", got)
	}
	interrupted, err := store.GetWorkUnit(ctx, taskID, "wu-a")
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != "running" {
		t.Fatalf("interrupted unit = %s, want running (durably recoverable)", interrupted.Status)
	}

	// The real CLI resume reconciles (running -> ready) and continues both
	// units and the parent with exactly six NEW provider attempts and three
	// effects: no replay of the canceled turn.
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"b","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"c.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"parent","evidence":[{"evidence_id":"obs-000003","tool":"read_file"}]}</runstead_final>`,
	)
	workUnitsFile := workUnitsFileFor(t, `[
	  {"work_unit_id":"wu-a","objective":"first","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}},
	  {"work_unit_id":"wu-b","objective":"second","dependencies":["wu-a"],"acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`)
	// The acceptance plan is the one persisted at task start (wuAcceptedPlan).
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts`); got != 7 {
		t.Fatalf("provider attempts total = %d, want 7 (1 canceled admission + 6 resumed, no replay)", got)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM tool_attempts`); got != 3 {
		t.Fatalf("tool attempts = %d, want 3 (no duplicate effects)", got)
	}
}

// TestWorkUnitUnsupportedVersionBlocksResume proves the durable contract
// version gate at the recovery boundary: a persisted Work Unit mutated to an
// unsupported version fails the resume BEFORE any provider dispatch or
// effect.
func TestWorkUnitUnsupportedVersionBlocksResume(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := verifier.ParsePlan([]byte(wuAcceptedPlan))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	store := openWorkUnitStore(t, stateDir)
	ctx := context.Background()
	taskID := "wu-version-task"
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
	mustCreateUnit(t, store, state.WorkUnitCreate{
		TaskID: taskID, WorkUnitID: "wu-v9", Objective: "future version",
		AcceptancePlan: []byte(wuAcceptedPlan), AcceptanceDigest: plan.Digest(),
	})
	if _, err := openReviewDB(t, stateDir).Exec(`UPDATE work_units SET version = 999 WHERE work_unit_id = 'wu-v9'`); err != nil {
		t.Fatal(err)
	}
	definitions := `[
	  {"work_unit_id":"wu-v9","objective":"future version","acceptance_plan":{"version":1,"checks":[{"id":"c1","type":"file_exists","path":"a.txt"}]}}
	]`
	workUnitsFile := workUnitsFileFor(t, definitions)
	resumeScript := writeScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"v9","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, _, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--workunits", workUnitsFile,
		"--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode == exitSuccess {
		t.Fatalf("unsupported-version resume must not succeed\nstderr:\n%s", resumeErr)
	}
	if !strings.Contains(resumeErr, "unsupported work unit version") {
		t.Fatalf("resume stderr missing version gate reason:\n%s", resumeErr)
	}
	if got := workUnitRowCount(t, stateDir, `SELECT COUNT(*) FROM provider_attempts`); got != 0 {
		t.Fatalf("provider attempts = %d, want 0 (version gate fails before dispatch)", got)
	}
}

// newPersistentScriptedExecutorFor builds the governor-owned executor over
// the fake client WITH the durable governor Persistence wired to the store,
// exactly like the production composition root (provider attempts persist).
func newPersistentScriptedExecutorFor(t *testing.T, store *state.Store, client provider.Client) agent.AttemptRunner {
	t.Helper()
	config := governor.DefaultInstantConfig("wu-review-policy", "fake", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	accountGovernor, err := governor.New(config, governor.Options{Persistence: store})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agent.NewExecutor(accountGovernor, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

// cancelOnFirstCall is a deterministic provider double: on the FIRST
// physical request it cancels the run context (the unit is mid-turn) and
// then blocks until the cancellation propagates through the governor-owned
// executor.
type cancelOnFirstCall struct {
	cancel context.CancelFunc
	once   atomic.Bool
}

func (p *cancelOnFirstCall) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (p *cancelOnFirstCall) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	if p.once.CompareAndSwap(false, true) {
		p.cancel()
	}
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}
