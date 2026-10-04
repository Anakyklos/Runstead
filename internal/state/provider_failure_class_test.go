package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

type providerFailureClient struct {
	err   error
	calls *int
}

func (c providerFailureClient) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (c providerFailureClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	if c.calls != nil {
		(*c.calls)++
	}
	return provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliverySentUnconfirmed}}, c.err
}

func TestTypedProviderFailureSurvivesConservativeDeliveryClassification(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runstead.db")
	store, err := Open(Options{Path: dbPath, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mustGovernorTask(t, store)
	g := newGovernor(t, store, nil, 80)
	calls := 0
	result := g.Execute(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-0001",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, providerFailureClient{calls: &calls, err: &openaicompat.Error{
		Kind: openaicompat.ErrorTimeout, Cause: errors.Join(context.DeadlineExceeded, errors.New("SECRET_DO_NOT_PERSIST")),
		DeliveryState: provider.DeliverySentUnconfirmed, UpstreamReached: true,
	}}, compat.NewClassifier())
	if result.Completion.Outcome != governor.OutcomeUncertainReached || result.Completion.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("conservative delivery result = outcome %q, delivery %q", result.Completion.Outcome, result.Completion.DeliveryState)
	}
	if result.Completion.AttemptDebited != 1 || result.Completion.RetryEligible {
		t.Fatalf("governor accounting/retry changed: %+v", result.Completion)
	}
	replay := g.Execute(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-0001",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, providerFailureClient{calls: &calls, err: result.Err}, compat.NewClassifier())
	if replay.Admission.Code != governor.AdmissionDuplicateClientRequest || calls != 1 {
		t.Fatalf("sent_unconfirmed replay = admission %q, provider calls %d; want duplicate rejection and one call", replay.Admission.Code, calls)
	}

	var failureClass, receiptError string
	if err := store.db.QueryRow(`SELECT provider_failure_class, error_class FROM provider_attempts WHERE task_id = ? AND client_request_id = ?`,
		"task-1", "task-1-0001").Scan(&failureClass, &receiptError); err != nil {
		t.Fatalf("load durable provider failure: %v", err)
	}
	if failureClass != "timeout" || receiptError != "" {
		t.Fatalf("durable classes = provider %q receipt %q, want timeout and empty", failureClass, receiptError)
	}
	var events int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE task_id = ? AND payload_json LIKE '%"provider_failure_class":"timeout"%'`, "task-1").Scan(&events); err != nil {
		t.Fatalf("query event evidence: %v", err)
	}
	if events != 1 {
		t.Fatalf("provider failure class journal events = %d, want 1", events)
	}
	var inspect strings.Builder
	if err := store.RenderInspect(context.Background(), &inspect, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	if !strings.Contains(inspect.String(), "provider_failure_class=timeout") {
		t.Fatalf("inspect missing provider failure class:\n%s", inspect.String())
	}
	if strings.Contains(inspect.String(), "SECRET_DO_NOT_PERSIST") {
		t.Fatal("inspect persisted provider error cause text")
	}
	var journal strings.Builder
	for _, event := range mustLoadEvents(t, store, "task-1") {
		journal.WriteString(event.Payload)
	}
	if strings.Contains(journal.String(), "SECRET_DO_NOT_PERSIST") {
		t.Fatal("event journal persisted provider error cause text")
	}
	var persisted string
	if err := store.db.QueryRow(`SELECT provider_failure_class || error_class FROM provider_attempts WHERE task_id = ?`, "task-1").Scan(&persisted); err != nil {
		t.Fatalf("read persisted classes: %v", err)
	}
	if strings.Contains(persisted, "SECRET_DO_NOT_PERSIST") {
		t.Fatal("provider attempt persisted provider error cause text")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	reopened, err := Open(Options{Path: dbPath, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	inspect.Reset()
	if err := reopened.RenderInspect(context.Background(), &inspect, "task-1"); err != nil {
		t.Fatalf("RenderInspect() after reopen: %v", err)
	}
	if !strings.Contains(inspect.String(), "provider_failure_class=timeout") || strings.Contains(inspect.String(), "SECRET_DO_NOT_PERSIST") {
		t.Fatalf("reopened inspect did not preserve only safe failure evidence:\n%s", inspect.String())
	}
}

func TestUnknownProviderErrorRemainsUnclassifiedAndUnpersisted(t *testing.T) {
	store := openTestStore(t)
	mustGovernorTask(t, store)
	g := newGovernor(t, store, nil, 80)
	result := g.Execute(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-unknown",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, providerFailureClient{err: errors.New("SECRET_DO_NOT_PERSIST")}, compat.NewClassifier())
	if result.Completion.Outcome != governor.OutcomeUncertainReached || result.Completion.AttemptDebited != 1 {
		t.Fatalf("unknown provider outcome/accounting = %+v", result.Completion)
	}
	var failureClass string
	if err := store.db.QueryRow(`SELECT provider_failure_class FROM provider_attempts WHERE task_id = ?`, "task-1").Scan(&failureClass); err != nil {
		t.Fatalf("read unknown provider failure class: %v", err)
	}
	if failureClass != "" {
		t.Fatalf("unknown error persisted as class %q", failureClass)
	}
	var inspect strings.Builder
	if err := store.RenderInspect(context.Background(), &inspect, "task-1"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	var journal strings.Builder
	for _, event := range mustLoadEvents(t, store, "task-1") {
		journal.WriteString(event.Payload)
	}
	if strings.Contains(inspect.String(), "SECRET_DO_NOT_PERSIST") || strings.Contains(journal.String(), "SECRET_DO_NOT_PERSIST") {
		t.Fatal("unknown raw provider error was persisted")
	}
}

func TestTypedTransportFailureSurvivesAmbiguousDelivery(t *testing.T) {
	store := openTestStore(t)
	mustGovernorTask(t, store)
	g := newGovernor(t, store, nil, 80)
	result := g.Execute(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-transport",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, providerFailureClient{err: &openaicompat.Error{
		Kind: openaicompat.ErrorTransport, Cause: errors.New("SECRET_DO_NOT_PERSIST"),
		DeliveryState: provider.DeliverySentUnconfirmed, UpstreamReached: true,
	}}, compat.NewClassifier())
	if result.Completion.Outcome != governor.OutcomeUncertainReached || result.Completion.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("conservative transport result = outcome %q, delivery %q", result.Completion.Outcome, result.Completion.DeliveryState)
	}
	if result.Completion.AttemptDebited != 1 || result.Completion.RetryEligible {
		t.Fatalf("transport accounting/retry changed: %+v", result.Completion)
	}
	var failureClass string
	if err := store.db.QueryRow(`SELECT provider_failure_class FROM provider_attempts WHERE task_id = ? AND client_request_id = ?`,
		"task-1", "task-1-transport").Scan(&failureClass); err != nil {
		t.Fatalf("load durable transport class: %v", err)
	}
	if failureClass != "transport" {
		t.Fatalf("durable transport class = %q, want transport", failureClass)
	}
}

func TestReceiptValidationCodeKeepsItsExistingField(t *testing.T) {
	store := openTestStore(t)
	mustGovernorTask(t, store)
	state := governor.PersistedState{AccountPolicyID: "policy-test", ProviderID: "scripted"}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-receipt", ProviderID: "scripted",
		ModelPool: "pool", Model: "scripted", AttemptSequence: 1, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	if err := store.RecordProviderFinished(context.Background(), governor.ProviderFinished{
		TaskID: "task-1", ClientRequestID: "task-1-receipt", Outcome: governor.OutcomeUncertainReached,
		Uncertain: true, UpstreamReached: true, DeliveryState: provider.DeliverySentUnconfirmed,
		ProviderFailureClass: provider.ProviderFailureClass("SECRET_DO_NOT_PERSIST"),
		ReceiptErrorCode:     "malformed", AttemptDebited: 1, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
	var failureClass, receiptError string
	if err := store.db.QueryRow(`SELECT provider_failure_class, error_class FROM provider_attempts WHERE task_id = ?`, "task-1").Scan(&failureClass, &receiptError); err != nil {
		t.Fatalf("read receipt error fields: %v", err)
	}
	if failureClass != "" || receiptError != "malformed" {
		t.Fatalf("provider failure/receipt errors = %q/%q, want empty/malformed", failureClass, receiptError)
	}
}

func TestSuccessHasNoProviderFailureClass(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttempt(t, store, "task-1", "task-1-success", 1)
	var failureClass string
	if err := store.db.QueryRow(`SELECT provider_failure_class FROM provider_attempts WHERE task_id = ? AND client_request_id = ?`,
		"task-1", "task-1-success").Scan(&failureClass); err != nil {
		t.Fatalf("read successful provider failure class: %v", err)
	}
	if failureClass != "" {
		t.Fatalf("success provider failure class = %q, want empty", failureClass)
	}
}

func mustLoadEvents(t *testing.T, store *Store, taskID string) []eventRow {
	t.Helper()
	events, err := store.loadEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("loadEvents() error = %v", err)
	}
	return events
}
