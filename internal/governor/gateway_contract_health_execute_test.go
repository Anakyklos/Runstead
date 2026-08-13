package governor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type healthAwareFakeClient struct {
	health        provider.GatewayContractHealthResult
	receipts      bool
	completeCalls int
}

func (c *healthAwareFakeClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	c.completeCalls++
	return provider.Response{}, nil
}

func (c *healthAwareFakeClient) RouteSafety() provider.RouteSafety {
	return provider.ReceiptRouteSafety()
}

func (c *healthAwareFakeClient) AttemptReceiptsEnabled() bool {
	return c.receipts
}

func (c *healthAwareFakeClient) GatewayContractHealth() provider.GatewayContractHealthResult {
	return c.health
}

type gatewayHealthEventSink struct {
	events []policy.Event
}

func (s *gatewayHealthEventSink) Emit(event policy.Event) {
	s.events = append(s.events, event)
}

func receiptRequiredGovernor(t *testing.T, events policy.EventSink) *policy.Governor {
	t.Helper()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.ReceiptRouteSafety())
	config.Model = "instant"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AccountLaneHash = "synthetic-lane-hash"
	config.MinimumStartInterval = time.Nanosecond
	governor, err := policy.New(config, policy.Options{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return governor
}

func executeHealthRequest(governor *policy.Governor, client provider.Client) policy.ExecutionResult {
	return governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ProviderRequest: provider.Request{Model: "instant", Prompt: "synthetic"},
	}, client, nil)
}

func TestExecuteBlocksNonHealthyGatewayContractBeforeComplete(t *testing.T) {
	for _, state := range []provider.GatewayContractHealth{
		provider.GatewayContractHealthUnknown,
		provider.GatewayContractHealthDegraded,
		provider.GatewayContractHealthProtocolChanged,
	} {
		t.Run(state.String(), func(t *testing.T) {
			events := &gatewayHealthEventSink{}
			client := &healthAwareFakeClient{
				health:   provider.GatewayContractHealthResult{State: state, ReasonCode: state.String()},
				receipts: true,
			}
			governor := receiptRequiredGovernor(t, events)
			result := executeHealthRequest(governor, client)
			if result.Admission.Code != policy.AdmissionGatewayContractUnhealthy {
				t.Fatalf("admission = %#v, want gateway_contract_unhealthy", result.Admission)
			}
			if !errors.Is(result.Err, provider.ErrGatewayContractUnhealthy) {
				t.Fatalf("execution error = %v, want gateway contract health error", result.Err)
			}
			if client.completeCalls != 0 {
				t.Fatalf("Complete calls = %d, want 0", client.completeCalls)
			}
			if result.Admission.GatewayContractHealth == nil || result.Admission.GatewayContractHealth.State != state {
				t.Fatalf("admission health = %#v", result.Admission.GatewayContractHealth)
			}
			if drained := governor.DrainEvents(); drained != 1 || len(events.events) != 1 {
				t.Fatalf("drained events = %d, events = %d, want one", drained, len(events.events))
			}
			if events.events[0].GatewayContractHealth == nil || events.events[0].GatewayContractHealth.State != state {
				t.Fatalf("event health = %#v", events.events[0].GatewayContractHealth)
			}
		})
	}
}

func TestExecuteHealthyGatewayContractStillRequiresReceipts(t *testing.T) {
	client := &healthAwareFakeClient{
		health:   provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "recognized"},
		receipts: false,
	}
	result := executeHealthRequest(receiptRequiredGovernor(t, nil), client)
	if result.Admission.Code != policy.AdmissionMissingAttemptReceipts {
		t.Fatalf("admission = %#v, want missing receipts", result.Admission)
	}
	if client.completeCalls != 0 {
		t.Fatalf("Complete calls = %d, want 0", client.completeCalls)
	}
}

func TestExecuteHealthyGatewayContractKeepsReceiptRequirementIndependent(t *testing.T) {
	client := &healthAwareFakeClient{
		health:   provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "recognized"},
		receipts: true,
	}
	result := executeHealthRequest(receiptRequiredGovernor(t, nil), client)
	if result.Admission.Code != policy.AdmissionAdmitted {
		t.Fatalf("admission = %#v, want admitted before the fake completion", result.Admission)
	}
	if client.completeCalls != 1 {
		t.Fatalf("Complete calls = %d, want one controlled call", client.completeCalls)
	}
	if result.Err == nil || result.Completion.Err == nil {
		t.Fatalf("healthy execution error = %v completion = %#v, want receipt validation failure", result.Err, result.Completion)
	}
	if result.Completion.Outcome != policy.OutcomeUncertainReached {
		t.Fatalf("completion outcome = %q, want uncertain_reached", result.Completion.Outcome)
	}
}
