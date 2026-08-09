package state

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Issue #58 durability: the unlimited-text allowance is a policy semantic,
// never a fresh allowance ledger. The SQLite projection stores the profile
// (and derives the typed kind), the manual reserve column is profile-specific
// and zero for non-numeric allowances, and transitions between allowance
// kinds must not reset rolling usage, task attempts, cooldown or circuit
// state.

func newUnlimitedGovernor(t *testing.T, store *Store, restore *governor.PersistedState) *governor.Governor {
	t.Helper()
	config := governor.DefaultLunaUnlimitedTextConfig("policy-unlimited", "scripted", "instant", provider.SafeRouteSafety())
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

func TestGovernorUnlimitedStateSurvivesRestart(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(3)
	mustGovernorTask(t, store)

	accountGovernor := newUnlimitedGovernor(t, store, nil)
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
	if restoredState.AllowanceProfile != governor.ProfileLunaUnlimitedText || restoredState.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("persisted allowance = profile %q kind %q, want luna unlimited", restoredState.AllowanceProfile, restoredState.AllowanceKind)
	}
	if len(restoredState.RollingEvents) != 3 {
		t.Fatalf("persisted rolling events = %d, want 3", len(restoredState.RollingEvents))
	}
	if restoredState.Ceilings.Rolling3h != 0 || restoredState.Ceilings.ManualReserve != 0 {
		t.Fatalf("persisted unlimited ceilings = %#v, want no fabricated numeric allowance", restoredState.Ceilings)
	}
	if restoredState.Ceilings.TaskBudget != 80 {
		t.Fatalf("persisted task ceiling = %d, want 80 (local workload control)", restoredState.Ceilings.TaskBudget)
	}

	restored := newUnlimitedGovernor(t, store, &restoredState)
	snapshot := restored.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 3 || snapshot.Budgets.Rolling1hUsed != 3 || snapshot.Budgets.Rolling10mUsed != 3 {
		t.Fatalf("restored unlimited usage = %#v, want 3/3/3", snapshot.Budgets)
	}
	if snapshot.AllowanceKind != governor.AllowanceKindUnlimitedText || snapshot.NextAttempt != 4 {
		t.Fatalf("restored snapshot = %#v", snapshot)
	}
}

func TestGovernorAllowanceKindTransitionSurvivesPersistence(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(4)
	mustGovernorTask(t, store)

	instant := newGovernor(t, store, nil, 80)
	for turn := 1; turn <= 3; turn++ {
		result := instant.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("instant execution %d failed: %#v", turn, result)
		}
	}
	classifier := func(provider.Response, error) governor.Outcome {
		return governor.Outcome{Class: governor.OutcomeRateCapacity, RetryAfter: 15 * time.Second, UpstreamReached: true}
	}
	rateResult := instant.Execute(context.Background(), governor.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "task-1-0004",
		ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
	}, client, classifier)
	if rateResult.Err != nil || rateResult.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("rate execution = %#v", rateResult)
	}

	persisted, ok, err := store.GovernorState(context.Background())
	if err != nil || !ok {
		t.Fatalf("GovernorState() = ok %t err %v", ok, err)
	}
	if persisted.AllowanceProfile != governor.ProfileInstant || persisted.AllowanceKind != governor.AllowanceKindPublishedQuota {
		t.Fatalf("persisted instant allowance = profile %q kind %q", persisted.AllowanceProfile, persisted.AllowanceKind)
	}
	if persisted.Ceilings.ManualReserve != 20 {
		t.Fatalf("persisted instant reserve = %d, want 20", persisted.Ceilings.ManualReserve)
	}

	// Transition published_quota -> unlimited_text: the durable ledger, task
	// attempts, cooldown and circuit must all carry over unchanged.
	unlimited := newUnlimitedGovernor(t, store, &persisted)
	snapshot := unlimited.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 4 || snapshot.Budgets.Rolling10mUsed != 4 {
		t.Fatalf("kind transition reset the ledger: %#v", snapshot.Budgets)
	}
	if task := snapshot.Tasks["task-1"]; task.Attempts != 4 {
		t.Fatalf("kind transition reset task attempts: %#v", task)
	}
	if snapshot.CooldownUntil.IsZero() {
		t.Fatal("kind transition reset the cooldown state")
	}
	if snapshot.AllowanceKind != governor.AllowanceKindUnlimitedText {
		t.Fatalf("transitioned snapshot kind = %q, want unlimited_text", snapshot.AllowanceKind)
	}
}

func TestInspectDistinguishesUpstreamAllowanceFromLocalCeilings(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(2)
	mustGovernorTask(t, store)

	accountGovernor := newUnlimitedGovernor(t, store, nil)
	for turn := 1; turn <= 2; turn++ {
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("execution %d failed: %#v", turn, result)
		}
	}

	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"upstream allowance: unlimited_text profile=luna_unlimited_text",
		"no published numeric rolling quota (explicitly configured unlimited text)",
		"local workload ceilings: task=80 retry=2",
		"serialized lane, start-to-start pacing, queue/fairness, cooldown and circuit breakers remain active for every allowance kind",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect output missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "rolling usage:") {
		t.Errorf("inspect must not print a fabricated rolling quota for unlimited text:\n%s", rendered)
	}
	if strings.Contains(rendered, "manual reserve:") {
		t.Errorf("inspect must not print a manual reserve for unlimited text:\n%s", rendered)
	}
}

func TestInspectShowsPublishedQuotaReserveAndLocalCeilings(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(2)
	mustGovernorTask(t, store)

	accountGovernor := newGovernor(t, store, nil, 80)
	for turn := 1; turn <= 2; turn++ {
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("execution %d failed: %#v", turn, result)
		}
	}

	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"upstream allowance: published_quota profile=plus_go_instant",
		"rolling usage: 10m=2/25 1h=2/80 3h=2/140",
		"manual reserve: 20 (20 remaining)",
		"local workload ceilings: task=80 retry=2",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect output missing %q:\n%s", want, rendered)
		}
	}
}

func TestInspectShowsUnknownUpstreamWithLocalConservativeCeilings(t *testing.T) {
	store := openTestStore(t)
	client := fakeResponses(2)
	mustGovernorTask(t, store)

	config := governor.DefaultUnknownConfig("policy-unknown", "scripted", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Millisecond
	accountGovernor, err := governor.New(config, governor.Options{Persistence: store})
	if err != nil {
		t.Fatalf("governor.New() error = %v", err)
	}
	for turn := 1; turn <= 2; turn++ {
		result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("execution %d failed: %#v", turn, result)
		}
	}

	var out bytes.Buffer
	if err := store.RenderInspect(context.Background(), &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"upstream allowance: unknown profile=unknown",
		"no published numeric rolling quota (no evidence; explicit local conservative ceilings still enforced)",
		"rolling usage: 10m=2/25 1h=2/80 3h=2/140",
		"manual reserve: 20 (20 remaining)",
		"local workload ceilings: task=80 retry=2",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect output missing %q:\n%s", want, rendered)
		}
	}
}
