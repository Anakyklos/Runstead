package governor_test

import (
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestGatewayContractHealthAdmissionSurfaceCarriesTypedResult(t *testing.T) {
	health := provider.GatewayContractHealthResult{
		State:      provider.GatewayContractHealthProtocolChanged,
		ReasonCode: "missing_or_invalid_field",
		Endpoint:   "/api/settings",
		CheckedAt:  time.Date(2026, time.August, 13, 13, 0, 0, 0, time.UTC),
	}
	result := policy.AdmissionResult{
		Code:                  policy.AdmissionGatewayContractUnhealthy,
		Reason:                policy.AdmissionGatewayContractUnhealthy,
		GatewayContractHealth: &health,
	}
	if result.Code != policy.AdmissionGatewayContractUnhealthy {
		t.Fatalf("admission code = %q", result.Code)
	}
	if result.GatewayContractHealth == nil || result.GatewayContractHealth.State != provider.GatewayContractHealthProtocolChanged {
		t.Fatalf("admission health = %#v", result.GatewayContractHealth)
	}

	event := policy.Event{GatewayContractHealth: result.GatewayContractHealth}
	if event.GatewayContractHealth == nil || event.GatewayContractHealth.ReasonCode != "missing_or_invalid_field" {
		t.Fatalf("event health = %#v", event.GatewayContractHealth)
	}
}
