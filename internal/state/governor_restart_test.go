package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Governor durability tests: restarting the process must not reset account
// protection (#21). The rolling ledger, cooldown, circuit and retained
// accounting state are restored from the persisted projection.

// newGovernor builds a governor wired to a store with a small task budget so
// protection is observable within a few executions.
func newGovernor(t *testing.T, store *Store, restore *governor.PersistedState, taskBudget int) *governor.Governor {
	t.Helper()
	config := governor.DefaultInstantConfig("policy-restart", "scripted", "instant", provider.SafeRouteSafety())
	config.TaskBudget = taskBudget
	config.MinimumStartInterval = time.Millisecond
	accountGovernor, err := governor.New(config, governor.Options{
		Persistence: store,
		Restore:     restore,
	})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	return accountGovernor
}

// mustGovernorTask creates the durable task row a governed execution
// references (the runtime loop does this before the first attempt).
func mustGovernorTask(t *testing.T, store *Store) {
	t.Helper()
	mustTask(t, store, "task-1")
}

// fakeResponses builds a fake provider with count identical responses so
// every governed execution consumes one.
func fakeResponses(count int) *provider.Fake {
	responses := make([]provider.Response, count)
	for index := range responses {
		responses[index] = provider.Response{Text: "ok"}
	}
	return provider.NewFake(responses...)
}

func executeOnce(t *testing.T, accountGovernor *governor.Governor, client provider.Client, classifier governor.OutcomeClassifier) governor.ExecutionResult {
	t.Helper()
	return accountGovernor.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "task-1-0001",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, client, classifier)
}

func TestGovernorRollingUsageSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(3)
	mustGovernorTask(t, store)

	accountGovernor := newGovernor(t, store, nil, 80)
	for turn := 1; turn <= 3; turn++ {
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("execution %d failed: %#v", turn, result)
		}
	}

	restoredState, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = ok %t err %v", ok, err)
	}
	if len(restoredState.RollingEvents) != 3 {
		t.Fatalf("persisted rolling events = %d, want 3", len(restoredState.RollingEvents))
	}

	restored := newGovernor(t, store, &restoredState, 80)
	snapshot := restored.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 3 || snapshot.Budgets.Rolling1hUsed != 3 || snapshot.Budgets.Rolling10mUsed != 3 {
		t.Fatalf("restored rolling usage = %#v, want 3/3/3", snapshot.Budgets)
	}
	if snapshot.NextAttempt != 4 {
		t.Fatalf("restored next attempt = %d, want 4", snapshot.NextAttempt)
	}
	if restored.PersistedState().AccountPolicyID != "policy-restart" {
		t.Fatal("restored governor must retain its identity")
	}
}

func TestGovernorCooldownSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(1)
	classifier := func(provider.Response, error) governor.Outcome {
		return governor.Outcome{Class: governor.OutcomeRateCapacity, UpstreamReached: true}
	}
	mustGovernorTask(t, store)

	accountGovernor := newGovernor(t, store, nil, 80)
	result := executeOnce(t, accountGovernor, client, classifier)
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("completion outcome = %s, want rate_or_capacity", result.Completion.Outcome)
	}
	if accountGovernor.Snapshot().CooldownUntil.IsZero() {
		t.Fatal("governor must enter cooldown after a rate response")
	}

	restoredState, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatalf("GovernorState() error = %v", err)
	}
	restored := newGovernor(t, store, &restoredState, 80)
	admission := restored.TryAdmit(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-0002",
	})
	// tryAdmit surfaces a future cooldown as delayed with the cooldown reason.
	if admission.Code != governor.AdmissionDelayed || admission.Reason != governor.AdmissionCooldownActive {
		t.Fatalf("restored admission = %s (reason %s), want delayed/cooldown_active", admission.Code, admission.Reason)
	}
}

func TestGovernorCircuitSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(1)
	classifier := func(provider.Response, error) governor.Outcome {
		return governor.Outcome{Class: governor.OutcomeAuthenticationDenied, UpstreamReached: true}
	}
	mustGovernorTask(t, store)

	accountGovernor := newGovernor(t, store, nil, 80)
	result := executeOnce(t, accountGovernor, client, classifier)
	if result.Completion.Circuit.State != governor.CircuitHumanReviewRequired {
		t.Fatalf("circuit = %s, want human_review_required", result.Completion.Circuit.State)
	}

	restoredState, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatalf("GovernorState() error = %v", err)
	}
	restored := newGovernor(t, store, &restoredState, 80)
	admission := restored.TryAdmit(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-0002",
	})
	if admission.Code != governor.AdmissionHumanAcknowledgementRequired {
		t.Fatalf("restored admission = %s, want human_acknowledgement_required", admission.Code)
	}
}

func TestGovernorTaskBudgetSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(2)
	mustGovernorTask(t, store)

	accountGovernor := newGovernor(t, store, nil, 2)
	for turn := 1; turn <= 2; turn++ {
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() {
			t.Fatalf("execution %d not admitted: %#v", turn, result.Admission)
		}
	}
	if client.Attempts() != 2 {
		t.Fatalf("provider attempts = %d, want 2", client.Attempts())
	}

	restoredState, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatalf("GovernorState() error = %v", err)
	}
	restored := newGovernor(t, store, &restoredState, 2)
	// A fresh client proves the provider is never reached after restart.
	freshClient := fakeResponses(1)
	result := restored.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "task-1-0003",
		ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
	}, freshClient, nil)
	if result.Admission.Code != governor.AdmissionTaskBudgetExhausted {
		t.Fatalf("restored admission = %s, want task_budget_exhausted", result.Admission.Code)
	}
	if freshClient.Attempts() != 0 {
		t.Fatalf("provider reached after restart: attempts = %d, want 0", freshClient.Attempts())
	}
}

func TestGovernorRequestDedupSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(1)
	mustGovernorTask(t, store)
	accountGovernor := newGovernor(t, store, nil, 80)
	result := executeOnce(t, accountGovernor, client, nil)
	if !result.Admission.Admitted() || result.Err != nil {
		t.Fatalf("execution failed: %#v", result)
	}

	restoredState, _, err := store.GovernorState(context.Background())
	if err != nil {
		t.Fatalf("GovernorState() error = %v", err)
	}
	restored := newGovernor(t, store, &restoredState, 80)
	duplicate := restored.TryAdmit(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-0001",
	})
	if duplicate.Code != governor.AdmissionDuplicateClientRequest {
		t.Fatalf("restored duplicate admission = %s, want duplicate_client_request", duplicate.Code)
	}
}

// --- Subprocess restart proof ---

// TestGovernorRestartHelper is the subprocess entry point. Mode "run"
// executes two governed attempts; mode "prove" restores the persisted state
// and reports the admission outcome for a third attempt.
func TestGovernorRestartHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_GOVERNOR_RESTART_HELPER") == "" {
		t.Skip("governor restart helper")
	}
	dbPath := os.Getenv("RUNSTEAD_GOVERNOR_RESTART_DB")
	mode := os.Getenv("RUNSTEAD_GOVERNOR_RESTART_MODE")
	store, err := Open(Options{Path: dbPath})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	config := governor.DefaultInstantConfig("policy-restart", "scripted", "instant", provider.SafeRouteSafety())
	config.TaskBudget = 2
	config.MinimumStartInterval = time.Millisecond
	var restore *governor.PersistedState
	if mode == "prove" {
		snapshot, ok, err := store.GovernorState(context.Background())
		if err != nil {
			t.Fatalf("GovernorState() error = %v", err)
		}
		if ok {
			restore = &snapshot
		}
	}
	accountGovernor, err := governor.New(config, governor.Options{Persistence: store, Restore: restore})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}

	client := fakeResponses(2)
	switch mode {
	case "run":
		if err := store.CreateTask(context.Background(), TaskRecord{TaskID: "task-1", Objective: "o", Workspace: "w"}); err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if err := store.StartTask(context.Background(), "task-1"); err != nil {
			t.Fatalf("StartTask() error = %v", err)
		}
		for turn := 1; turn <= 2; turn++ {
			result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
				TaskID:          "task-1",
				ClientRequestID: "task-1-000" + string(rune('0'+turn)),
				ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
			}, client, nil)
			if !result.Admission.Admitted() {
				t.Fatalf("execution %d not admitted: %#v", turn, result.Admission)
			}
		}
	case "prove":
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-0003",
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		snapshot := accountGovernor.Snapshot()
		output := os.Getenv("RUNSTEAD_GOVERNOR_RESTART_OUTPUT")
		report := "admission=" + string(result.Admission.Code) +
			" rolling=" + strconv.Itoa(snapshot.Budgets.Rolling3hUsed) +
			" attempts=" + strconv.Itoa(client.Attempts())
		if err := os.WriteFile(output, []byte(report), 0o600); err != nil {
			t.Fatalf("write report: %v", err)
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

func TestGovernorProtectionSurvivesSubprocessRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "runstead.db")
	report := filepath.Join(dir, "report.txt")

	runHelper := func(mode string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=TestGovernorRestartHelper")
		cmd.Env = append(os.Environ(),
			"RUNSTEAD_GOVERNOR_RESTART_HELPER=1",
			"RUNSTEAD_GOVERNOR_RESTART_DB="+dbPath,
			"RUNSTEAD_GOVERNOR_RESTART_MODE="+mode,
			"RUNSTEAD_GOVERNOR_RESTART_OUTPUT="+report,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("helper %s failed: %v\n%s", mode, err, output)
		}
	}

	runHelper("run")
	runHelper("prove")

	content, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(content), "admission=task_budget_exhausted") {
		t.Fatalf("restarted process must refuse admission: %s", content)
	}
	if !strings.Contains(string(content), "rolling=2") {
		t.Fatalf("restarted process must retain the rolling ledger: %s", content)
	}
	if !strings.Contains(string(content), "attempts=0") {
		t.Fatalf("restarted process must not reach the provider: %s", content)
	}
}
