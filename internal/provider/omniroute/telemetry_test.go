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

	"github.com/RenyEnnos/Runstead/internal/governor"
)

func TestSnapshotMapsOnlyPresentTelemetryFields(t *testing.T) {
	reset := time.Date(2026, time.August, 3, 13, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/rate-limits":
			json.NewEncoder(w).Encode(map[string]any{
				"overview": map[string]any{
					"remaining": 4,
					"resetAt":   reset.Format(time.RFC3339),
				},
			})
		case "/api/resilience":
			json.NewEncoder(w).Encode(map[string]any{
				"rateLimited":       true,
				"capacityExhausted": true,
				"retryAfter":        9,
				"upstreamCircuit":   "open",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Remaining == nil || *snapshot.Remaining != 4 || !snapshot.ResetAt.Equal(reset) || snapshot.RetryAfter != 9*time.Second || !snapshot.RateLimited || !snapshot.CapacityExhausted || snapshot.UpstreamCircuit != governor.UpstreamCircuitOpen {
		t.Fatalf("Snapshot() = %#v, want mapped fields", snapshot)
	}
	if !snapshot.CooldownUntil.IsZero() {
		t.Fatalf("Snapshot() cooldown = %v, want absent", snapshot.CooldownUntil)
	}
}

func TestSnapshotTreatsManagementAuthFailureAsOptionalTelemetryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Snapshot(context.Background())
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorTelemetry {
		t.Fatalf("Snapshot() error = %T %v, want telemetry error", err, err)
	}
	if strings.Contains(err.Error(), "secret-api-key") {
		t.Fatalf("telemetry error leaked API key: %v", err)
	}
}

func TestSnapshotAllowsValidEmptyOptionalObjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Remaining != nil || snapshot.RateLimited || snapshot.CapacityExhausted || snapshot.UpstreamCircuit != governor.UpstreamCircuitUnknown {
		t.Fatalf("empty telemetry = %#v, want no optional values", snapshot)
	}
}

func TestSnapshotRejectsMalformedOptionalTelemetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/rate-limits" {
			io.WriteString(w, "{")
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Snapshot(context.Background())
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorTelemetry {
		t.Fatalf("Snapshot() error = %v, want telemetry error", err)
	}
}
