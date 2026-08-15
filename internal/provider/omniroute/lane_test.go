package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Golden vector for the v1 account-lane hash derivation (issue #30). The
// OmniRoute producer derives the receipt lane hash with the same formula over
// the actually selected connection; the static vector prevents producer and
// consumer from silently sharing the same derivation bug.
func TestLaneHashForConnectionGoldenVector(t *testing.T) {
	const want = "ebae45b2394081da729b4006e58d00145162145bfae0bd2db50de6661961259f"
	got := LaneHashForConnection("conn-test-123")
	if got != want {
		t.Fatalf("LaneHashForConnection(conn-test-123) = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("lane hash length = %d, want 64 lowercase hex characters", len(got))
	}
	for _, char := range got {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			t.Fatalf("lane hash %q is not lowercase hexadecimal", got)
		}
	}
}

func TestLaneHashForConnectionIsDomainSeparated(t *testing.T) {
	if LaneHashForConnection("conn-1") == LaneHashForConnection("conn-2") {
		t.Fatal("different connection ids must produce different lane hashes")
	}
	if LaneHashForConnection("") == LaneHashForConnection("conn-1") {
		t.Fatal("empty connection id must not collide with a real one")
	}
	// A connection id that embeds the prefix must not collide with the raw
	// prefix: the 0x00 separator keeps the derivation domain-separated.
	prefixLike := "omniroute-connection-v1"
	if LaneHashForConnection(prefixLike) == LaneHashForConnection("") {
		t.Fatal("domain separator is ineffective")
	}
}

func TestConfigStringRedactsConnectionID(t *testing.T) {
	config := Config{
		BaseURL:         "https://omniroute.invalid/v1",
		APIKey:          "secret-api-key",
		ConnectionID:    "conn-secret-42",
		Model:           "chatgpt-web/model",
		Provider:        ProviderChatGPTWeb,
		AccountLaneHash: LaneHashForConnection("conn-secret-42"),
		ChatEndpoint:    DedicatedChatEndpoint,
		RouteSafety:     provider.ReceiptRouteSafety(),
	}
	rendered := config.String()
	for _, secret := range []string{"conn-secret-42", "secret-api-key"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("Config.String() leaks %q: %s", secret, rendered)
		}
	}
	if !strings.Contains(rendered, "ConnectionID:<redacted>") {
		t.Fatalf("Config.String() must mark ConnectionID as redacted: %s", rendered)
	}
	if config.GoString() != rendered {
		t.Fatal("GoString must redact exactly like String")
	}
}

// pinnedLaneClient returns a receipt-aware chatgpt-web client whose gateway
// contract health is pre-populated as healthy.
func pinnedLaneClient(t *testing.T, baseURL, connectionID string, mutate func(*Config)) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	config := testConfig(server.URL)
	config.EnableAttemptReceipts = true
	config.Provider = ProviderChatGPTWeb
	config.ConnectionID = connectionID
	config.AccountLaneHash = LaneHashForConnection(connectionID)
	config.ChatEndpoint = DedicatedChatEndpoint
	config.RouteSafety = provider.ReceiptRouteSafety()
	if mutate != nil {
		mutate(&config)
	}
	client, err := New(config, Options{HTTPClient: server.Client()})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client.gatewayContractHealth = provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "test_fixture"}
	return client, server
}

func TestCompletePinnedLaneSendsConnectionPinAndReceiptHeaders(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	const connectionID = "conn-test-123"
	client, server := pinnedLaneClient(t, "", connectionID, func(config *Config) {
		config.Model = "chatgpt-web/model"
	})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/providers/chatgpt-web/chat/completions" {
			t.Errorf("chat POST path = %q, want /v1/providers/chatgpt-web/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get(connectionHeader); got != connectionID {
			t.Errorf("connection pin header = %q, want %q", got, connectionID)
		}
		if got := r.Header.Get(clientRequestIDHeader); got != "request-1" {
			t.Errorf("client request header = %q, want request-1", got)
		}
		if got := r.Header.Get(attemptReceiptRequestHeader); got != "v1" {
			t.Errorf("receipt request header = %q, want v1", got)
		}
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
				Model:           "chatgpt-web/model",
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

	response, err := client.Complete(context.Background(), provider.Request{
		Prompt:          "prompt",
		ClientRequestID: "request-1",
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if response.Text != "response" {
		t.Fatalf("response text = %q, want response", response.Text)
	}
	if response.Metadata.AttemptReceipts == nil || len(response.Metadata.AttemptReceipts.Receipts) != 1 {
		t.Fatalf("receipts = %#v, want one validated receipt", response.Metadata.AttemptReceipts)
	}
	receipt := response.Metadata.AttemptReceipts.Receipts[0]
	if receipt.AccountLaneHash != LaneHashForConnection(connectionID) {
		t.Fatalf("receipt lane hash = %q, want derived hash %q", receipt.AccountLaneHash, LaneHashForConnection(connectionID))
	}
	if receipt.Provider != ProviderChatGPTWeb || receipt.Model != "chatgpt-web/model" {
		t.Fatalf("receipt provider/model = %q/%q, want chatgpt-web/chatgpt-web/model", receipt.Provider, receipt.Model)
	}
}

func TestCompletePinnedLaneRejectsWrongAccountLaneHash(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	const connectionID = "conn-test-123"
	client, server := pinnedLaneClient(t, "", connectionID, func(config *Config) {
		config.Model = "chatgpt-web/model"
	})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The producer selected a DIFFERENT connection than the pinned one.
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
				Model:           "chatgpt-web/model",
				AccountLaneHash: LaneHashForConnection("other-connection"),
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
		t.Fatalf("Complete() error = %v, want typed invalid-receipts error for lane mismatch", err)
	}
}

func TestCompletePinnedLaneRequiresConnectionPinBeforePOST(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("model POST must not be issued without a connection pin")
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client.config.EnableAttemptReceipts = true
	client.config.Provider = ProviderChatGPTWeb
	client.config.ConnectionID = ""
	client.config.Model = "chatgpt-web/model"
	client.config.AccountLaneHash = LaneHashForConnection("conn-test-123")
	client.config.ChatEndpoint = DedicatedChatEndpoint
	client.config.RouteSafety = provider.ReceiptRouteSafety()
	client.gatewayContractHealth = provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "test_fixture"}
	_, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt", ClientRequestID: "request-1"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorAttemptReceiptsInvalid {
		t.Fatalf("Complete() error = %v, want typed invalid error before any POST", err)
	}
}

func TestNewPinnedLaneRejectsArbitraryChatEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()
	config := testConfig(server.URL)
	config.EnableAttemptReceipts = true
	config.Provider = ProviderChatGPTWeb
	config.ConnectionID = "conn-test-123"
	config.AccountLaneHash = LaneHashForConnection("conn-test-123")
	config.ChatEndpoint = "chat/completions"
	config.RouteSafety = provider.ReceiptRouteSafety()
	if _, err := New(config, Options{HTTPClient: server.Client()}); err == nil {
		t.Fatal("New() with an arbitrary chat endpoint on the pinned lane must fail closed")
	}
}

func TestNewPinnedLaneComposesDedicatedEndpointFromV1Base(t *testing.T) {
	base, err := normalizeBaseURL("https://omniroute.invalid:20128/v1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := chatURL(base, DedicatedChatEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://omniroute.invalid:20128/v1/providers/chatgpt-web/chat/completions" {
		t.Fatalf("dedicated chat URL = %q, want /v1/providers/chatgpt-web/chat/completions", got)
	}
}
