package omniroute

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func newGatewayContractHealthTestClient(t *testing.T, server *contractMockServer, configure func(*Config)) *Client {
	t.Helper()
	config := testConfig(server.URL())
	if configure != nil {
		configure(&config)
	}
	client, err := New(config, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGatewayContractHealthStartsUnknown(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	got := client.GatewayContractHealth()
	if got.State != provider.GatewayContractHealthUnknown {
		t.Fatalf("initial health = %#v, want unknown", got)
	}
}

func TestProbeGatewayContractRecognizesThreeManagementFixtures(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	got := client.ProbeGatewayContract(context.Background())
	if got.State != provider.GatewayContractHealthHealthy {
		t.Fatalf("probe health = %#v, want healthy", got)
	}
	if got.ReasonCode != "recognized" {
		t.Fatalf("probe reason = %q, want recognized", got.ReasonCode)
	}
	counts := server.Counts()
	if counts["management_gets"] != 3 || counts["chat_posts"] != 0 || counts["total"] != 3 {
		t.Fatalf("probe counts = %#v, want exactly three management GETs and no chat POST", counts)
	}
	requests := server.Requests()
	paths := make([]string, 0, len(requests))
	for _, request := range requests {
		if request.Method != http.MethodGet {
			t.Fatalf("probe request method = %s, want GET", request.Method)
		}
		if cookie := request.Headers.Get("Cookie"); cookie != "" {
			t.Fatalf("probe sent cookie %q", cookie)
		}
		paths = append(paths, request.Path)
	}
	if want := []string{providersPath, settingsPath, modelAliasesPath}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("probe paths = %#v, want %#v", paths, want)
	}
	if latest := client.GatewayContractHealth(); latest != got {
		t.Fatalf("latest health = %#v, want %#v", latest, got)
	}
}

func TestProbeGatewayContractClassifiesProtocolChanges(t *testing.T) {
	tests := []struct {
		name       string
		management map[string]contractMockResponse
		wantReason string
	}{
		{
			name: "settings shape drift",
			management: map[string]contractMockResponse{
				"settings": {status: http.StatusOK, body: []byte(mustReadContractFixture("management/settings-shape-drift.json"))},
			},
			wantReason: "missing_or_invalid_field",
		},
		{
			name: "ambiguous provider evidence",
			management: map[string]contractMockResponse{
				"providers": {status: http.StatusOK, body: []byte(mustReadContractFixture("management/providers-ambiguous.json"))},
			},
			wantReason: "provider_model_ambiguous",
		},
		{
			name: "malformed json",
			management: map[string]contractMockResponse{
				"providers": {status: http.StatusOK, body: []byte(`{"connections":`)},
			},
			wantReason: "malformed_json",
		},
		{
			name: "malformed shape",
			management: map[string]contractMockResponse{
				"model_aliases": {status: http.StatusOK, body: []byte(`{"aliases":[]}`)},
			},
			wantReason: "missing_or_invalid_field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newContractMockServer(t, contractMockConfig{management: tt.management})
			defer server.Close()
			client := newGatewayContractHealthTestClient(t, server, nil)

			got := client.ProbeGatewayContract(context.Background())
			if got.State != provider.GatewayContractHealthProtocolChanged {
				t.Fatalf("probe health = %#v, want protocol_changed", got)
			}
			if got.ReasonCode != tt.wantReason {
				t.Fatalf("probe reason = %q, want %q", got.ReasonCode, tt.wantReason)
			}
			if got.Endpoint == "" {
				t.Fatal("protocol change result has no fixed endpoint")
			}
			counts := server.Counts()
			if counts["chat_posts"] != 0 || counts["management_gets"] > 3 || counts["total"] > 3 {
				t.Fatalf("probe counts = %#v, want bounded management-only requests", counts)
			}
		})
	}
}

func TestProbeGatewayContractClassifiesGoneAndNotFoundAsProtocolChanged(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
				"providers": {status: status},
			}})
			defer server.Close()
			client := newGatewayContractHealthTestClient(t, server, nil)

			got := client.ProbeGatewayContract(context.Background())
			if got.State != provider.GatewayContractHealthProtocolChanged {
				t.Fatalf("probe health = %#v, want protocol_changed", got)
			}
			if got.ReasonCode != map[int]string{http.StatusNotFound: "http_404", http.StatusGone: "http_410"}[status] {
				t.Fatalf("probe reason = %q, want status-specific reason", got.ReasonCode)
			}
			counts := server.Counts()
			if counts["management_gets"] != 1 || counts["chat_posts"] != 0 || counts["redirect_replays"] != 0 {
				t.Fatalf("probe counts = %#v, want one bounded GET and no replay", counts)
			}
		})
	}
}

func TestProbeGatewayContractClassifiesRecognizedTemporaryHTTPAsDegraded(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
		"providers": {status: http.StatusServiceUnavailable, body: []byte(`synthetic temporary failure`)},
	}})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	got := client.ProbeGatewayContract(context.Background())
	if got.State != provider.GatewayContractHealthDegraded {
		t.Fatalf("probe health = %#v, want degraded", got)
	}
	if got.ReasonCode != "temporary_http_status" {
		t.Fatalf("probe reason = %q, want temporary_http_status", got.ReasonCode)
	}
	if strings.Contains(got.ReasonCode, "synthetic") {
		t.Fatalf("probe reason leaked response content: %q", got.ReasonCode)
	}
}

func TestProbeGatewayContractTimeoutAndCancellationRemainUnknown(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
			"providers": {transport: contractTransportSpec{Kind: "delay", DelayMS: 100}},
		}})
		defer server.Close()
		client := newGatewayContractHealthTestClient(t, server, func(config *Config) { config.Timeout = 15 * time.Millisecond })

		got := client.ProbeGatewayContract(context.Background())
		if got.State != provider.GatewayContractHealthUnknown || got.ReasonCode != "timeout" {
			t.Fatalf("timeout health = %#v, want unknown/timeout", got)
		}
		if counts := server.Counts(); counts["management_gets"] != 1 || counts["chat_posts"] != 0 {
			t.Fatalf("timeout counts = %#v", counts)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		server := newContractMockServer(t, contractMockConfig{})
		defer server.Close()
		client := newGatewayContractHealthTestClient(t, server, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		got := client.ProbeGatewayContract(ctx)
		if got.State != provider.GatewayContractHealthUnknown || got.ReasonCode != "context_cancelled" {
			t.Fatalf("cancellation health = %#v, want unknown/context_cancelled", got)
		}
		if counts := server.Counts(); counts["total"] != 0 || counts["chat_posts"] != 0 {
			t.Fatalf("cancellation counts = %#v, want no requests", counts)
		}
	})
}

func TestProbeGatewayContractRedirectDoesNotReplay(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
		"providers": {
			status:    http.StatusTemporaryRedirect,
			headers:   http.Header{"Location": []string{"/api/providers-replayed"}},
			transport: contractTransportSpec{Kind: "redirect", Location: "/api/providers-replayed"},
		},
	}})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	got := client.ProbeGatewayContract(context.Background())
	if got.State == provider.GatewayContractHealthHealthy {
		t.Fatalf("redirect health = %#v, must not be healthy", got)
	}
	counts := server.Counts()
	if counts["total"] != 1 || counts["management_gets"] != 1 || counts["redirect_replays"] != 0 || counts["chat_posts"] != 0 {
		t.Fatalf("redirect counts = %#v, want one non-replayed GET", counts)
	}
}

func TestProbeGatewayContractDoesNotExposeTransportOrBodyDetails(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
		"providers": {status: http.StatusBadGateway, body: []byte(`api-key=synthetic-secret body=private`)},
	}})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	got := client.ProbeGatewayContract(context.Background())
	if got.State != provider.GatewayContractHealthDegraded {
		t.Fatalf("health = %#v", got)
	}
	encoded := got.State.String() + " " + got.ReasonCode + " " + got.Endpoint
	for _, forbidden := range []string{"synthetic-secret", "private", "api-key"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("health result leaked %q: %q", forbidden, encoded)
		}
	}
}

func TestProbeGatewayContractUsesNoRetryOnTransientResponse(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{management: map[string]contractMockResponse{
		"providers": {status: http.StatusBadGateway},
	}})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)

	_ = client.ProbeGatewayContract(context.Background())
	if counts := server.Counts(); counts["management_gets"] != 1 || counts["total"] != 1 {
		t.Fatalf("transient response counts = %#v, want one request and no retry", counts)
	}
}

func TestProbeGatewayContractContextErrorClassificationDoesNotUseCompletion(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{})
	defer server.Close()
	client := newGatewayContractHealthTestClient(t, server, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := client.ProbeGatewayContract(ctx)
	if !errors.Is(context.Canceled, context.Canceled) || result.State != provider.GatewayContractHealthUnknown {
		t.Fatalf("context result = %#v", result)
	}
	if got := server.Counts()["chat_posts"]; got != 0 {
		t.Fatalf("context probe chat POSTs = %d, want 0", got)
	}
}
