package governor

import (
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type gatewayContractHealthEventSink struct {
	events []Event
}

func (s *gatewayContractHealthEventSink) Emit(event Event) {
	s.events = append(s.events, event)
}

func TestEmitAdmissionCopiesGatewayContractHealth(t *testing.T) {
	config := DefaultInstantConfig("policy-account-1", "provider-1", "instant", provider.SafeRouteSafety())
	g, err := New(config, Options{Events: &gatewayContractHealthEventSink{}})
	if err != nil {
		t.Fatal(err)
	}
	sink := g.events.(*gatewayContractHealthEventSink)
	health := provider.GatewayContractHealthResult{
		State:      provider.GatewayContractHealthUnknown,
		ReasonCode: "unprobed",
		Endpoint:   "/api/providers",
		CheckedAt:  time.Date(2026, time.August, 13, 13, 0, 0, 0, time.UTC),
	}
	g.mu.Lock()
	g.emitAdmissionLocked(AttemptRequest{TaskID: "task-1", ClientRequestID: "request-1"}, AdmissionResult{
		Code:                  AdmissionGatewayContractUnhealthy,
		Reason:                AdmissionGatewayContractUnhealthy,
		GatewayContractHealth: &health,
	}, false)
	g.mu.Unlock()
	if drained := g.DrainEvents(); drained != 1 {
		t.Fatalf("drained events = %d, want 1", drained)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	got := sink.events[0].GatewayContractHealth
	if got == nil || got.State != provider.GatewayContractHealthUnknown || got.ReasonCode != "unprobed" {
		t.Fatalf("event health = %#v", got)
	}
}
