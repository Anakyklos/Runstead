package omniroute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestPreflightFailsClosedForMissingOrUnsafeResilienceEvidence(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing stream recovery", body: strings.Replace(safeResilienceResponse, "\n  \"streamRecovery\": {\"enabled\": false, \"midstreamEnabled\": false}", "", 1)},
		{name: "concurrent requests", body: strings.Replace(safeResilienceResponse, `"concurrentRequests": 1`, `"concurrentRequests": 2`, 1)},
		{name: "cooldown retry", body: strings.Replace(safeResilienceResponse, `"maxRetries": 0`, `"maxRetries": 1`, 1)},
		{name: "combo replay", body: strings.Replace(safeResilienceResponse, `"maxAttempts": 0`, `"maxAttempts": 1`, 1)},
		{name: "provider cooldown", body: strings.Replace(safeResilienceResponse, `"providerCooldown": {"enabled": false}`, `"providerCooldown": {"enabled": true}`, 1)},
		{name: "unknown setting", body: strings.Replace(safeResilienceResponse, "\n}", ",\n  \"unknownSetting\": {\"enabled\": false}\n}", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/resilience" {
					io.WriteString(w, tt.body)
					return
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"should not run"}}]}`)
			}))
			defer server.Close()
			client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Preflight(context.Background()); !errors.Is(err, provider.ErrUnsafeRoute) {
				t.Fatalf("Preflight() error = %v, want unsafe route", err)
			}
			if client.RouteSafety().Validate() == nil {
				t.Fatalf("RouteSafety after unsafe preflight = %#v, want unknown", client.RouteSafety())
			}
		})
	}
}

func TestPreflightClearsPreviouslyVerifiedRouteOnDrift(t *testing.T) {
	var safe atomic.Bool
	safe.Store(true)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/resilience" {
			if safe.Load() {
				io.WriteString(w, safeResilienceResponse)
			} else {
				io.WriteString(w, strings.Replace(safeResilienceResponse, `"enabled": false`, `"enabled": true`, 1))
			}
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			posts.Add(1)
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
		}
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); err != nil {
		t.Fatal(err)
	}
	safe.Store(false)
	if err := client.Preflight(context.Background()); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("drifted Preflight() error = %v, want unsafe route", err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("Complete() after drift = %v, want unsafe route", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("chat POSTs = %d, want one before drift", posts.Load())
	}
}

func TestNewRejectsNonExplicitSingleTargetModel(t *testing.T) {
	for _, model := range []string{"auto", "priority-combo", "weighted/fallback"} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig("http://127.0.0.1:1")
			cfg.Model = model
			if _, err := New(cfg, Options{}); !errors.Is(err, provider.ErrUnsafeRoute) {
				t.Fatalf("New(%q) error = %v, want unsafe route", model, err)
			}
		})
	}
}
