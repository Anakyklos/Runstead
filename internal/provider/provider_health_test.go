package provider_test

import (
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestGatewayContractHealthZeroValueIsUnknown(t *testing.T) {
	var state provider.GatewayContractHealth
	if state != provider.GatewayContractHealthUnknown {
		t.Fatalf("zero health = %v, want unknown", state)
	}
	if state.String() != "unknown" {
		t.Fatalf("zero health string = %q, want unknown", state.String())
	}
}

func TestGatewayContractHealthResultHealthyOnlyForHealthyState(t *testing.T) {
	result := provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy}
	if !result.Healthy() {
		t.Fatal("healthy result did not report Healthy")
	}
	result.State = provider.GatewayContractHealthDegraded
	if result.Healthy() {
		t.Fatal("degraded result reported Healthy")
	}
}

func TestGatewayContractHealthStringsAreExplicitAndConservative(t *testing.T) {
	cases := []struct {
		state provider.GatewayContractHealth
		want  string
	}{
		{provider.GatewayContractHealthUnknown, "unknown"},
		{provider.GatewayContractHealthHealthy, "healthy"},
		{provider.GatewayContractHealthDegraded, "degraded"},
		{provider.GatewayContractHealthProtocolChanged, "protocol_changed"},
		{provider.GatewayContractHealth(255), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("state %d string = %q, want %q", tc.state, got, tc.want)
		}
	}
}
