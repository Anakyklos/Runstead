package governor_test

import (
	"context"
	"errors"
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
	return receiptGovernorWithConfig(t, events, nil)
}

func receiptGovernorWithConfig(t *testing.T, events *eventSink, configure func(*policy.Config)) (*policy.Governor, *fakeClock) {
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
	if configure != nil {
		configure(&config)
	}
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

func TestReceiptAwareExecutionDebitsEveryNewAmplifiedAttemptAndBlocksLane(t *testing.T) {
	events := &eventSink{}
	governor, clock := receiptGovernor(t, events)
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: receiptSet(clock.Now(), 2)}, nil)
	if result.Completion.Err == nil || result.Completion.AttemptDebited != 2 {
		t.Fatalf("internal amplification result = %#v, want two authoritative debits", result.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 2 {
		t.Fatalf("internal amplification budget = %#v, want two debits", governor.Snapshot().Budgets)
	}
	governor.DrainEvents()
	var upstreamEvents int
	for _, event := range events.Events() {
		if event.Kind == policy.EventUpstreamAttempt {
			upstreamEvents++
		}
	}
	if upstreamEvents != 2 {
		t.Fatalf("amplified attempt events = %d, want two", upstreamEvents)
	}
	if next := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}); next.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("post-amplification admission = %#v, want unsafe fail-closed result", next)
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

func TestReceiptAwareExecutionDebitsExactlyNewIDsInMixedReplay(t *testing.T) {
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
	mixed := rebindReceiptSet(receiptSet(clock.Now(), 3), "request-2")
	second := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	}, &receiptClient{set: mixed}, nil)
	if second.Completion.Err == nil || second.Completion.AttemptDebited != 2 {
		t.Fatalf("mixed replay result = %#v, want two new-ID debits", second.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 3 {
		t.Fatalf("mixed replay budget = %#v, want three total debits", governor.Snapshot().Budgets)
	}
}

func TestReceiptAwareExecutionUsesLocalStartForPacingWithDelayedClock(t *testing.T) {
	governor, clock := receiptGovernorWithConfig(t, &eventSink{}, func(config *policy.Config) {
		config.MinimumStartInterval = 5 * time.Second
	})
	localStart := clock.Now()
	set := receiptSet(localStart, 1)
	set.Receipts[0].StartedAt = localStart.Add(-4 * time.Minute)
	set.Receipts[0].CompletedAt = localStart.Add(-4*time.Minute + time.Second)
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: set}, nil)
	if result.Completion.Err != nil {
		t.Fatalf("delayed-clock receipt execution = %#v", result.Completion)
	}
	next := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	})
	if next.Code != policy.AdmissionDelayed || !next.RetryAt.Equal(localStart.Add(5*time.Second)) {
		t.Fatalf("delayed-clock pacing = %#v, want local five-second delay", next)
	}
}

func TestReceiptAwareExecutionUsesLocalStartForRollingWindowWithDelayedClock(t *testing.T) {
	governor, clock := receiptGovernorWithConfig(t, &eventSink{}, func(config *policy.Config) {
		config.Rolling10m = 1
	})
	localStart := clock.Now()
	set := receiptSet(localStart, 1)
	set.Receipts[0].StartedAt = localStart.Add(-4 * time.Minute)
	set.Receipts[0].CompletedAt = localStart.Add(-4*time.Minute + time.Second)
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
	}, &receiptClient{set: set}, nil)
	if result.Completion.Err != nil {
		t.Fatalf("delayed-clock receipt execution = %#v", result.Completion)
	}
	clock.Advance(7 * time.Minute)
	next := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	})
	if next.Code != policy.AdmissionDelayed || next.Reason != policy.AdmissionRollingBudgetExhausted || !next.RetryAt.Equal(localStart.Add(10*time.Minute)) {
		t.Fatalf("delayed-clock rolling budget = %#v, want local ten-minute window", next)
	}
}

// failPersistence is a governor.Persistence stub whose RecordProviderPrepared
// always fails, simulating a durable-intent write error at TX 1.
type failPersistence struct{}

func (failPersistence) RecordProviderPrepared(context.Context, policy.ProviderPrepared) error {
	return errors.New("durable write failed")
}

func (failPersistence) RecordProviderFinished(context.Context, policy.ProviderFinished) error {
	return nil
}

// TestReceiptAwareTX1FailureReleasesLane proves the reviewer-requested abort
// path: when the durable provider intent (TX 1) cannot be committed after a
// receipt-aware start, the provider is never called, no debit is recorded,
// and the account lane is fully released instead of being stuck waiting for
// receipts that will never arrive.
func TestReceiptAwareTX1FailureReleasesLane(t *testing.T) {
	events := &eventSink{}
	config := fastConfig(t)
	config.ProviderID = "provider"
	config.ModelPool = "model"
	config.Model = "concrete-model"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AttemptProviderID = "provider"
	config.AccountLaneHash = "lane"
	config.RouteSafety = provider.ReceiptRouteSafety()
	accountGovernor, err := policy.New(config, policy.Options{
		Clock:       newFakeClock(),
		Jitter:      fixedJitter{},
		Events:      events,
		Persistence: failPersistence{},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &receiptClient{set: receiptSet(time.Now(), 1)}

	result := accountGovernor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "p", Model: "concrete-model"},
	}, client, nil)
	if result.Err == nil {
		t.Fatal("Execute must fail when the durable intent cannot be persisted")
	}
	if client.calls != 0 {
		t.Fatalf("provider must never be called when TX 1 fails: calls=%d", client.calls)
	}
	if result.Completion.AttemptDebited != 0 {
		t.Fatalf("aborted start must not debit: debited=%d", result.Completion.AttemptDebited)
	}
	snapshot := accountGovernor.Snapshot()
	if snapshot.InFlight {
		t.Fatal("account lane must be released after the aborted receipt-aware start")
	}
	if snapshot.QueueLength != 0 {
		t.Fatalf("queue length = %d, want 0", snapshot.QueueLength)
	}
	// A subsequent admission must succeed: the lane is not stuck.
	next := accountGovernor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-2",
		ModelPool:       "model",
	})
	if next.Code != policy.AdmissionAdmitted {
		t.Fatalf("post-abort admission = %s, want admitted", next.Code)
	}
}
