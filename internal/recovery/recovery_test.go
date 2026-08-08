package recovery_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recovery"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

const fixtureTask = "task-1"

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(t.TempDir(), "runstead.db")})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustCreate(t *testing.T, store *state.Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: taskID, Objective: "inspect the workspace", Workspace: "/ws", Model: "scripted",
		ConfigJSON: []byte(`{"max_steps":24,"max_corrections":2,"max_repeated_actions":2,"time_budget_ns":600000000000,"provider_budget":80}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
}

func mustAction(t *testing.T, store *state.Store, taskID, tool, arguments string, fingerprint, signature string) string {
	t.Helper()
	actionID, err := store.RecordAction(context.Background(), state.ActionRecord{
		TaskID: taskID, Tool: tool, Arguments: []byte(arguments),
		Fingerprint: fingerprint, WorkspaceSignature: signature,
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	return actionID
}

func mustToolAttempt(t *testing.T, store *state.Store, taskID, actionID, tool, arguments string, recoveryClass int) string {
	t.Helper()
	executionID, err := store.PrepareToolAttempt(context.Background(), state.ToolAttemptPrepared{
		TaskID: taskID, ActionID: actionID, Tool: tool, Arguments: []byte(arguments), RecoveryClass: recoveryClass,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	return executionID
}

func mustCompleteTool(t *testing.T, store *state.Store, taskID, executionID, evidenceID, tool, content string) {
	t.Helper()
	observation := tools.Observation{
		ID: evidenceID, Tool: tool, Success: true,
		Data:     map[string]any{"path": "a.txt", "content": content},
		Metadata: tools.Metadata{Source: tool, Untrusted: true, Path: "a.txt", ExitCode: 0},
	}
	if err := store.CompleteToolAttempt(context.Background(), state.ToolAttemptCompleted{
		TaskID: taskID, ExecutionID: executionID, Status: "completed", EvidenceID: evidenceID,
		DurationNanos: 1000, Observation: observation,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
}

func mustProviderPrepared(t *testing.T, store *state.Store, taskID, requestID string, sequence int) {
	t.Helper()
	now := time.Now().UTC()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: sequence + 1,
		Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:      governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		RollingEvents: []governor.LedgerEvent{{At: now, TaskID: taskID}},
		TaskStates:    []governor.TaskStateRecord{{TaskID: taskID, Attempts: sequence, Retries: 0, LastTouched: now}},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: taskID, ClientRequestID: requestID, ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: sequence,
		StartedAt: now, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
}

func mustProviderCompleted(t *testing.T, store *state.Store, taskID, requestID string, sequence int) {
	t.Helper()
	now := time.Now().UTC()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
		AllowanceProfile: governor.ProfileInstant, NextAttempt: sequence + 1,
		Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:      governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		RollingEvents: []governor.LedgerEvent{{At: now, TaskID: taskID}},
		TaskStates:    []governor.TaskStateRecord{{TaskID: taskID, Attempts: sequence, Retries: 0, LastTouched: now}},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: taskID, ClientRequestID: requestID, ProviderID: "scripted", ModelPool: "instant",
		Model: "scripted", AllowanceProfile: governor.ProfileInstant, AttemptSequence: sequence,
		StartedAt: now, State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	persisted.NextAttempt = sequence + 2
	persisted.TaskStates[0].Attempts = sequence + 1
	if err := store.RecordProviderFinished(context.Background(), governor.ProviderFinished{
		TaskID: taskID, ClientRequestID: requestID, Outcome: governor.OutcomeSuccess,
		UpstreamReached: true, AttemptDebited: 1, Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
		State: persisted,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
}

func traceCapture(t *testing.T) (*[]agent.TraceLine, agent.TraceSink) {
	var lines []agent.TraceLine
	return &lines, func(line agent.TraceLine) { lines = append(lines, line) }
}

func mustLoadSnapshot(t *testing.T, store *state.Store, taskID string) *state.RecoverySnapshot {
	t.Helper()
	snapshot, err := store.LoadRecoverySnapshot(context.Background(), taskID)
	if err != nil {
		t.Fatalf("LoadRecoverySnapshot() error = %v", err)
	}
	return snapshot
}

func traceKinds(lines []agent.TraceLine) []string {
	kinds := make([]string, len(lines))
	for index, line := range lines {
		kinds[index] = line.Kind
	}
	return kinds
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return len(values)
}

func mustRender(t *testing.T, store *state.Store, taskID string) string {
	t.Helper()
	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, taskID); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	return out.String()
}

func TestResumeReconcilesInterruptedAttemptsAndContinues(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-read-a", "sig-alpha")
	execID := mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)
	mustCompleteTool(t, store, fixtureTask, execID, "obs-000001", "read_file", "alpha\n")
	// A second action was interrupted: prepared but never executed.
	actionID2 := mustAction(t, store, fixtureTask, "read_file", `{"path":"b.txt"}`, "fp-read-b", "sig-alpha")
	mustToolAttempt(t, store, fixtureTask, actionID2, "read_file", `{"path":"b.txt"}`, 1)
	// One provider request may have reached upstream.
	mustProviderPrepared(t, store, fixtureTask, "task-1-0002", 2)

	captured, traceSink := traceCapture(t)
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask, Trace: traceSink})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %q, want continue (reason %q)", plan.Decision, plan.Reason)
	}
	if plan.ReconciledToolAttempts != 1 {
		t.Errorf("reconciled tool attempts = %d, want 1", plan.ReconciledToolAttempts)
	}
	if plan.ReconciledProviderAttempts != 1 {
		t.Errorf("reconciled provider attempts = %d, want 1", plan.ReconciledProviderAttempts)
	}
	if plan.Seed == nil {
		t.Fatal("continue plan must carry a seed")
	}
	// The interrupted prepared tool attempt was reconciled as replay-safe while
	// the completed attempt stayed untouched.
	interrupted := ""
	for _, attempt := range mustLoadSnapshot(t, store, fixtureTask).ToolAttempts {
		if attempt.Status == "reconciled" {
			interrupted = attempt.ExecutionID
			if attempt.RecoveryReason != "replay_safe_observation" {
				t.Errorf("interrupted attempt reason = %q, want replay_safe_observation", attempt.RecoveryReason)
			}
		}
		if attempt.EvidenceID == "obs-000001" && attempt.Status != "completed" {
			t.Errorf("completed attempt obs-000001 status = %q, want completed (untouched)", attempt.Status)
		}
	}
	if interrupted == "" {
		t.Error("no interrupted tool attempt was reconciled")
	}
	if interrupted == execID {
		t.Error("the completed attempt must not be reconciled")
	}
	// The prepared provider attempt was reconciled conservatively: the debit
	// stands and the may-have-reached marker is preserved.
	for _, attempt := range mustLoadSnapshot(t, store, fixtureTask).ProviderAttempts {
		if attempt.Status != "reconciled" || !attempt.Uncertain || attempt.AttemptDebited != 1 || attempt.RecoveryReason != "upstream_may_have_been_reached" {
			t.Errorf("prepared provider attempt = %s uncertain=%t debited=%d reason=%s",
				attempt.Status, attempt.Uncertain, attempt.AttemptDebited, attempt.RecoveryReason)
		}
	}
	// Seed: the completed observation is citable, the guard is seeded with the
	// executed action's fingerprint and the counters continue.
	if len(plan.Seed.Evidence) != 1 || plan.Seed.Evidence[0].ID != "obs-000001" {
		t.Fatalf("seed evidence = %+v, want obs-000001", plan.Seed.Evidence)
	}
	if plan.Seed.Guard["fp-read-a"] != "sig-alpha" {
		t.Errorf("seed guard missing fp-read-a -> sig-alpha: %v", plan.Seed.Guard)
	}
	if _, ok := plan.Seed.Guard["fp-read-b"]; ok {
		t.Errorf("prepared (not executed) action must not seed the guard: %v", plan.Seed.Guard)
	}
	if plan.Seed.Turns != 1 || plan.Seed.Attempts != 1 {
		t.Errorf("seed counters = %d/%d, want 1/1 (one historical provider attempt)", plan.Seed.Turns, plan.Seed.Attempts)
	}
	// The reconstructed context is bounded and retains required evidence.
	if plan.Seed.Context == "" {
		t.Error("reconstructed context must not be empty")
	}
	if !strings.Contains(plan.Seed.Context, "obs-000001") {
		t.Error("reconstructed context must retain the evidence id")
	}
	if !strings.Contains(plan.Seed.Context, "may have reached upstream") {
		t.Error("reconstructed context must represent the uncertain provider attempt")
	}
	if !strings.Contains(plan.Seed.Context, "inspect the workspace") {
		t.Error("reconstructed context must retain the objective")
	}
	// The recovery trace must show the ordered boundaries.
	kinds := traceKinds(*captured)
	joined := strings.Join(kinds, ",")
	for _, want := range []string{
		"recovery_start", "reconcile", "recovery_uncertain", "recovery_context",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("trace missing %q: %s", want, joined)
		}
	}
	if indexOf(kinds, "recovery_start") > indexOf(kinds, "reconcile") {
		t.Error("recovery_start must precede reconciliation")
	}
	if indexOf(kinds, "reconcile") > indexOf(kinds, "recovery_uncertain") {
		t.Error("tool reconciliation must precede provider uncertainty")
	}
	if indexOf(kinds, "recovery_uncertain") > indexOf(kinds, "recovery_context") {
		t.Error("reconciliation must precede context reconstruction")
	}
	// The journal is visible in inspect with the recovery boundaries ordered.
	rendered := mustRender(t, store, fixtureTask)
	if !strings.Contains(rendered, "Resumes: 1") {
		t.Errorf("inspect must show the resume count:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recovery_started") || !strings.Contains(rendered, "tool_attempt_reconciled") ||
		!strings.Contains(rendered, "provider_attempt_reconciled") || !strings.Contains(rendered, "recovery_context_reconstructed") ||
		!strings.Contains(rendered, "recovery_continued") {
		t.Errorf("inspect must show the ordered recovery journal:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recovery=replay_safe_observation") {
		t.Errorf("inspect must render the tool reconciliation reason:\n%s", rendered)
	}
	if !strings.Contains(rendered, "recovery=upstream_may_have_been_reached") {
		t.Errorf("inspect must render the provider reconciliation reason:\n%s", rendered)
	}
}

func TestResumeNoHistoricalProviderCallReplay(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	// One completed provider turn and one completed observation.
	mustProviderCompleted(t, store, fixtureTask, "task-1-0001", 1)
	actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-read-a", "sig-alpha")
	execID := mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)
	mustCompleteTool(t, store, fixtureTask, execID, "obs-000001", "read_file", "alpha\n")

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	// The runtime never persists provider response text, so the reconstructed
	// context cannot contain historical model output. A marker that only ever
	// appeared in the (unpersisted) old conversation must be absent.
	if strings.Contains(plan.Seed.Context, "historical-model-secret-marker") {
		t.Fatal("reconstructed context must not contain historical model output")
	}
	if strings.Contains(plan.Seed.Context, "runstead:assistant") {
		t.Fatal("reconstructed context must not embed the old conversation")
	}
}

func TestResumeReconciledAttemptsAreIdempotent(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-read-a", "sig-alpha")
	mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)

	if _, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask}); err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("second Resume() error = %v", err)
	}
	if plan.ReconciledToolAttempts != 0 {
		t.Errorf("second resume reconciled %d attempts, want 0 (already terminal)", plan.ReconciledToolAttempts)
	}
	if plan.Seed == nil || plan.Seed.Turns != 0 || plan.Seed.Attempts != 0 {
		t.Fatalf("second resume seed = %+v, want zero historical provider attempts", plan.Seed)
	}
	if snapshot := mustLoadSnapshot(t, store, fixtureTask); snapshot.Task.ResumeCount != 2 {
		t.Errorf("resume count = %d, want 2", snapshot.Task.ResumeCount)
	}
}

func TestResumeUncertainProviderAttemptKeepsDebit(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	mustProviderPrepared(t, store, fixtureTask, "task-1-0001", 1)

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.UncertainProviderAttempts != 1 {
		t.Errorf("uncertain provider attempts = %d, want 1", plan.UncertainProviderAttempts)
	}
	// The conservative debit stays visible on the row and in the governor
	// ledger persisted at TX 1.
	for _, attempt := range mustLoadSnapshot(t, store, fixtureTask).ProviderAttempts {
		if attempt.AttemptDebited != 1 || !attempt.Uncertain {
			t.Errorf("reconciled provider attempt must retain debit and uncertainty: %+v", attempt)
		}
	}
	if !strings.Contains(plan.Seed.Context, "conservative debit preserved") {
		t.Error("recovery evidence must represent the retained debit")
	}
}

func TestResumePreparedReplaySafeObservationContinues(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-read-a", "sig-alpha")
	mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionContinue {
		t.Fatalf("decision = %q, want continue", plan.Decision)
	}
	// The prepared observation must NOT seed the guard (it was never executed),
	// so a re-proposal executes as a new attempt with fresh evidence.
	if _, ok := plan.Seed.Guard["fp-read-a"]; ok {
		t.Fatal("prepared observation must not seed the repeat guard")
	}
	if len(plan.Seed.Evidence) != 0 {
		t.Fatal("prepared observation must not produce citable evidence")
	}
	if !strings.Contains(plan.Seed.Context, "replay-safe") {
		t.Error("recovery context must explain the replay-safe reconciliation")
	}
}

func TestResumeHumanReviewRequiredForUnreconcilableEffect(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	// A future class-4 (unreconcilable) interrupted attempt: the read-only
	// runtime cannot safely decide whether its effect occurred, so automatic
	// continuation must stop with a typed human-review outcome.
	actionID := mustAction(t, store, fixtureTask, "write_file", `{"path":"x"}`, "fp-write", "sig-alpha")
	mustToolAttempt(t, store, fixtureTask, actionID, "write_file", `{"path":"x"}`, 4)
	if snapshot := mustLoadSnapshot(t, store, fixtureTask); snapshot.ToolAttempts[0].RecoveryClass != 4 {
		t.Fatalf("fixture recovery class = %d, want 4", snapshot.ToolAttempts[0].RecoveryClass)
	}

	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if plan.Decision != recovery.DecisionHumanReview {
		t.Fatalf("decision = %q, want human_review_required", plan.Decision)
	}
	if plan.Seed != nil {
		t.Fatal("human-review plan must not carry a continuation seed")
	}
	// The task is persisted in the typed human-review state with a reason, and
	// no automatic continuation happened.
	reloaded := mustLoadSnapshot(t, store, fixtureTask)
	if reloaded.Task.Status != "human_review_required" {
		t.Errorf("task status = %q, want human_review_required", reloaded.Task.Status)
	}
	if !strings.Contains(plan.Reason, "human review") {
		t.Errorf("reason = %q, want human-review explanation", plan.Reason)
	}
	for _, attempt := range reloaded.ToolAttempts {
		if attempt.Status != "human_review_required" {
			t.Errorf("unreconcilable attempt status = %q, want human_review_required", attempt.Status)
		}
	}
	// The structured reason is visible in inspect without secrets.
	rendered := mustRender(t, store, fixtureTask)
	if !strings.Contains(rendered, "Status: human_review_required") || !strings.Contains(rendered, "task_human_review_required") {
		t.Errorf("inspect must expose the human-review outcome:\n%s", rendered)
	}
}

func TestResumeContextBudgetIsBounded(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	// Substantial history: 40 completed observations with large content.
	for index := 1; index <= 40; index++ {
		actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-a", "sig-alpha")
		execID := mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)
		content := strings.Repeat("x", 2000) + fmt.Sprint(index%10)
		mustCompleteTool(t, store, fixtureTask, execID, fmt.Sprintf("obs-%06d", index), "read_file", content)
	}

	budget := recovery.Budget{MaxContextBytes: 4096, MaxObservationCount: 2, MaxObservationChars: 128, MaxFailureLines: 8, MaxUncertainLines: 4}
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask, Budget: budget})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if len(plan.Seed.Context) > budget.MaxContextBytes {
		t.Fatalf("context = %d bytes, want <= %d", len(plan.Seed.Context), budget.MaxContextBytes)
	}
	// Required evidence IDs survive even under a tight budget: the newest
	// observation ID and the oldest one are both listed.
	if !strings.Contains(plan.Seed.Context, "obs-000040") {
		t.Error("context must retain the newest evidence id")
	}
	if !strings.Contains(plan.Seed.Context, "obs-000001") {
		t.Error("context must retain the oldest evidence id (evidence must never be silently dropped)")
	}
	// The hard truncation carries an explicit marker.
	if len(plan.Seed.Context) == budget.MaxContextBytes && !strings.Contains(plan.Seed.Context, "truncated to budget") {
		t.Error("hard truncation must carry an explicit marker")
	}
}

func TestResumeGovernorBlocksContinuation(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	mustProviderPrepared(t, store, fixtureTask, "task-1-0001", 1)

	// Restore a governor whose cooldown is in the future: continuation is
	// blocked by account protection until the cooldown clears.
	persisted, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = %v, %v", ok, err)
	}
	persisted.CooldownUntil = time.Now().Add(time.Hour)
	accountConfig := governor.DefaultInstantConfig("runstead-cli", "scripted", "instant", provider.SafeRouteSafety())
	accountGovernor, err := governor.New(accountConfig, governor.Options{Restore: &persisted})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}

	blocked := false
	plan, err := recovery.Resume(context.Background(), store, recovery.Options{
		TaskID: fixtureTask,
		Blocked: func() (bool, string) {
			blocked, _ = recovery.GovernorBlocks(accountGovernor, fixtureTask)
			return blocked, "account cooldown active until later"
		},
	})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !blocked {
		t.Fatal("GovernorBlocks must report the restored cooldown")
	}
	if plan.Decision != recovery.DecisionBlocked {
		t.Fatalf("decision = %q, want governor_blocked", plan.Decision)
	}
	if plan.Seed != nil {
		t.Fatal("blocked plan must not carry a continuation seed")
	}
	// The task stays pending (running) with a journaled recovery_blocked
	// decision; it is not finalized.
	if snapshot := mustLoadSnapshot(t, store, fixtureTask); snapshot.Task.Status != "running" {
		t.Errorf("task status = %q, want running (pending)", snapshot.Task.Status)
	}
	rendered := mustRender(t, store, fixtureTask)
	if !strings.Contains(rendered, "recovery_blocked") {
		t.Errorf("journal must contain recovery_blocked:\n%s", rendered)
	}
	if strings.Contains(rendered, "recovery_continued") {
		t.Errorf("blocked resume must not journal recovery_continued:\n%s", rendered)
	}
}

// TestResumeGovernorBudgetBlockedSurvivesRestart covers the remaining
// account-protection block branches in the resume pre-check: a persisted task
// budget or rolling budget at its ceiling must block continuation after
// restart, complementing the cooldown and circuit tests.
func TestResumeGovernorBudgetBlockedSurvivesRestart(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		state     governor.PersistedState
		wantBlock string
	}{
		{
			name: "task budget exhausted",
			state: governor.PersistedState{
				AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
				AllowanceProfile: governor.ProfileInstant, NextAttempt: 81,
				Circuit:    governor.CircuitSnapshot{State: governor.CircuitClosed},
				Ceilings:   governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
				TaskStates: []governor.TaskStateRecord{{TaskID: fixtureTask, Attempts: 80, Retries: 0, LastTouched: now}},
			},
			wantBlock: "task provider budget exhausted",
		},
		{
			name: "rolling 3h budget exhausted",
			state: governor.PersistedState{
				AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
				AllowanceProfile: governor.ProfileInstant, NextAttempt: 141,
				Circuit:  governor.CircuitSnapshot{State: governor.CircuitClosed},
				Ceilings: governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
			},
			wantBlock: "rolling 3h provider budget exhausted",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// The rolling case needs 140 ledger events inside the 3h window.
			if strings.Contains(testCase.name, "rolling") {
				for index := 0; index < 140; index++ {
					testCase.state.RollingEvents = append(testCase.state.RollingEvents,
						governor.LedgerEvent{At: now.Add(-time.Duration(index) * time.Second), TaskID: fixtureTask})
				}
			}
			store := openStore(t)
			mustCreate(t, store, fixtureTask)
			accountConfig := governor.DefaultInstantConfig("runstead-cli", "scripted", "instant", provider.SafeRouteSafety())
			accountGovernor, err := governor.New(accountConfig, governor.Options{Restore: &testCase.state})
			if err != nil {
				t.Fatalf("governor.New() error = %v", err)
			}
			blocked, reason := recovery.GovernorBlocks(accountGovernor, fixtureTask)
			if !blocked {
				t.Fatal("GovernorBlocks must report the restored budget block")
			}
			if !strings.Contains(reason, testCase.wantBlock) {
				t.Fatalf("block reason = %q, want %q", reason, testCase.wantBlock)
			}
			plan, err := recovery.Resume(context.Background(), store, recovery.Options{
				TaskID: fixtureTask,
				Blocked: func() (bool, string) {
					return recovery.GovernorBlocks(accountGovernor, fixtureTask)
				},
			})
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if plan.Decision != recovery.DecisionBlocked {
				t.Fatalf("decision = %q, want governor_blocked", plan.Decision)
			}
			if plan.Seed != nil {
				t.Fatal("blocked plan must not carry a continuation seed")
			}
			rendered := mustRender(t, store, fixtureTask)
			if !strings.Contains(rendered, "recovery_blocked") {
				t.Errorf("journal must contain recovery_blocked:\n%s", rendered)
			}
			if strings.Contains(rendered, "recovery_continued") {
				t.Errorf("blocked resume must not journal recovery_continued:\n%s", rendered)
			}
		})
	}
}

func TestResumeTaskNotFoundAndTerminal(t *testing.T) {
	store := openStore(t)
	if _, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: "missing"}); err == nil {
		t.Fatal("Resume() for a missing task must fail")
	}
	mustCreate(t, store, fixtureTask)
	if err := store.FinalizeTask(context.Background(), state.TaskFinalize{
		TaskID: fixtureTask, Outcome: "completed", StopReason: "grounded final accepted", Summary: "done",
	}); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	if _, err := recovery.Resume(context.Background(), store, recovery.Options{TaskID: fixtureTask}); err == nil {
		t.Fatal("Resume() for a completed task must fail")
	}
}

func TestResumeContextCancellationPropagates(t *testing.T) {
	store := openStore(t)
	mustCreate(t, store, fixtureTask)
	actionID := mustAction(t, store, fixtureTask, "read_file", `{"path":"a.txt"}`, "fp-a", "sig-alpha")
	mustToolAttempt(t, store, fixtureTask, actionID, "read_file", `{"path":"a.txt"}`, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recovery.Resume(ctx, store, recovery.Options{TaskID: fixtureTask}); err == nil {
		t.Fatal("Resume() must propagate a canceled context")
	}
}

func TestResumeCredentialShapedStateIsRedacted(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	// A credential-shaped objective must never reach the reconstructed context
	// or the inspect output: the same redaction boundary as normal persisted
	// state applies to recovery context.
	if err := store.CreateTask(ctx, state.TaskRecord{
		TaskID: fixtureTask, Objective: "inspect Bearer sk-0123456789abcdef", Workspace: "/ws", Model: "scripted",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, fixtureTask); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	plan, err := recovery.Resume(ctx, store, recovery.Options{TaskID: fixtureTask})
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	// The raw credential bytes must never reach the reconstructed context; the
	// redaction marker must. (ContainsCredentialShape on the whole context is
	// too coarse here: removing the marker can create "Bearer \nObjective"
	// cross-line false positives, so assert the exact secret and the marker.)
	for _, secret := range []string{"sk-0123456789abcdef", "Bearer sk-"} {
		if strings.Contains(plan.Seed.Context, secret) {
			t.Fatalf("reconstructed context leaked %q", secret)
		}
	}
	if !strings.Contains(plan.Seed.Context, "Bearer <redacted>") {
		t.Error("reconstructed context must retain the redaction marker")
	}
	rendered := mustRender(t, store, fixtureTask)
	for _, secret := range []string{"sk-0123456789abcdef", "Bearer sk-"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("inspect output leaked %q", secret)
		}
	}
}
