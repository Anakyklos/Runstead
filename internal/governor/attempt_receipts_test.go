package governor_test

import (
	"context"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type receiptClient struct {
	set       provider.AttemptReceiptSet
	calls     int
	requestID string
}

type missingReceiptClient struct{}

func (missingReceiptClient) RouteSafety() provider.RouteSafety { return provider.ReceiptRouteSafety() }

func (missingReceiptClient) AttemptReceiptsEnabled() bool { return true }

func (missingReceiptClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (c *receiptClient) RouteSafety() provider.RouteSafety { return provider.ReceiptRouteSafety() }

func (c *receiptClient) AttemptReceiptsEnabled() bool { return true }

func (c *receiptClient) Complete(_ context.Context, request provider.Request) (provider.Response, error) {
	c.calls++
	c.requestID = request.ClientRequestID
	return provider.Response{
		Text: "response",
		Metadata: provider.ResponseMetadata{
			AttemptReceipts: &c.set,
		},
	}, nil
}

func receiptSet(now time.Time, count int) provider.AttemptReceiptSet {
	set := provider.AttemptReceiptSet{
		SchemaVersion:   provider.AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
	}
	for i := 1; i <= count; i++ {
		trigger := provider.AttemptTriggerInitial
		if i > 1 {
			trigger = provider.AttemptTriggerExecutorRetry
		}
		set.Receipts = append(set.Receipts, provider.AttemptReceipt{
			SchemaVersion:   provider.AttemptReceiptSchemaVersion,
			AttemptID:       "attempt-" + string(rune('0'+i)),
			ClientRequestID: "request-1",
			Sequence:        i,
			Provider:        "provider",
			Model:           "concrete-model",
			AccountLaneHash: "lane",
			StartedAt:       now.Add(time.Duration(i-1) * time.Second),
			CompletedAt:     now.Add(time.Duration(i) * time.Second),
			Outcome:         provider.AttemptOutcomeSuccess,
			Trigger:         trigger,
			UpstreamReached: true,
		})
	}
	return set
}

func rebindReceiptSet(set provider.AttemptReceiptSet, requestID string) provider.AttemptReceiptSet {
	set.ClientRequestID = requestID
	for index := range set.Receipts {
		set.Receipts[index].ClientRequestID = requestID
	}
	return set
}

func receiptGovernor(t *testing.T, events *eventSink) (*policy.Governor, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	config := fastConfig(t)
	config.ProviderID = "provider"
	config.ModelPool = "model"
	config.Model = "concrete-model"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AttemptProviderID = "provider"
	config.AccountLaneHash = "lane"
	config.RouteSafety = provider.ReceiptRouteSafety()
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return governor, clock
}

func TestReceiptAwareExecutionDebitsExactlyOnePerReceipt(t *testing.T) {
	events := &eventSink{}
	governor, clock := receiptGovernor(t, events)
	client := &receiptClient{set: receiptSet(clock.Now(), 1)}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, client, nil)
	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("receipt-aware Execute() = %#v", result)
	}
	if client.calls != 1 || client.requestID != "request-1" {
		t.Fatalf("provider call = %d/%q, want one correlated call", client.calls, client.requestID)
	}
	if result.Completion.AttemptDebited != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("receipt accounting = %#v, snapshot=%#v", result.Completion, governor.Snapshot().Budgets)
	}
	if !governor.Snapshot().LastStart.Equal(clock.Now()) {
		t.Fatalf("last start = %s, want receipt start %s", governor.Snapshot().LastStart, clock.Now())
	}
	governor.DrainEvents()
	var receiptEvents int
	for _, event := range events.Events() {
		if event.Kind == policy.EventUpstreamAttempt {
			receiptEvents++
		}
	}
	if receiptEvents != 1 {
		t.Fatalf("receipt events = %d, want 1", receiptEvents)
	}
}

func TestReceiptAwareExecutionPreservesInternalSecuritySignals(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	set := receiptSet(clock.Now(), 1)
	set.Receipts[0].Outcome = provider.AttemptOutcomeCAPTCHA
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: set}, nil)
	if result.Completion.Outcome != policy.OutcomeCAPTCHA || result.Completion.Circuit.State != policy.CircuitHumanReviewRequired {
		t.Fatalf("internal security signal = %#v, want human-review circuit", result.Completion)
	}
	if next := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-2", ClientRequestID: "request-2", ModelPool: "model"}); next.Code != policy.AdmissionHumanAcknowledgementRequired {
		t.Fatalf("post-CAPTCHA admission = %#v, want acknowledgement gate", next)
	}
}

func TestReceiptAwareExecutionDoesNotDoubleChargeTheInitialAttempt(t *testing.T) {
	events := &eventSink{}
	governor, clock := receiptGovernor(t, events)
	client := &receiptClient{set: receiptSet(clock.Now(), 1)}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, client, nil)
	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("receipt-aware Execute() error = %#v", result)
	}
	if result.Completion.AttemptDebited != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("initial receipt accounting = %#v, snapshot=%#v", result.Completion, governor.Snapshot().Budgets)
	}
	if task := governor.Snapshot().Tasks["task-1"]; task.Attempts != 1 || task.Retries != 0 {
		t.Fatalf("task accounting = %#v, want one non-retry attempt", task)
	}
}

func TestReceiptAwareExecutionRejectsInternalAmplificationWithOneConservativeDebit(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: receiptSet(clock.Now(), 2)}, nil)
	if result.Completion.Err == nil || result.Completion.AttemptDebited != 1 {
		t.Fatalf("internal amplification result = %#v, want one conservative debit", result.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("internal amplification budget = %#v, want one debit", governor.Snapshot().Budgets)
	}
}

func TestReceiptAwareExecutionCountsUncertainAttemptAndBlocksOnMissingProof(t *testing.T) {
	events := &eventSink{}
	governor, clock := receiptGovernor(t, events)
	set := receiptSet(clock.Now(), 1)
	set.Receipts[0].Outcome = provider.AttemptOutcomeUncertain
	client := &receiptClient{set: set}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, client, nil)
	if result.Completion.Outcome != policy.OutcomeUncertainReached || result.Completion.AttemptDebited != 1 {
		t.Fatalf("uncertain receipt result = %#v", result.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("uncertain receipt was not debited: %#v", governor.Snapshot().Budgets)
	}

	missingGovernor, _ := receiptGovernor(t, &eventSink{})
	missing := missingGovernor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}, missingReceiptClient{}, nil)
	if missing.Completion.Err == nil || missing.Completion.AttemptDebited != 1 {
		t.Fatalf("missing receipt result = %#v, want fail-closed uncertain debit", missing.Completion)
	}
	if next := missingGovernor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-3",
		ClientRequestID: "request-3",
		ModelPool:       "model",
	}); next.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("post-missing admission = %#v, want unsafe fail-closed result", next)
	}
}

func TestReceiptAwareExecutionRejectsPreUpstreamReceiptWithoutDebiting(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	set := receiptSet(clock.Now(), 1)
	set.Receipts[0].UpstreamReached = false
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: set}, nil)
	if result.Completion.Err == nil || result.Completion.AttemptDebited != 1 {
		t.Fatalf("pre-upstream receipt result = %#v, want fail-closed uncertain debit", result.Completion)
	}
	if next := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}); next.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("post-invalid admission = %#v, want unsafe fail-closed result", next)
	}
}

func TestReceiptAwareExecutionRejectsAttemptIDReplayWithoutSecondDebit(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	first := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: receiptSet(clock.Now(), 1)}, nil)
	if first.Completion.Err != nil {
		t.Fatalf("first receipt execution = %#v", first.Completion)
	}
	clock.Advance(time.Second)
	secondSet := rebindReceiptSet(receiptSet(clock.Now(), 1), "request-2")
	second := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}, &receiptClient{set: secondSet}, nil)
	if second.Completion.Err == nil || second.Completion.Err.Error() != policy.ErrAttemptReceiptReplayed.Error() {
		t.Fatalf("replayed receipt execution = %#v, want replay error", second.Completion)
	}
	if second.Completion.AttemptDebited != 0 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("replayed receipt accounting = %#v, snapshot=%#v", second.Completion, governor.Snapshot().Budgets)
	}
}

func TestReceiptAttemptIDRetentionIsBoundedToProtectionWindow(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: receiptSet(clock.Now(), 1)}, nil)
	if result.Completion.Err != nil || governor.Snapshot().RetainedAttemptIDs != 1 {
		t.Fatalf("retained attempt IDs after reconciliation = %#v", governor.Snapshot())
	}
	clock.Advance(3*time.Hour + time.Second)
	if got := governor.Snapshot().RetainedAttemptIDs; got != 0 {
		t.Fatalf("retained attempt IDs after retention window = %d, want 0", got)
	}
}

func TestReceiptAwareExecutionRejectsMixedReplayWithUncertainDebit(t *testing.T) {
	governor, clock := receiptGovernor(t, &eventSink{})
	first := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: receiptSet(clock.Now(), 1)}, nil)
	if first.Completion.Err != nil {
		t.Fatalf("first receipt execution = %#v", first.Completion)
	}
	clock.Advance(time.Second)
	mixed := rebindReceiptSet(receiptSet(clock.Now(), 2), "request-2")
	second := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}, &receiptClient{set: mixed}, nil)
	if second.Completion.Err == nil || second.Completion.AttemptDebited != 1 {
		t.Fatalf("mixed replay result = %#v, want one uncertain debit", second.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 2 {
		t.Fatalf("mixed replay budget = %#v, want two total debits", governor.Snapshot().Budgets)
	}
}
