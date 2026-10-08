package compat

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestSIWCRemainsInertWithoutResponsesAdapter(t *testing.T) {
	cfg := provider.Config{
		DocumentVersion: 2,
		WireContract:    provider.WireResponsesSIWCV1,
		ProviderID:      "siwc", ProtocolFamily: provider.FamilyOpenAICompatible,
		BaseURL: "https://api.openai.com/v1", Model: "exact-model",
		AuthRequirement: provider.AuthReferenceRequired, Auth: provider.SecretRef("SIWC_CONNECTION"),
		AccountBinding:    "hmac-sha256:v1:" + strings.Repeat("a", 64),
		CredentialBinding: "hmac-sha256:v1:" + strings.Repeat("b", 64),
		Profile: provider.CapabilityProfile{ProfileVersion: "v1", Capabilities: provider.Capabilities{
			provider.CapabilityTextTurn: true, provider.CapabilityRunsteadProtocol: true,
		}, RouteSafety: provider.SafeRouteSafety()},
		ConfigVersion: "1",
	}
	registry, err := provider.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve("siwc", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := New(*resolved, nil); err == nil || got != nil {
		t.Fatal("Responses SIWC dispatched via legacy adapter")
	}
}
