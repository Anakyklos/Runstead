package governor_test

import (
	"context"
	"errors"
	"testing"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type deliveryClient struct {
	response provider.Response
	err      error
	requests []provider.Request
}

func (c *deliveryClient) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (c *deliveryClient) Complete(_ context.Context, request provider.Request) (provider.Response, error) {
	c.requests = append(c.requests, request)
	return c.response, c.err
}

type deliveryReceiptClient struct {
	response provider.Response
}

func (deliveryReceiptClient) RouteSafety() provider.RouteSafety { return provider.ReceiptRouteSafety() }
func (deliveryReceiptClient) AttemptReceiptsEnabled() bool      { return true }
func (c deliveryReceiptClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	return c.response, nil
}

func TestExecuteClassifierCannotFabricateDeliveryState(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := provider.NewFake(provider.Response{
		Text: "model text claims completed",
		Metadata: provider.ResponseMetadata{
			DeliveryState: provider.DeliverySentUnconfirmed,
		},
	})
	classifier := func(provider.Response, error) policy.Outcome {
		return policy.Outcome{Class: policy.OutcomeSuccess, DeliveryState: provider.DeliveryCompleted}
	}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, classifier)
	if result.Response.Metadata.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("metadata state = %v, want sent_unconfirmed", result.Response.Metadata.DeliveryState)
	}
	if result.Completion.Outcome != policy.OutcomeUncertainReached || result.Completion.RetryEligible {
		t.Fatalf("ambiguous delivery completion = %#v", result.Completion)
	}
}

func TestUnobservedDeliveryRemainsUnobservedButIsConservative(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := provider.NewFake(provider.Response{Text: "response"})
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, nil)
	if result.Response.Metadata.DeliveryState.Valid() {
		t.Fatalf("raw delivery state = %v, want unobserved", result.Response.Metadata.DeliveryState)
	}
	if result.Completion.Outcome != policy.OutcomeSuccess || result.Completion.RetryEligible {
		t.Fatalf("unobserved delivery completion = %#v, want existing success classification without replay permission", result.Completion)
	}
	if result.Completion.DeliveryState.Valid() {
		t.Fatalf("completion delivery state = %v, want unobserved", result.Completion.DeliveryState)
	}
}

func TestNotSentCancellationIsNotConvertedToUncertain(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := &deliveryClient{
		response: provider.Response{Metadata: provider.ResponseMetadata{
			DeliveryState: provider.DeliveryNotSent,
		}},
		err: context.Canceled,
	}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, nil)
	if result.Completion.Outcome != policy.OutcomeCancelledBeforeUpstream {
		t.Fatalf("cancellation completion = %#v, want cancelled before upstream", result.Completion)
	}
	if result.Response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("delivery state = %v, want not_sent", result.Response.Metadata.DeliveryState)
	}
}

func TestResponseStartedForcesConservativeOutcome(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := &deliveryClient{
		response: provider.Response{Metadata: provider.ResponseMetadata{
			DeliveryState: provider.DeliveryResponseStarted,
		}},
		err: errors.New("connection reset after response started"),
	}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, nil)
	if result.Completion.Outcome != policy.OutcomeUncertainReached || result.Completion.RetryEligible {
		t.Fatalf("partial response completion = %#v", result.Completion)
	}
}

func TestCompletedFailureKeepsExistingNewAttemptRetryPolicy(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := &deliveryClient{response: provider.Response{Metadata: provider.ResponseMetadata{
		DeliveryState: provider.DeliveryCompleted,
	}}}
	classifier := func(provider.Response, error) policy.Outcome {
		return policy.Outcome{Class: policy.OutcomeRateCapacity}
	}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, classifier)
	if result.Completion.Outcome != policy.OutcomeRateCapacity || !result.Completion.RetryEligible {
		t.Fatalf("completed failure retry policy = %#v, want existing retry eligibility", result.Completion)
	}
}

func TestAmbiguousDeliveryDoesNotBypassNewAdmission(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.Response{Metadata: provider.ResponseMetadata{
		DeliveryState: provider.DeliverySentUnconfirmed,
	}})
	first := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
	}, client, nil)
	if first.Completion.RetryEligible {
		t.Fatal("ambiguous delivery was marked retry eligible")
	}
	clock.Advance(1)
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-2",
	})
	if !second.Admitted() {
		t.Fatalf("new request admission = %#v, want a fresh governed admission", second)
	}
	if err := second.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	second.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestDeliveryStateDoesNotSubstituteForMissingReceipts(t *testing.T) {
	events := &eventSink{}
	governor, clock := receiptGovernor(t, events)
	client := deliveryReceiptClient{response: provider.Response{
		Text:     "complete body",
		Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted},
	}}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Model: "concrete-model"},
	}, client, nil)
	_ = clock
	if result.Completion.Outcome != policy.OutcomeUncertainReached || result.Completion.RetryEligible {
		t.Fatalf("missing receipt completion = %#v", result.Completion)
	}
	if !governor.Snapshot().Telemetry.Unsafe {
		t.Fatal("missing receipt did not keep conservative telemetry unsafe")
	}
}
