package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// The proposed singleAttemptContract is intentionally present in this
// fixture so tests prove it cannot authorize production execution.
const safeResilienceResponse = `{
  "requestQueue": {"concurrentRequests": 1, "minTimeBetweenRequestsMs": 0, "maxWaitMs": 15000, "maxQueueDepth": 0},
  "singleAttemptContract": {
    "version": 1,
    "guaranteed": true,
    "internalRetries": false,
    "credentialRefreshRetry": false,
    "cooldownReplay": false,
    "accountPooling": false,
    "automaticFallback": false
  },
  "connectionCooldown": {
    "oauth": {"baseCooldownMs": 0, "useUpstreamRetryHints": false, "useUpstream429BreakerHints": false, "maxBackoffSteps": 0},
    "apikey": {"baseCooldownMs": 0, "useUpstreamRetryHints": false, "useUpstream429BreakerHints": false, "maxBackoffSteps": 0}
  },
  "providerBreaker": {
    "oauth": {"failureThreshold": 1, "degradationThreshold": 0, "resetTimeoutMs": 1000},
    "apikey": {"failureThreshold": 1, "degradationThreshold": 0, "resetTimeoutMs": 1000}
  },
  "waitForCooldown": {"enabled": false, "maxRetries": 0, "maxRetryWaitSec": 0},
  "comboCooldownWait": {"enabled": false, "maxAttempts": 0, "maxWaitMs": 0},
  "quotaShareConcurrencyLimit": {"enabled": false},
  "providerCooldown": {"enabled": false, "minRetryCooldownMs": 0, "maxRetryCooldownMs": 0},
  "legacy": {"requestRetry": 0, "maxRetryIntervalSec": 0}
}`

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL + "/v1",
		APIKey:           "secret-api-key",
		Model:            "chatgpt-web/model",
		Timeout:          time.Second,
		MaxRequestBytes:  1 << 20,
		MaxResponseBytes: 1 << 20,
		RouteSafety:      provider.SafeRouteSafety(),
	}
}

func newTransportClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func safeHandler(chat http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/resilience", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, safeResilienceResponse)
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"wildcardAliases":[],"modelAliases":{},"globalFallbackModel":""}`)
	})
	mux.HandleFunc("/api/models/alias", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"aliases":{}}`)
	})
	mux.HandleFunc("/api/settings/model-aliases", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"builtIn":{},"custom":{},"all":{}}`)
	})
	mux.HandleFunc("/api/fallback/chains", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/combos", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"combos":[],"total":0}`)
	})
	mux.HandleFunc("/api/model-combo-mappings", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"mappings":[],"total":0}`)
	})
	mux.HandleFunc("/api/providers", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"connections":[{"id":"account-1","provider":"chatgpt-web","isActive":true,"defaultModel":"model"}],"total":1}`)
	})
	mux.HandleFunc("/v1/chat/completions", chat)
	return mux
}

func TestCompleteTransportSendsOneMinimalNonStreamingRequestAndPreservesModelText(t *testing.T) {
	var posts atomic.Int32
	var gotAuthorization string
	var gotNoCache string
	var gotBody map[string]any
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		gotAuthorization = r.Header.Get("Authorization")
		gotNoCache = r.Header.Get("X-OmniRoute-No-Cache")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("request JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "request-opaque-1")
		w.Header().Set("X-OmniRoute-Session-Id", "private-session")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":" prose <runstead_final>text</runstead_final> "}}]}`)
	}))
	defer server.Close()

	response, err := client.completeOnce(context.Background(), provider.Request{
		Protocol: protocol.Current,
		Prompt:   "private prompt",
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if response.Text != " prose <runstead_final>text</runstead_final> " {
		t.Fatalf("response text = %q, want exact provider content", response.Text)
	}
	if posts.Load() != 1 {
		t.Fatalf("chat POSTs = %d, want 1", posts.Load())
	}
	if gotAuthorization != "Bearer secret-api-key" {
		t.Fatalf("Authorization = %q", gotAuthorization)
	}
	if gotNoCache != "true" {
		t.Fatalf("X-OmniRoute-No-Cache = %q, want true", gotNoCache)
	}
	wantBody := map[string]any{
		"model": "chatgpt-web/model",
		"messages": []any{
			map[string]any{"role": "user", "content": "private prompt"},
		},
		"stream": false,
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Fatalf("request body = %#v, want %#v", gotBody, wantBody)
	}
	if response.Metadata.StatusCode != http.StatusOK || response.Metadata.RequestID != "request-opaque-1" {
		t.Fatalf("response metadata = %#v", response.Metadata)
	}
	if response.Metadata.SessionID == "private-session" || response.Metadata.SessionID == "" {
		t.Fatalf("session metadata was not sanitized: %#v", response.Metadata)
	}
}

func TestCompleteRejectsBeforeAnyPreflightOrChat(t *testing.T) {
	var requests atomic.Int32
	baseHandler := safeHandler(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		baseHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.RouteSafety(); got.Validate() == nil {
		t.Fatalf("RouteSafety before preflight = %#v, want unknown", got)
	}
	if _, err = client.Complete(context.Background(), provider.Request{Prompt: "prompt"}); !errors.Is(err, provider.ErrUnsafeRoute) {
		t.Fatalf("Complete() error = %v, want unsafe route until receipts exist", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests after blocked Complete() = %d, want 0", requests.Load())
	}
	if got := client.RouteSafety(); got.Validate() == nil {
		t.Fatalf("RouteSafety after blocked Complete() = %#v, want unknown", got)
	}
}

func TestCompleteTransportAcceptsMixedProseAndRefusalAsProviderText(t *testing.T) {
	for _, content := range []string{
		"before <runstead_final>not a protocol response</runstead_final> after",
		"I cannot comply with that request.",
	} {
		t.Run(content, func(t *testing.T) {
			client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content))
			}))
			defer server.Close()
			response, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
			if err != nil || response.Text != content {
				t.Fatalf("Complete() = (%q, %v), want provider text", response.Text, err)
			}
		})
	}
}

func TestCompleteTransportClassifiesMalformedAndEmptyResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want ErrorKind
	}{
		{name: "empty body", body: "", want: ErrorEmptyResponse},
		{name: "invalid json", body: "{", want: ErrorMalformedJSON},
		{name: "choices absent", body: `{}`, want: ErrorInvalidEnvelope},
		{name: "message content absent", body: `{"choices":[{"message":{}}]}`, want: ErrorInvalidEnvelope},
		{name: "empty content", body: `{"choices":[{"message":{"content":""}}]}`, want: ErrorEmptyResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			_, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Kind != tt.want {
				t.Fatalf("Complete() error = %T %v, want kind %s", err, err, tt.want)
			}
		})
	}
}

func TestCompleteTransportClassifiesHTTPFailuresWithoutRetry(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		headers   map[string]string
		want      ErrorKind
		wantRetry time.Duration
	}{
		{name: "expired authentication", status: http.StatusUnauthorized, body: `{"error":{"code":"token_expired"}}`, want: ErrorAuthenticationExpired},
		{name: "denied authentication", status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key"}}`, want: ErrorAuthenticationDenied},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, want: ErrorHTTP403},
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"error":{"code":"rate_limit_exceeded"}}`, headers: map[string]string{"Retry-After": "7"}, want: ErrorRateCapacity, wantRetry: 7 * time.Second},
		{name: "request timeout", status: http.StatusRequestTimeout, body: `{}`, want: ErrorTimeout},
		{name: "upstream failure", status: http.StatusBadGateway, body: `{}`, want: ErrorUpstreamServerFailure},
		{name: "login challenge", status: http.StatusUnauthorized, body: `{"error":{"code":"login_required"}}`, want: ErrorLoginChallenge},
		{name: "captcha", status: http.StatusForbidden, body: `{"error":{"message":"captcha required"}}`, want: ErrorCAPTCHA},
		{name: "suspicious activity", status: http.StatusForbidden, body: `{"error":{"code":"suspicious_activity"}}`, want: ErrorSuspiciousActivity},
		{name: "account warning", status: http.StatusForbidden, body: `{"error":{"code":"account_warning"}}`, want: ErrorAccountWarning},
		{name: "feature restriction", status: http.StatusBadRequest, body: `{"error":{"code":"feature_restricted"}}`, want: ErrorFeatureRestriction},
		{name: "connection reset signal", status: http.StatusBadGateway, body: `{"error":{"code":"connection_reset"}}`, want: ErrorConnectionReset},
		{name: "plain text expired", status: http.StatusUnauthorized, body: "token expired", want: ErrorAuthenticationExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts atomic.Int32
			client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			_, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Kind != tt.want {
				t.Fatalf("Complete() error = %T %v, want kind %s", err, err, tt.want)
			}
			if providerErr.RetryAfter != tt.wantRetry {
				t.Fatalf("RetryAfter = %s, want %s", providerErr.RetryAfter, tt.wantRetry)
			}
			if posts.Load() != 1 {
				t.Fatalf("chat POSTs = %d, want exactly one", posts.Load())
			}
			if strings.Contains(err.Error(), "secret-api-key") {
				t.Fatalf("error leaked API key: %v", err)
			}
		})
	}
}

func TestCompleteTransportRejectsOversizedResponseWithoutRetainingIt(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(safeHandler(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat("x", 8192)+`"}}]}`)
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.MaxResponseBytes = 4096
	client, err := New(cfg, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorResponseTooLarge {
		t.Fatalf("Complete() error = %T %v, want response-too-large", err, err)
	}
	if posts.Load() != 1 {
		t.Fatalf("chat POSTs = %d, want 1", posts.Load())
	}
}

func TestCompleteTransportDoesNotFollowRedirectsOrReplayThePOST(t *testing.T) {
	var posts atomic.Int32
	var replayed atomic.Int32
	baseHandler := safeHandler(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Location", "/replayed")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/replayed" {
			replayed.Add(1)
			return
		}
		baseHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorHTTPStatus {
		t.Fatalf("Complete() error = %T %v, want redirect status", err, err)
	}
	if posts.Load() != 1 || replayed.Load() != 0 {
		t.Fatalf("redirect requests = post %d, replay %d; want 1, 0", posts.Load(), replayed.Load())
	}
}

func TestCompleteTransportHonorsCancellationAndClientTimeout(t *testing.T) {
	server := httptest.NewServer(safeHandler(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.Timeout = 20 * time.Millisecond
	client, err := New(cfg, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	var timeoutErr *Error
	if !errors.As(err, &timeoutErr) || timeoutErr.Kind != ErrorTimeout {
		t.Fatalf("timeout Complete() error = %T %v, want timeout", err, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.completeOnce(ctx, provider.Request{Prompt: "prompt"})
	var cancelledErr *Error
	if !errors.As(err, &cancelledErr) || cancelledErr.Kind != ErrorCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Complete() error = %T %v, want cancellation", err, err)
	}
}

func TestConfigRedactsAPIKeyFromDiagnostics(t *testing.T) {
	cfg := testConfig("https://omniroute.example/v1")
	if strings.Contains(cfg.String(), cfg.APIKey) || strings.Contains(fmt.Sprintf("%#v", cfg), cfg.APIKey) {
		t.Fatal("Config diagnostics leaked API key")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cfg.APIKey) {
		t.Fatal("Config JSON leaked API key")
	}
}

func TestCompleteTransportRejectsOversizedRequestBeforePOST(t *testing.T) {
	var posts atomic.Int32
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()
	client.config.MaxRequestBytes = 1
	_, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Kind != ErrorRequestTooLarge {
		t.Fatalf("Complete() error = %T %v, want request-too-large", err, err)
	}
	if posts.Load() != 0 {
		t.Fatalf("chat POSTs = %d, want zero", posts.Load())
	}
}
