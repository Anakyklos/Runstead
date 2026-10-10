package compat

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/responsessiwc"
)

func TestSIWCSelectsDirectResponsesAdapter(t *testing.T) {
	cfg := provider.Config{
		DocumentVersion: 2,
		WireContract:    provider.WireResponsesSIWCV1,
		ProviderID:      "siwc", ProtocolFamily: provider.FamilyOpenAICompatible,
		BaseURL: "https://api.openai.com/v1", Model: "exact-model",
		AuthRequirement: provider.AuthReferenceRequired, Auth: provider.SecretRef("siwc:issued-client"),
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
	got, err := New(*resolved, nil)
	if err != nil {
		if _, ok := err.(*responsessiwc.Error); !ok {
			t.Fatalf("SIWC did not fail through direct adapter: %T %v", err, err)
		}
		return // no protected fixture is required by this composition test
	}
	if _, ok := got.(*responsessiwc.Client); !ok {
		t.Fatalf("Responses SIWC selected legacy adapter %T", got)
	}
}
