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

type reviewRouteState struct {
	resilience   string
	settings     string
	aliases      string
	modelAliases string
	fallbacks    string
	combos       string
	mappings     string
	providers    string
}

func newReviewRouteState() *reviewRouteState {
	return &reviewRouteState{
		resilience:   safeResilienceResponse,
		settings:     `{"wildcardAliases":[],"modelAliases":{},"globalFallbackModel":""}`,
		aliases:      `{"aliases":{}}`,
		modelAliases: `{"builtIn":{},"custom":{},"all":{}}`,
		fallbacks:    `[]`,
		combos:       `{"combos":[],"total":0}`,
		mappings:     `{"mappings":[],"total":0}`,
		providers:    `{"connections":[{"id":"account-1","provider":"chatgpt-web","isActive":true,"defaultModel":"model"}],"total":1}`,
	}
}

func reviewHandler(state *reviewRouteState, posts *atomic.Int32) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resilience", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.resilience)
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.settings)
	})
	mux.HandleFunc("/api/models/alias", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.aliases)
	})
	mux.HandleFunc("/api/settings/model-aliases", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.modelAliases)
	})
	mux.HandleFunc("/api/fallback/chains", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.fallbacks)
	})
	mux.HandleFunc("/api/combos", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.combos)
	})
	mux.HandleFunc("/api/model-combo-mappings", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.mappings)
	})
	mux.HandleFunc("/api/providers", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, state.providers)
	})
	mux.HandleFunc("/api/rate-limits", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	return mux
}

func newReviewClient(t *testing.T, state *reviewRouteState, posts *atomic.Int32) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(reviewHandler(state, posts))
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func TestCompleteRejectsProductionExecutionUntilAuthoritativeAttemptReceipts(t *testing.T) {
	state := newReviewRouteState()
	var posts atomic.Int32
	client, server := newReviewClient(t, state, &posts)
	defer server.Close()

	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("Complete() error = %v, want unsafe route without authoritative receipts", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("chat POSTs with synthetic contract = %d, want 0", posts.Load())
	}
	if client.RouteSafety().Validate() == nil {
		t.Fatalf("RouteSafety with synthetic contract = %#v, want unknown", client.RouteSafety())
	}
}

func TestPreflightRejectsProposedSingleAttemptContractUntilReceiptsExist(t *testing.T) {
	state := newReviewRouteState()
	client, server := newReviewClient(t, state, &atomic.Int32{})
	defer server.Close()

	if err := client.Preflight(context.Background()); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("Preflight() error = %v, want unsafe route until receipts exist", err)
	}
	if client.RouteSafety().Validate() == nil {
		t.Fatalf("RouteSafety after proposed contract = %#v, want unknown", client.RouteSafety())
	}
}

func TestPreflightRejectsWithoutProposedContractToo(t *testing.T) {
	state := newReviewRouteState()
	state.resilience = strings.Replace(
		safeResilienceResponse,
		`  "singleAttemptContract": {
    "version": 1,
    "guaranteed": true,
    "internalRetries": false,
    "credentialRefreshRetry": false,
    "cooldownReplay": false,
    "accountPooling": false,
    "automaticFallback": false
  },
`,
		"",
		1,
	)
	var posts atomic.Int32
	client, server := newReviewClient(t, state, &posts)
	defer server.Close()

	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("Complete() error = %v, want unsafe route without authoritative receipts", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("chat POSTs without proposed contract = %d, want 0", posts.Load())
	}
}

func TestPreflightRejectsNormalLookingAliasesFallbacksAndAccountPools(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*reviewRouteState)
	}{
		{
			name: "database alias",
			mutate: func(state *reviewRouteState) {
				state.aliases = `{"aliases":{"chatgpt-web/model":"chatgpt-web/other"}}`
			},
		},
		{
			name: "settings alias",
			mutate: func(state *reviewRouteState) {
				state.modelAliases = `{"builtIn":{},"custom":{"chatgpt-web/model":"chatgpt-web/other"},"all":{"chatgpt-web/model":"chatgpt-web/other"}}`
			},
		},
		{
			name: "wildcard alias",
			mutate: func(state *reviewRouteState) {
				state.settings = `{"wildcardAliases":[{"pattern":"chatgpt-web/*","target":"chatgpt-web/other"}],"modelAliases":{},"globalFallbackModel":""}`
			},
		},
		{
			name: "fallback chain",
			mutate: func(state *reviewRouteState) {
				state.fallbacks = `[{"model":"chatgpt-web/model","chain":["chatgpt-web/other"]}]`
			},
		},
		{
			name: "model combo mapping",
			mutate: func(state *reviewRouteState) {
				state.mappings = `{"mappings":[{"pattern":"chatgpt-web/*","comboId":"combo-1","enabled":true}],"total":1}`
			},
		},
		{
			name: "account pool",
			mutate: func(state *reviewRouteState) {
				state.providers = `{"connections":[{"id":"account-1","provider":"chatgpt-web","isActive":true},{"id":"account-2","provider":"chatgpt-web","isActive":true}],"total":2}`
			},
		},
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
		})
	}
}

func TestSnapshotNeverPublishesRouteSafetyWithoutReceipts(t *testing.T) {
	state := newReviewRouteState()
	client, server := newReviewClient(t, state, &atomic.Int32{})
	defer server.Close()
	if _, err := client.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot() with safe observable settings = %v", err)
	}
	if client.RouteSafety().Validate() == nil {
		t.Fatal("RouteSafety from management snapshots is verified")
	}
	state.resilience = `{"requestQueue":{"concurrentRequests":2}}`

	_, err := client.Snapshot(context.Background())
	var telemetryErr *Error
	if !errors.As(err, &telemetryErr) || telemetryErr.Kind != ErrorTelemetry {
		t.Fatalf("Snapshot() error = %T %v, want telemetry error", err, err)
	}
	if client.RouteSafety().Validate() == nil {
		t.Fatal("RouteSafety after unsafe telemetry remains verified")
	}
}

type opaqueReviewDoer struct{}

func (opaqueReviewDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("opaque doer should not be called")
}

func TestNewRejectsOpaqueHTTPDoer(t *testing.T) {
	if _, err := New(testConfig("http://127.0.0.1:1"), Options{HTTPClient: opaqueReviewDoer{}}); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("New() error = %v, want unsafe route for opaque HTTP client", err)
	}
}
