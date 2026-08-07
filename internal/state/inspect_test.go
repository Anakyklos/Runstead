package state

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Inspect tests: RenderInspect must reconstruct a task from persisted state
// with stable, sanitized, human-readable output.

func TestRenderInspectReconstructsCompletedTask(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-1", Tool: "read_file",
		Arguments: []byte(`{"path":"a.txt"}`), Fingerprint: "fp",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	mustToolAttempt(t, store, "task-1", actionID)
	mustProviderAttempt(t, store, "task-1", "task-1-0001", 1)
	mustFinalize(t, store, "task-1")

	var out bytes.Buffer
	if err := store.RenderInspect(ctx, &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		"Task: task-1",
		"Objective: inspect the workspace",
		"Status: completed",
		"Outcome: completed",
		"Stop reason: grounded final accepted",
		"Workspace: /tmp/ws",
		"Model: scripted",
		"Configuration:",
		"Events:",
		"1. task_created",
		"task_finalized",
		"Actions:",
		actionID + " read_file status=completed",
		"Tool attempts:",
		"evidence=obs-000001",
		"Provider attempts:",
		"request=task-1-0001",
		"outcome=success upstream_reached=true",
		"Governor state:",
		"rolling usage:",
		"circuit: closed",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspect output missing %q:\n%s", want, rendered)
		}
	}
	// Output must be stable: two renders of the same task are identical.
	var again bytes.Buffer
	if err := store.RenderInspect(ctx, &again, "task-1"); err != nil {
		t.Fatalf("second RenderInspect() error = %v", err)
	}
	if again.String() != rendered {
		t.Fatal("inspect output is not deterministic")
	}
}

func TestRenderInspectUnknownTask(t *testing.T) {
	store := openTestStore(t)
	var out bytes.Buffer
	err := store.RenderInspect(context.Background(), &out, "missing-task")
	if err != ErrTaskNotFound {
		t.Fatalf("RenderInspect(missing) error = %v, want ErrTaskNotFound", err)
	}
}

func TestRenderInspectFlagsUncertainAndPreparedStates(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	actionID, err := store.RecordAction(ctx, ActionRecord{TaskID: "task-1", Tool: "read_file", Arguments: []byte(`{}`)})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	// A tool attempt left prepared (crash window) must be flagged.
	if _, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-1", ActionID: actionID, Tool: "read_file", Arguments: []byte(`{}`), RecoveryClass: 1,
	}); err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	// An uncertain provider attempt must be flagged and never reinterpreted.
	state := governor.PersistedState{
		AccountPolicyID: "p", ProviderID: "scripted", ModelPool: "instant",
		Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
	}
	if err := store.RecordProviderPrepared(ctx, governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AttemptSequence: 1, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.RecordProviderFinished(ctx, governor.ProviderFinished{
		TaskID: "task-1", ClientRequestID: "task-1-0001",
		Outcome: governor.OutcomeUncertainReached, UpstreamReached: true,
		Uncertain: true, AttemptDebited: 1,
		Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed}, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}

	var out bytes.Buffer
	if err := store.RenderInspect(ctx, &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "uncertain=prepared: the effect may have started") {
		t.Errorf("inspect must flag prepared tool attempts:\n%s", rendered)
	}
	if !strings.Contains(rendered, "uncertain=yes: the upstream may have been reached") {
		t.Errorf("inspect must flag uncertain provider attempts:\n%s", rendered)
	}
	if !strings.Contains(rendered, "status=uncertain") {
		t.Errorf("inspect must show the uncertain status:\n%s", rendered)
	}
}

func TestRenderInspectShowsGovernorConsumption(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-1")
	// Drive a real governor so the persisted projection accumulates.
	accountGovernor := newGovernor(t, store, nil, 80)
	client := fakeResponses(2)
	for turn := 1; turn <= 2; turn++ {
		result := accountGovernor.Execute(ctx, governor.AttemptRequest{
			TaskID:          "task-1",
			ClientRequestID: "task-1-000" + string(rune('0'+turn)),
			ProviderRequest: provider.Request{Prompt: "p", Model: "scripted"},
		}, client, nil)
		if !result.Admission.Admitted() || result.Err != nil {
			t.Fatalf("execution %d failed: %#v", turn, result)
		}
	}

	var out bytes.Buffer
	if err := store.RenderInspect(ctx, &out, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "rolling usage: 10m=2/25 1h=2/80 3h=2/140") {
		t.Errorf("inspect must show windowed governor consumption:\n%s", rendered)
	}
	if !strings.Contains(rendered, "cooldown until: none") {
		t.Errorf("inspect must render empty cooldown as none:\n%s", rendered)
	}
}
