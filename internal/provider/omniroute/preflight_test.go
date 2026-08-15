package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func receiptAwarePreflightConfig(baseURL string) Config {
	config := testConfig(baseURL)
	config.EnableAttemptReceipts = true
	config.Provider = ProviderChatGPTWeb
	config.ConnectionID = "synthetic-connection-001"
	config.ChatEndpoint = DedicatedChatEndpoint
	config.AccountLaneHash = LaneHashForConnection("synthetic-connection-001")
	config.RouteSafety = provider.ReceiptRouteSafety()
	return config
}

func newReceiptAwarePreflightClient(t *testing.T, server *contractMockServer) *Client {
	t.Helper()
	client, err := New(receiptAwarePreflightConfig(server.URL()), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fixtureResponse(name string) contractMockResponse {
	return contractMockResponse{status: http.StatusOK, body: []byte(mustReadContractFixture("management/" + name))}
}

// A. The receipt-aware preflight passes with two or more active chatgpt-web
// connections: the protected request is explicitly pinned and the gateway
// contract is healthy, so unrelated active connections never block it.
func TestReceiptAwarePreflightAllowsMultipleActiveConnections(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"providers": fixtureResponse("providers-multiple-active.json"),
		},
	})
	defer server.Close()
	client := newReceiptAwarePreflightClient(t, server)

	health := client.ProbeGatewayContract(context.Background())
	if !health.Healthy() {
		t.Fatalf("gateway contract health = %s (%s), want healthy", health.State, health.ReasonCode)
	}
	if err := client.Preflight(context.Background()); err != nil {
		t.Fatalf("receipt-aware preflight must pass with multiple active connections: %v", err)
	}
}

// B. The receipt-aware preflight passes when combos, fallback chains and
// model-combo mappings exist for other OmniRoute traffic: receipt-v1
// neutrality is enforced per request by the pinned producer, not by empty
// global configuration.
func TestReceiptAwarePreflightAllowsUnrelatedGlobalComboConfig(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"combos":               fixtureResponse("combos-populated.json"),
			"fallback_chains":      fixtureResponse("fallback-chains-populated.json"),
			"model_combo_mappings": fixtureResponse("model-combo-mappings-populated.json"),
		},
	})
	defer server.Close()
	client := newReceiptAwarePreflightClient(t, server)

	health := client.ProbeGatewayContract(context.Background())
	if !health.Healthy() {
		t.Fatalf("gateway contract health = %s (%s), want healthy", health.State, health.ReasonCode)
	}
	if err := client.Preflight(context.Background()); err != nil {
		t.Fatalf("receipt-aware preflight must pass with unrelated global combo config: %v", err)
	}
}

// C1. The legacy single-attempt preflight still rejects globally populated
// combos/fallback/mappings: those guarantees are global for the legacy lane.
func TestLegacyPreflightStillRejectsGlobalComboConfig(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"combos":               fixtureResponse("combos-populated.json"),
			"fallback_chains":      fixtureResponse("fallback-chains-populated.json"),
			"model_combo_mappings": fixtureResponse("model-combo-mappings-populated.json"),
		},
	})
	defer server.Close()
	client, err := New(testConfig(server.URL()), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("legacy preflight must reject globally populated combos/fallback/mappings")
	}
}

// C2. The legacy single-attempt preflight still rejects more than one active
// connection for the provider: the legacy lane depends on that global
// guarantee.
func TestLegacyPreflightStillRejectsMultipleActiveConnections(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"providers": fixtureResponse("providers-multiple-active.json"),
		},
	})
	defer server.Close()
	client, err := New(testConfig(server.URL()), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("legacy preflight must reject multiple active connections for the provider")
	}
}

// D. A malformed / protocol-changed gateway stays fail-closed on the
// receipt-aware lane: ProbeGatewayContract classifies the drift and preflight
// refuses before any model request.
func TestReceiptAwarePreflightRejectsProtocolChangedGateway(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"providers": {status: http.StatusNotFound},
		},
	})
	defer server.Close()
	client := newReceiptAwarePreflightClient(t, server)

	health := client.ProbeGatewayContract(context.Background())
	if health.Healthy() {
		t.Fatal("gateway contract must not be healthy after a 404 drift")
	}
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("receipt-aware preflight must fail closed on a protocol-changed gateway")
	}
}

// D2. Malformed JSON on a mandatory gateway endpoint stays fail-closed.
func TestReceiptAwarePreflightRejectsMalformedGatewaySchema(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		management: map[string]contractMockResponse{
			"settings": {status: http.StatusOK, body: []byte(`{not json`)},
		},
	})
	defer server.Close()
	client := newReceiptAwarePreflightClient(t, server)

	health := client.ProbeGatewayContract(context.Background())
	if health.Healthy() {
		t.Fatal("gateway contract must not be healthy with malformed settings JSON")
	}
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("receipt-aware preflight must fail closed on malformed gateway schema")
	}
}

// F. Provider/model mismatch in the finalized receipt stays fail-closed even
// when the lane hash matches: the receipt correlation is the authority.
func TestReceiptAwarePreflightProviderModelMismatchFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	const connectionID = "conn-test-123"
	client, server := pinnedLaneClient(t, "", connectionID, func(config *Config) {
		config.Model = "chatgpt-web/model"
	})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The producer receipt claims a DIFFERENT model than requested.
		set := provider.AttemptReceiptSet{
			SchemaVersion:   provider.AttemptReceiptSchemaVersion,
			ClientRequestID: "request-1",
			Finalized:       true,
			Receipts: []provider.AttemptReceipt{{
				SchemaVersion:   provider.AttemptReceiptSchemaVersion,
				AttemptID:       "attempt-1",
				ClientRequestID: "request-1",
				Sequence:        1,
				Provider:        ProviderChatGPTWeb,
				Model:           "chatgpt-web/other-model",
				AccountLaneHash: LaneHashForConnection(connectionID),
				StartedAt:       now,
				CompletedAt:     now.Add(time.Second),
				Outcome:         provider.AttemptOutcomeSuccess,
				Trigger:         provider.AttemptTriggerInitial,
				UpstreamReached: true,
			}},
		}
		encoded, err := json.Marshal(set)
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set(attemptReceiptHeader, string(encoded))
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	})
	defer server.Close()
	client.now = func() time.Time { return now.Add(2 * time.Second) }

	_, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt", ClientRequestID: "request-1"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorAttemptReceiptsInvalid {
		t.Fatalf("Complete() error = %v, want typed invalid-receipts error for model mismatch", err)
	}
}
