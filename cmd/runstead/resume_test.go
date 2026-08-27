package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// runCrashedRunAfter crashes the run helper on the Nth occurrence of the crash
// point, so tests can interrupt the process after a completed transition.
func runCrashedRunAfter(t *testing.T, args []string, point string, after int) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRuntimeCrashHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_RUNTIME_CRASH_HELPER=1",
		"RUNSTEAD_RUNTIME_CRASH_POINT="+point,
		"RUNSTEAD_RUNTIME_CRASH_AFTER="+itoa(after),
		"RUNSTEAD_RUNTIME_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("crash helper failed to run: %v\n%s", err, output)
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	return code, string(output)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// crashWorkspace returns a workspace with a.txt containing alpha.
func crashWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// crashScript builds a scripted responses file.
func crashScript(t *testing.T, responses ...string) string {
	t.Helper()
	return writeScript(t, responses...)
}

// runResume runs the resume command in-process and returns the exit code.
func runResume(ctx context.Context, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(ctx, append([]string{"resume"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

// toolResultData reads the persisted observation data for one evidence id.
func toolResultData(t *testing.T, stateDir, taskID, evidenceID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var data string
	if err := db.QueryRow(
		`SELECT data_json FROM tool_results WHERE task_id = ? AND evidence_id = ?`,
		taskID, evidenceID).Scan(&data); err != nil {
		t.Fatalf("query evidence %s: %v", evidenceID, err)
	}
	return data
}

func countRowsFor(t *testing.T, stateDir, taskID, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE task_id = ?`, taskID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// TestResumeBasicInterruptedReadOnlyTask is the primary acceptance scenario:
// a read-only task is interrupted in a real subprocess, then resumed from
// durable state and reaches completion with a new provider conversation.
func TestResumeBasicInterruptedReadOnlyTask(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// Resume with a fresh script (a new provider conversation): the model
	// re-proposes the completed read, which the seeded repeat guard rejects,
	// then finishes grounded on the persisted evidence.
	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"continued after crash","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	// The completed historical action was not executed again: exactly one tool
	// attempt exists after the resumed run.
	if got := countRowsFor(t, stateDir, taskID, "tool_attempts"); got != 1 {
		t.Fatalf("tool_attempts = %d, want 1 (the completed action must not execute twice)", got)
	}
	// The resumed run made new provider calls (2: proposal + final) on top of
	// the historical one, and never replayed the old conversation.
	if got := countRowsFor(t, stateDir, taskID, "provider_attempts"); got != 3 {
		t.Fatalf("provider_attempts = %d, want 3 (1 historical + 2 new)", got)
	}
	// The recovery boundary is visible in inspect.
	rendered := inspectRendered(t, stateDir, taskID)
	for _, want := range []string{
		"Resumes: 1",
		"recovery_started",
		"recovery_context_reconstructed",
		"recovery_continued",
		"Status: completed",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect missing %q:\n%s", want, rendered)
		}
	}
}

// TestResumeCompletedActionNotDuplicated instruments the duplicate case: the
// resumed run proposes the completed historical action and the seeded repeat
// guard rejects it instead of executing it again.
func TestResumeCompletedActionNotDuplicated(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The resumed script proposes the SAME completed action twice before the
	// final: both must be rejected as repeats, and the final must be grounded
	// on the persisted evidence without any new tool execution.
	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	if got := countRowsFor(t, stateDir, taskID, "tool_attempts"); got != 1 {
		t.Fatalf("tool_attempts = %d, want 1 (repeated proposals must not execute)", got)
	}
	// Both rejected proposals are persisted as distinct rejected actions.
	rendered := inspectRendered(t, stateDir, taskID)
	if strings.Count(rendered, "status=rejected") < 2 {
		t.Errorf("inspect must show both rejected proposals:\n%s", rendered)
	}
}

// TestResumePreparedReplaySafeObservationContinues crashes after the tool TX 1
// commit (the effect provably never started) and resumes: the prepared
// observation reconciles as replay-safe and a re-proposal executes as a new
// attempt.
func TestResumePreparedReplaySafeObservationContinues(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "tool_tx1_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("fixture must have a prepared tool attempt:\n%s", rendered)
	}

	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	// The prepared attempt reconciled as replay-safe and the re-proposal
	// executed as a new attempt: two tool attempts total, the first reconciled.
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "recovery=replay_safe_observation") {
		t.Errorf("inspect must show the replay-safe reconciliation:\n%s", rendered)
	}
	if got := countRowsFor(t, stateDir, taskID, "tool_attempts"); got != 2 {
		t.Fatalf("tool_attempts = %d, want 2 (prepared reconciled + new completed)", got)
	}
}

// TestResumeUncertainProviderAttemptKeepsDebit crashes after the provider TX 1
// commit: the request may have reached upstream. Resume must keep the
// conservative debit, never re-issue the old request, and continue with new
// governed attempts.
func TestResumeUncertainProviderAttemptKeepsDebit(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "provider_tx1_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)
	rendered := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "status=prepared") {
		t.Fatalf("fixture must have a prepared provider attempt:\n%s", rendered)
	}

	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, _, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	rendered = inspectRendered(t, stateDir, taskID)
	if !strings.Contains(rendered, "recovery=upstream_may_have_been_reached") {
		t.Errorf("inspect must show the conservative provider reconciliation:\n%s", rendered)
	}
	if !strings.Contains(rendered, "debited=1") {
		t.Errorf("inspect must show the retained conservative debit:\n%s", rendered)
	}
	// The old request was never re-issued: the prepared request id stays
	// reconciled and the new turns use fresh request ids.
	if strings.Count(rendered, "request="+taskID+"-0001") != 1 {
		t.Errorf("the interrupted request must appear exactly once (reconciled, not re-issued):\n%s", rendered)
	}
}

// TestResumeObservationAfterWorkspaceMutation proves fingerprint equality is
// not a result-reuse key: after an external workspace change, a re-read
// executes fresh and reflects the CURRENT workspace state.
func TestResumeObservationAfterWorkspaceMutation(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// The workspace changes externally between crash and resume.
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"fresh","evidence":[{"evidence_id":"obs-000002","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	// The second observation reflects the CURRENT workspace, not the stale one.
	second := toolResultData(t, stateDir, taskID, "obs-000002")
	if !strings.Contains(second, "beta") {
		t.Errorf("fresh evidence obs-000002 must contain the current content: %q", second)
	}
	if strings.Contains(second, "alpha") {
		t.Errorf("fresh evidence must not reuse the stale content: %q", second)
	}
	// Both observations persist; the old fingerprint never merged the actions.
	if got := countRowsFor(t, stateDir, taskID, "tool_results"); got != 2 {
		t.Fatalf("tool_results = %d, want 2", got)
	}
}

// TestResumeResultCommittedNextTurnMissing proves the "after TX 2, before the
// next model turn" crash window: the persisted observation is consumed by the
// resumed run without executing the completed historical tool call again.
func TestResumeResultCommittedNextTurnMissing(t *testing.T) {
	workspace := crashWorkspace(t)
	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(script, workspace, stateDir), "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)
	// The observation was committed before the crash.
	if got := countRowsFor(t, stateDir, taskID, "tool_results"); got != 1 {
		t.Fatalf("tool_results before resume = %d, want 1", got)
	}

	resumeScript := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"consumed old evidence","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeCode, _, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", resumeScript, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	// The committed observation was consumed without a re-execution: still
	// exactly one tool attempt and one tool result.
	if got := countRowsFor(t, stateDir, taskID, "tool_attempts"); got != 1 {
		t.Fatalf("tool_attempts after resume = %d, want 1", got)
	}
	if got := countRowsFor(t, stateDir, taskID, "tool_results"); got != 1 {
		t.Fatalf("tool_results after resume = %d, want 1", got)
	}
}

// TestResumeNewProviderConversation proves the resumed task continues with a
// different scripted provider input: the old conversation is never required.
func TestResumeNewProviderConversation(t *testing.T) {
	workspace := crashWorkspace(t)
	scriptA := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"old conversation","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	stateDir := t.TempDir()
	code, output := runCrashedRunAfter(t, crashRunArgs(scriptA, workspace, stateDir), "tool_tx2_after", 1)
	if code != 42 {
		t.Fatalf("crash helper exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)

	// Script B is a completely different provider input with no overlap with
	// script A; the resume must succeed from durable state alone.
	scriptB := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"continued via a brand new conversation","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"list_files"}]}</runstead_final>`,
	)
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		taskID, "--state-dir", stateDir, "--scripted", scriptB, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("resume stdout missing completed:\n%s", resumeOut)
	}
	// The new conversation executed a new observation (obs-000002) alongside
	// the persisted one and grounded both.
	if got := countRowsFor(t, stateDir, taskID, "tool_results"); got != 2 {
		t.Fatalf("tool_results = %d, want 2 (persisted + new)", got)
	}
}

// TestResumeGovernorProtectionSurvivesRestart proves a task blocked by
// restored account protection stays blocked: resume exits with the typed
// governor-blocked code, never reaches the provider, and leaves the task
// pending.
func TestResumeGovernorProtectionSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	workspace := crashWorkspace(t)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-blocked", Objective: "inspect", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-blocked"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	// One prepared provider attempt with a future cooldown persisted in the
	// governor projection: after restart the block must remain.
	future := time.Now().Add(time.Hour)
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2, CooldownUntil: future,
		Circuit:    governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:   governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		TaskStates: []governor.TaskStateRecord{{TaskID: "task-blocked", Attempts: 1, Retries: 0, LastTouched: future}},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: "task-blocked", ClientRequestID: "task-blocked-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: 1, StartedAt: future, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"should not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-blocked", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitGovernorBlocked {
		t.Fatalf("resume exit = %d, want %d (governor blocked)\nstderr:\n%s", resumeCode, exitGovernorBlocked, resumeErr)
	}
	if !strings.Contains(resumeErr, "cooldown") {
		t.Fatalf("resume diagnostic must explain the block:\n%s", resumeErr)
	}
	// No provider call happened after restart.
	if got := countRowsFor(t, stateDir, "task-blocked", "provider_attempts"); got != 1 {
		t.Fatalf("provider_attempts = %d, want 1 (no new provider call)", got)
	}
	// The task stays pending with a journaled recovery_blocked decision.
	rendered := inspectRendered(t, stateDir, "task-blocked")
	if !strings.Contains(rendered, "Status: running") {
		t.Errorf("blocked task must remain pending:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recovery_blocked") {
		t.Errorf("inspect must show recovery_blocked:\n%s", rendered)
	}
}

// TestResumeHumanReviewRequiredCLI proves the typed human-review outcome: an
// unreconcilable interrupted effect stops automatic continuation with exit
// code 5 and a persisted human_review_required state.
func TestResumeHumanReviewRequiredCLI(t *testing.T) {
	stateDir := t.TempDir()
	workspace := crashWorkspace(t)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-review", Objective: "inspect", Workspace: workspace, Model: "scripted",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-review"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	actionID, err := store.RecordAction(ctx, state.ActionRecord{
		TaskID: "task-review", Tool: "write_file", Arguments: []byte(`{"path":"x"}`),
		Fingerprint: "fp", WorkspaceSignature: "sig",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	if _, err := store.PrepareToolAttempt(ctx, state.ToolAttemptPrepared{
		TaskID: "task-review", ActionID: actionID, Tool: "write_file", Arguments: []byte(`{"path":"x"}`),
		RecoveryClass: 4,
	}); err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-review", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitHumanReview {
		t.Fatalf("resume exit = %d, want %d (human review)\nstderr:\n%s", resumeCode, exitHumanReview, resumeErr)
	}
	rendered := inspectRendered(t, stateDir, "task-review")
	if !strings.Contains(rendered, "Status: human_review_required") {
		t.Errorf("task must be persisted as human_review_required:\n%s", rendered)
	}
	if !strings.Contains(rendered, "task_human_review_required") {
		t.Errorf("inspect must show the human-review journal event:\n%s", rendered)
	}
	if strings.Contains(rendered, "must not run") {
		t.Error("the provider must never be reached after human review")
	}
	// A second resume reports the unresolved human review requirement with the
	// same typed exit code and never starts a new execution.
	code2, _, errOut2 := runResume(context.Background(),
		"task-review", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if code2 != exitHumanReview {
		t.Fatalf("second resume exit = %d, want %d\nstderr:\n%s", code2, exitHumanReview, errOut2)
	}
	if !strings.Contains(errOut2, "unresolved human review") {
		t.Fatalf("second resume diagnostic = %q", errOut2)
	}
}

// TestResumeCancellationPropagates proves cancellation through the resume
// command and that a canceled resume does not corrupt the pending task.
func TestResumeCancellationPropagates(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-cancel", Objective: "inspect", Workspace: "/ws", Model: "scripted",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-cancel"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"x","evidence":[]}</runstead_final>`)
	code, _, errOut := runResume(canceled, "task-cancel", "--state-dir", stateDir, "--scripted", script)
	if code != agent.OutcomeCanceled.ExitCode() {
		t.Fatalf("canceled resume exit = %d, want %d\nstderr:\n%s", code, agent.OutcomeCanceled.ExitCode(), errOut)
	}
	if !strings.Contains(errOut, "canceled") {
		t.Fatalf("canceled resume diagnostic = %q", errOut)
	}
}

// TestResumeMissingPersistedWorkspaceFailsCleanly proves resume fails with the
// stable unavailable code and never reaches the provider when the persisted
// task workspace no longer exists and no --workspace override is supplied.
func TestResumeMissingPersistedWorkspaceFailsCleanly(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "gone")
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-missing-ws", Objective: "inspect", Workspace: missing, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-missing-ws"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-missing-ws", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitUnavailable {
		t.Fatalf("resume exit = %d, want %d (unavailable)\nstderr:\n%s", resumeCode, exitUnavailable, resumeErr)
	}
	if !strings.Contains(resumeErr, "workspace unavailable") {
		t.Fatalf("resume diagnostic must explain the missing workspace:\n%s", resumeErr)
	}
	if got := countRowsFor(t, stateDir, "task-missing-ws", "provider_attempts"); got != 0 {
		t.Fatalf("provider_attempts = %d, want 0 (missing workspace must fail before any provider call)", got)
	}
	// Pre-flight validation means no recovery events were journaled and the
	// resume count was not inflated by an invocation that could not execute.
	rendered := inspectRendered(t, stateDir, "task-missing-ws")
	if strings.Contains(rendered, "recovery_started") {
		t.Errorf("pre-flight failure must not journal recovery_started:\n%s", rendered)
	}
	if strings.Contains(rendered, "Resumes:") {
		t.Errorf("pre-flight failure must not inflate the resume count:\n%s", rendered)
	}
}

// TestLimitsFromConfigPreservesPersistedBounds proves the resumed loop keeps
// the same control boundaries as the interrupted run, including explicit zero
// correction/repeat allowances.
func TestLimitsFromConfigPreservesPersistedBounds(t *testing.T) {
	limits, err := limitsFromConfig(`{
		"max_steps": 7,
		"max_corrections": 0,
		"max_repeated_actions": 0,
		"time_budget_ns": 25000000000,
		"provider_budget": 9
	}`)
	if err != nil {
		t.Fatalf("limitsFromConfig() error = %v", err)
	}
	if limits.MaxSteps != 7 || limits.MaxCorrections != 0 || limits.MaxRepeatedActions != 0 || limits.ProviderBudget != 9 || limits.TimeBudget != 25*time.Second {
		t.Fatalf("limits = %+v", limits)
	}
	// An empty or unknown snapshot falls back to the loop defaults.
	fallback, err := limitsFromConfig("{}")
	if err != nil {
		t.Fatalf("limitsFromConfig({}) error = %v", err)
	}
	if fallback.MaxSteps != agent.DefaultLimits().MaxSteps {
		t.Fatalf("fallback limits = %+v", fallback)
	}
	if _, err := limitsFromConfig("not json"); err == nil {
		t.Fatal("limitsFromConfig must reject an undecodable snapshot")
	}
}

// TestResumeGovernorBlocksHelper ensures recovery.GovernorBlocks is covered
// under the race detector through the CLI path.
func TestResumeGovernorBlocksHelper(t *testing.T) {
	config := governor.DefaultInstantConfig("policy", "scripted", "instant", provider.SafeRouteSafety())
	g, err := governor.New(config, governor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if blocked, _ := recovery.GovernorBlocks(g, "task-x"); blocked {
		t.Fatal("fresh governor must not report a block")
	}
}

// TestResumeUnavailableDatabaseFailsClearly covers the required CLI error
// matrix: a state directory that cannot be created reports the stable
// unavailable code with a clear diagnostic.
func TestResumeUnavailableDatabaseFailsClearly(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"resume", "task-1", "--state-dir", filepath.Join(blocker, "state")}, &out, &errOut)
	if code != exitUnavailable {
		t.Fatalf("resume exit code = %d, want %d\nstderr:\n%s", code, exitUnavailable, errOut.String())
	}
	if !strings.Contains(errOut.String(), "state database unavailable") {
		t.Fatalf("resume diagnostic = %q", errOut.String())
	}
}

// TestResumeCorruptStateFailsClearly covers the corrupted/incompatible
// persisted state error: an undecodable persisted configuration snapshot stops
// with the stable corrupt-state code (6) and never reaches the provider.
func TestResumeCorruptStateFailsClearly(t *testing.T) {
	workspace := crashWorkspace(t)
	stateDir := t.TempDir()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-corrupt", Objective: "inspect", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte("not json"),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-corrupt"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-corrupt", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitCorrupt {
		t.Fatalf("resume exit = %d, want %d (corrupt state)\nstderr:\n%s", resumeCode, exitCorrupt, resumeErr)
	}
	if !strings.Contains(resumeErr, "persisted configuration") {
		t.Fatalf("resume diagnostic must explain the corrupt state:\n%s", resumeErr)
	}
	if got := countRowsFor(t, stateDir, "task-corrupt", "provider_attempts"); got != 0 {
		t.Fatalf("provider_attempts = %d, want 0 (corrupt state must fail before any provider call)", got)
	}
}

// TestResumeGovernorCircuitBlockedSurvivesRestart proves the restored account
// circuit blocks continuation after restart, complementing the cooldown test:
// the typed governor-blocked code is returned, no provider call happens and
// the task stays pending.
func TestResumeGovernorCircuitBlockedSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	workspace := crashWorkspace(t)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-circuit", Objective: "inspect", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-circuit"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	future := time.Now().Add(time.Hour)
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:    governor.CircuitSnapshot{State: governor.CircuitOpenUntil, OpenUntil: future, Reason: governor.OutcomeRateCapacity},
		Ceilings:   governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		TaskStates: []governor.TaskStateRecord{{TaskID: "task-circuit", Attempts: 1, Retries: 0, LastTouched: future}},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: "task-circuit", ClientRequestID: "task-circuit-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: 1, StartedAt: future, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-circuit", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitGovernorBlocked {
		t.Fatalf("resume exit = %d, want %d (governor blocked)\nstderr:\n%s", resumeCode, exitGovernorBlocked, resumeErr)
	}
	if !strings.Contains(resumeErr, "circuit") {
		t.Fatalf("resume diagnostic must explain the circuit block:\n%s", resumeErr)
	}
	if got := countRowsFor(t, stateDir, "task-circuit", "provider_attempts"); got != 1 {
		t.Fatalf("provider_attempts = %d, want 1 (no new provider call)", got)
	}
	rendered := inspectRendered(t, stateDir, "task-circuit")
	if !strings.Contains(rendered, "recovery_blocked") {
		t.Errorf("inspect must show recovery_blocked:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: running") {
		t.Errorf("blocked task must remain pending:\n%s", rendered)
	}
}

// TestResumeRejectsWorkspaceOverride proves the task workspace is part of its
// durable identity: --workspace is not a resume flag, because continuing the
// same task in a different directory would break evidence/guard identity.
func TestResumeRejectsWorkspaceOverride(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"resume", "task-1", "--workspace", "/tmp/whatever", "--state-dir", t.TempDir()}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("resume exit code = %d, want %d\nstderr:\n%s", code, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "unknown flag") || !strings.Contains(errOut.String(), "--workspace") {
		t.Fatalf("resume diagnostic = %q, want unknown flag --workspace", errOut.String())
	}
}

// TestResumeReceiptAwareConservativeDebitBlocksCLI proves the reviewer blocker
// end to end through the CLI: a receipt-aware attempt interrupted before TX 2
// is reconciled, the conservative #29 debit is applied to the persisted
// governor projection (unsafe telemetry), and resume exits with the typed
// governor-blocked code without any provider call.
func TestResumeReceiptAwareConservativeDebitBlocksCLI(t *testing.T) {
	stateDir := t.TempDir()
	workspace := crashWorkspace(t)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-receipt", Objective: "inspect", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-receipt"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	now := time.Now().UTC()
	// Receipt-aware TX1 with ZERO debit: StartReceiptAware defers all debits to
	// the receipt finish path, so a crash between TX1 and TX2 leaves the
	// governor protection projection not debited.
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:  governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: "task-receipt", ClientRequestID: "task-receipt-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: 1, StartedAt: now, ReceiptAware: true, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"must not run","evidence":[]}</runstead_final>`)
	resumeCode, _, resumeErr := runResume(context.Background(),
		"task-receipt", "--state-dir", stateDir, "--scripted", script, "--log-level", "error")
	if resumeCode != exitGovernorBlocked {
		t.Fatalf("resume exit = %d, want %d (governor blocked)\nstderr:\n%s", resumeCode, exitGovernorBlocked, resumeErr)
	}
	if !strings.Contains(resumeErr, "unsafe") {
		t.Fatalf("resume diagnostic must explain the unsafe conservative accounting:\n%s", resumeErr)
	}
	// No provider call happened after reconciliation.
	if got := countRowsFor(t, stateDir, "task-receipt", "provider_attempts"); got != 1 {
		t.Fatalf("provider_attempts = %d, want 1 (no new provider call)", got)
	}
	// The attempt is reconciled, the conservative debit is journaled, the task
	// stays pending, and the persisted telemetry is unsafe.
	rendered := inspectRendered(t, stateDir, "task-receipt")
	if !strings.Contains(rendered, "recovery=upstream_may_have_been_reached") {
		t.Errorf("inspect must show the conservative reconciliation:\n%s", rendered)
	}
	if !strings.Contains(rendered, "telemetry: unsafe") {
		t.Errorf("inspect must show the persisted unsafe telemetry:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Status: running") {
		t.Errorf("blocked task must remain pending:\n%s", rendered)
	}
	if strings.Contains(rendered, "recovery_continued") {
		t.Errorf("blocked resume must not journal recovery_continued:\n%s", rendered)
	}
}

// TestResumeCrashDuringRecoveryLeavesConsistentState kills the resume process
// after the provider reconciliation commit: the journaled prefix must survive,
// and a second resume must complete the task without re-reconciling.
func TestResumeCrashDuringRecoveryLeavesConsistentState(t *testing.T) {
	stateDir := t.TempDir()
	workspace := crashWorkspace(t)
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: "task-crash", Objective: "inspect", Workspace: workspace, Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-crash"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	now := time.Now().UTC()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:      governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		RollingEvents: []governor.LedgerEvent{{At: now, TaskID: "task-crash"}},
		TaskStates:    []governor.TaskStateRecord{{TaskID: "task-crash", Attempts: 1, Retries: 0, LastTouched: now}},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: "task-crash", ClientRequestID: "task-crash-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: 1, StartedAt: now, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	script := crashScript(t,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	)
	resumeArgs := []string{"resume", "task-crash", "--state-dir", stateDir, "--scripted", script, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error"}
	cmd := exec.Command(os.Args[0], "-test.run=TestRuntimeResumeCrashHelper")
	cmd.Env = append(os.Environ(),
		"RUNSTEAD_RUNTIME_RESUME_CRASH_HELPER=1",
		"RUNSTEAD_RUNTIME_RESUME_CRASH_POINT=recovery_provider_reconciled_after",
		"RUNSTEAD_RUNTIME_RESUME_ARGS="+strings.Join(resumeArgs, "\x1f"),
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("resume crash helper failed to run: %v\n%s", err, output)
	}
	if exitErr == nil || exitErr.ExitCode() != 42 {
		t.Fatalf("resume crash helper exit = %v, want 42\n%s", exitErr, output)
	}

	// The crash left the reconciliation journaled and no recovery_continued.
	rendered := inspectRendered(t, stateDir, "task-crash")
	if !strings.Contains(rendered, "recovery_started") || !strings.Contains(rendered, "provider_attempt_reconciled") {
		t.Fatalf("crash during recovery must keep the journaled prefix:\n%s", rendered)
	}
	if strings.Contains(rendered, "recovery_continued") {
		t.Fatalf("crash before continuation must not journal recovery_continued:\n%s", rendered)
	}
	// A second resume completes the task; the already-reconciled attempt is
	// not re-reconciled.
	resumeCode, resumeOut, resumeErr := runResume(context.Background(),
		"task-crash", "--state-dir", stateDir, "--scripted", script, "--acceptance", acceptanceFor(t, "a.txt"), "--min-start-interval", "1ms", "--log-level", "error")
	if resumeCode != exitSuccess {
		t.Fatalf("second resume exit = %d, want 0\nstderr:\n%s", resumeCode, resumeErr)
	}
	if !strings.Contains(resumeOut, "outcome: completed") {
		t.Fatalf("second resume stdout missing completed:\n%s", resumeOut)
	}
	if strings.Count(inspectRendered(t, stateDir, "task-crash"), "provider_attempt_reconciled") != 1 {
		t.Fatal("the already-reconciled attempt must not be reconciled twice")
	}
}

// Issue #58: resume reconstructs the allowance policy from the persisted
// profile/kind instead of always assuming the published-quota Instant policy,
// and fails closed when the persisted policy contradicts the reconstruction.
func TestResumeReconstructsUnlimitedTextPolicyFromPersistedState(t *testing.T) {
	persisted := governor.PersistedState{
		AccountPolicyID:  "runstead-cli",
		ProviderID:       "scripted",
		ModelPool:        "instant",
		Model:            "scripted",
		AllowanceProfile: governor.ProfileLunaUnlimitedText,
		AllowanceKind:    governor.AllowanceKindUnlimitedText,
		NextAttempt:      3,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:         governor.BudgetCeilings{TaskBudget: 80, RetryBudget: 2},
		TaskStates:       []governor.TaskStateRecord{{TaskID: "task-1", Attempts: 2, LastTouched: time.Now().UTC()}},
	}
	accountConfig, err := resolveResumeGovernorConfig(&persisted, "", false)
	if err != nil {
		t.Fatalf("resolveResumeGovernorConfig() error = %v", err)
	}
	if accountConfig.AllowanceProfile != governor.ProfileLunaUnlimitedText || accountConfig.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("reconstructed policy = profile %q kind %q, want luna unlimited", accountConfig.AllowanceProfile, accountConfig.AllowanceKind)
	}
	if accountConfig.Rolling3h != 0 || accountConfig.ManualReserve != 0 {
		t.Fatalf("reconstructed unlimited policy fabricated numeric allowance state: %#v", accountConfig)
	}
	if accountConfig.MaxInFlight != 1 || accountConfig.TaskBudget != 80 || accountConfig.RetryBudget != 2 || accountConfig.QueueCapacity != 16 {
		t.Fatalf("reconstructed unlimited policy dropped local workload controls: %#v", accountConfig)
	}
}

func TestResumeReconstructsUnknownPolicyFromPersistedState(t *testing.T) {
	persisted := governor.PersistedState{
		AllowanceProfile: governor.ProfileUnknown,
		AllowanceKind:    governor.AllowanceKindUnknown,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{
			Rolling3h: 40, Rolling1h: 20, Rolling10m: 8, TaskBudget: 10, RetryBudget: 1, ManualReserve: 8,
		},
	}
	accountConfig, err := resolveResumeGovernorConfig(&persisted, "", false)
	if err != nil {
		t.Fatalf("resolveResumeGovernorConfig() error = %v", err)
	}
	if accountConfig.AllowanceKind != governor.AllowanceKindUnknown {
		t.Fatalf("reconstructed unknown policy = %#v", accountConfig)
	}
	// The conservative local layer is the durable policy the operator
	// configured: the reconstructed unknown policy must carry the persisted
	// explicit ceilings and reserve (#21 contract, #58 review).
	if accountConfig.Rolling3h != 40 || accountConfig.Rolling1h != 20 || accountConfig.Rolling10m != 8 ||
		accountConfig.ManualReserve != 8 || accountConfig.TaskBudget != 10 || accountConfig.RetryBudget != 1 {
		t.Fatalf("reconstructed unknown policy lost the persisted local layer: %#v", accountConfig)
	}
}

func TestResumeFailsClosedOnUnknownProjectionWithoutLocalCeilings(t *testing.T) {
	persisted := governor.PersistedState{
		AllowanceProfile: governor.ProfileUnknown,
		AllowanceKind:    governor.AllowanceKindUnknown,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:         governor.BudgetCeilings{},
	}
	if _, err := resolveResumeGovernorConfig(&persisted, "", false); err == nil {
		t.Fatal("resolveResumeGovernorConfig() accepted an unknown projection without explicit local ceilings")
	}
}

func TestResumeFailsClosedOnFabricatedCeilingsForUnlimitedText(t *testing.T) {
	persisted := governor.PersistedState{
		AllowanceProfile: governor.ProfileLunaUnlimitedText,
		AllowanceKind:    governor.AllowanceKindUnlimitedText,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{
			Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2,
		},
	}
	if _, err := resolveResumeGovernorConfig(&persisted, "", false); err == nil {
		t.Fatal("resolveResumeGovernorConfig() accepted fabricated rolling ceilings on an unlimited-text policy")
	}
}

func TestResumeFailsClosedOnIncompatiblePersistedReserve(t *testing.T) {
	persisted := governor.PersistedState{
		AllowanceProfile: governor.ProfileInstant,
		AllowanceKind:    governor.AllowanceKindPublishedQuota,
		Circuit:          governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings: governor.BudgetCeilings{
			Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2, ManualReserve: 5,
		},
	}
	if _, err := resolveResumeGovernorConfig(&persisted, "", false); err == nil {
		t.Fatal("resolveResumeGovernorConfig() accepted a persisted reserve that contradicts the reconstructed policy")
	}
}

// TestResolveGovernorConfigProviderNeutralDefaultsToUnknown is the #14
// review regression: a configured compatible endpoint must never inherit the
// historical plus_go_instant published-quota contract by default. Without an
// explicit operator declaration, the provider-neutral surface stays unknown
// (conservative local ceilings, no fabricated upstream quota). The legacy
// scripted/OmniRoute lanes keep their historical default.
func TestResolveGovernorConfigProviderNeutralDefaultsToUnknown(t *testing.T) {
	t.Setenv(config.EnvAllowanceProfile, "")
	resolved := &provider.Resolved{ProviderID: "provider-a", Model: "model-a", ProtocolFamily: provider.FamilyOpenAICompatible}

	// No explicit declaration: unknown, never the historical instant contract.
	unknown, err := resolveGovernorConfig(false, config.Config{}, resolved, "", false, "", false)
	if err != nil {
		t.Fatalf("resolveGovernorConfig() error = %v", err)
	}
	if unknown.AllowanceProfile != governor.ProfileUnknown || unknown.AllowanceKind != governor.AllowanceKindUnknown {
		t.Fatalf("provider-neutral default profile = %#v, want unknown/unknown", unknown)
	}
	if unknown.ProviderID != "provider-a" || unknown.Model != "model-a" {
		t.Fatalf("provider identity lost: %#v", unknown)
	}
	if unknown.ProtocolFamily != resolved.ProtocolFamily || unknown.ConfigIdentity != resolved.ConfigIdentity {
		t.Fatalf("sanitized identity must reach the governor: %#v", unknown)
	}

	// An explicit operator declaration is honored.
	explicit, err := resolveGovernorConfig(false, config.Config{}, resolved, "", false, "plus_go_instant", true)
	if err != nil {
		t.Fatalf("resolveGovernorConfig(explicit) error = %v", err)
	}
	if explicit.AllowanceProfile != governor.ProfileInstant || explicit.AllowanceKind != governor.AllowanceKindPublishedQuota {
		t.Fatalf("explicit instant profile not honored: %#v", explicit)
	}

	// The legacy scripted lane keeps the historical default.
	scripted, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "", false)
	if err != nil {
		t.Fatalf("resolveGovernorConfig(scripted) error = %v", err)
	}
	if scripted.AllowanceProfile != governor.ProfileInstant {
		t.Fatalf("scripted lane default changed: %#v", scripted)
	}
}

// Issue #58 review: the CLI profile selection must be explicit and must not
// weaken the local layer. Unknown keeps the conservative local ceilings and
// reserve; unlimited text keeps no numeric layer; the default remains the
// historical published-quota Instant policy.
func TestResolveGovernorConfigSelectsExplicitAllowanceProfiles(t *testing.T) {
	t.Setenv(config.EnvAllowanceProfile, "")

	defaultConfig, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "", false)
	if err != nil {
		t.Fatalf("default resolveGovernorConfig() error = %v", err)
	}
	if defaultConfig.AllowanceProfile != governor.ProfileInstant || defaultConfig.AllowanceKind != governor.AllowanceKindPublishedQuota {
		t.Fatalf("default profile = %#v, want plus_go_instant published_quota", defaultConfig)
	}
	if defaultConfig.Rolling3h != 140 || defaultConfig.ManualReserve != 20 {
		t.Fatalf("default profile lost the historical numbers: %#v", defaultConfig)
	}

	unknown, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "unknown", true)
	if err != nil {
		t.Fatalf("unknown resolveGovernorConfig() error = %v", err)
	}
	if unknown.AllowanceKind != governor.AllowanceKindUnknown || unknown.Rolling3h <= 0 || unknown.ManualReserve <= 0 {
		t.Fatalf("CLI unknown profile dropped the conservative local layer: %#v", unknown)
	}

	unlimited, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "luna_unlimited_text", true)
	if err != nil {
		t.Fatalf("luna resolveGovernorConfig() error = %v", err)
	}
	if unlimited.AllowanceKind != governor.AllowanceKindUnlimitedText || unlimited.Rolling3h != 0 || unlimited.ManualReserve != 0 {
		t.Fatalf("CLI luna profile fabricated numeric allowance state: %#v", unlimited)
	}
	if unlimited.MaxInFlight != 1 || unlimited.TaskBudget != 80 || unlimited.RetryBudget != 2 {
		t.Fatalf("CLI luna profile dropped local workload controls: %#v", unlimited)
	}

	if _, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "reasoning", true); err == nil {
		t.Fatal("CLI accepted reasoning without explicit ceilings")
	}
	if _, err := resolveGovernorConfig(true, config.Config{}, nil, "", false, "not-a-profile", true); err == nil {
		t.Fatal("CLI accepted an unsupported allowance profile")
	}
}
