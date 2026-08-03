package omniroute

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestPreflightFailsClosedForMissingOrUnsafeResilienceEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*reviewRouteState)
	}{
		{name: "missing request queue", mutate: func(state *reviewRouteState) { state.resilience = `{}` }},
		{name: "concurrent requests", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"concurrentRequests": 1`, `"concurrentRequests": 2`, 1)
		}},
		{name: "cooldown retry", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"maxRetries": 0`, `"maxRetries": 1`, 1)
		}},
		{name: "combo replay", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"maxAttempts": 0`, `"maxAttempts": 1`, 1)
		}},
		{name: "provider cooldown", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"providerCooldown": {"enabled": false`, `"providerCooldown": {"enabled": true`, 1)
		}},
		{name: "legacy request retry", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"requestRetry": 0`, `"requestRetry": 1`, 1)
		}},
		{name: "legacy retry interval", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"maxRetryIntervalSec": 0`, `"maxRetryIntervalSec": 1`, 1)
		}},
		{name: "credential refresh retry", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"credentialRefreshRetry": false`, `"credentialRefreshRetry": true`, 1)
		}},
		{name: "single-attempt contract disabled", mutate: func(state *reviewRouteState) {
			state.resilience = strings.Replace(safeResilienceResponse, `"guaranteed": true`, `"guaranteed": false`, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newReviewRouteState()
			tt.mutate(state)
			client, server := newReviewClient(t, state, &atomic.Int32{})
			defer server.Close()
			if err := client.Preflight(context.Background()); !errors.Is(err, provider.ErrUnsafeRoute) {
				t.Fatalf("Preflight() error = %v, want unsafe route", err)
			}
			if client.RouteSafety().Validate() == nil {
				t.Fatalf("RouteSafety after unsafe preflight = %#v, want unknown", client.RouteSafety())
			}
		})
	}
}

func TestPreflightRejectsIncompleteSettingsEvidence(t *testing.T) {
	for _, settings := range []struct {
		name string
		body string
	}{
		{name: "empty", body: `{}`},
		{name: "missing wildcard aliases", body: `{"modelAliases":{},"globalFallbackModel":""}`},
		{name: "missing model aliases", body: `{"wildcardAliases":[],"globalFallbackModel":""}`},
		{name: "missing global fallback", body: `{"wildcardAliases":[],"modelAliases":{}}`},
	} {
		t.Run(settings.name, func(t *testing.T) {
			state := newReviewRouteState()
			state.settings = settings.body
			client, server := newReviewClient(t, state, &atomic.Int32{})
			defer server.Close()
			if err := client.Preflight(context.Background()); !errors.Is(err, provider.ErrUnsafeRoute) {
				t.Fatalf("Preflight() error = %v, want unsafe route for incomplete settings evidence", err)
			}
		})
	}
}

func TestPreflightAllowsUnrelatedResilienceFieldsWithContract(t *testing.T) {
	state := newReviewRouteState()
	state.resilience = strings.TrimSuffix(strings.TrimSpace(safeResilienceResponse), "}") + `,"unrelatedSetting":{"enabled":false}}`
	client, server := newReviewClient(t, state, &atomic.Int32{})
	defer server.Close()

	if err := client.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() error = %v, want unrelated current setting ignored", err)
	}
}

func TestPreflightClearsPreviouslyVerifiedRouteOnDrift(t *testing.T) {
	state := newReviewRouteState()
	var posts atomic.Int32
	client, server := newReviewClient(t, state, &posts)
	defer server.Close()
	if err := client.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); err != nil {
		t.Fatal(err)
	}
	state.resilience = strings.Replace(safeResilienceResponse, `"enabled": false`, `"enabled": true`, 1)
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

func TestNewDoesNotUseModelNameHeuristics(t *testing.T) {
	for _, model := range []string{"auto", "priority-combo", "weighted/fallback"} {
		t.Run(model, func(t *testing.T) {
			cfg := testConfig("http://127.0.0.1:1")
			cfg.Model = model
			if _, err := New(cfg, Options{}); err != nil {
				t.Fatalf("New(%q) error = %v, want name accepted for later route evidence", model, err)
			}
		})
	}
}
