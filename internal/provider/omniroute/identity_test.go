package omniroute

import (
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestIdentityFromConfigIsStableAndSanitized(t *testing.T) {
	config := Config{
		BaseURL:               "http://route.example/v1",
		ManagementBaseURL:     "http://route.example",
		APIKey:                "fixture-api-key",
		ConnectionID:          "fixture-connection",
		Model:                 "chatgpt-web/model",
		Provider:              ProviderChatGPTWeb,
		AccountLaneHash:       LaneHashForConnection("fixture-connection"),
		EnableAttemptReceipts: true,
		ChatEndpoint:          DedicatedChatEndpoint,
		Timeout:               17 * time.Second,
		MaxRequestBytes:       1024,
		MaxResponseBytes:      2048,
		RouteSafety:           provider.ReceiptRouteSafety(),
	}

	identity := IdentityFromConfig(config)
	if !IsIdentity(identity) {
		t.Fatalf("IdentityFromConfig() identity = %#v, want OmniRoute identity marker", identity)
	}
	if identity.ProviderID != IdentityProviderID || identity.ProtocolFamily != provider.FamilyOpenAICompatible {
		t.Fatalf("identity = %#v, want OmniRoute/openai-compatible identity", identity)
	}
	for _, forbidden := range []string{"fixture-api-key", "fixture-connection", "APIKey", "ConnectionID"} {
		if strings.Contains(identity.ConfigIdentity, forbidden) {
			t.Fatalf("config identity contains %q: %s", forbidden, identity.ConfigIdentity)
		}
	}
	if !strings.HasPrefix(identity.ConfigIdentity, "provider.Config{") {
		t.Fatalf("config identity = %q, want provider.Config sanitized identity", identity.ConfigIdentity)
	}

	changedSecret := config
	changedSecret.APIKey = "another-fixture-api-key"
	if got := IdentityFromConfig(changedSecret); got.ConfigIdentity != identity.ConfigIdentity {
		t.Fatalf("API key changed sanitized identity: %q vs %q", got.ConfigIdentity, identity.ConfigIdentity)
	}
	changedConnection := config
	changedConnection.ConnectionID = "another-fixture-connection"
	changedConnection.AccountLaneHash = LaneHashForConnection(changedConnection.ConnectionID)
	if got := IdentityFromConfig(changedConnection); got.ConfigIdentity == identity.ConfigIdentity {
		t.Fatal("connection lane changed without changing sanitized identity")
	}
}
