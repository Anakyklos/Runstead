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
	if gotAuthorization != "Bearer fixture-api-key" {
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
