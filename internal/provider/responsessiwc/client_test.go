package responsessiwc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type testAccess struct {
	calls  atomic.Int32
	token  string
	err    error
	before func()
}

func (a *testAccess) WithAccessToken(ctx context.Context, clientID, model, account, credential string, request func(string) error) error {
	a.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.err != nil {
		return a.err
	}
	if clientID != "issued-client" || model != "exact-model" || account == "" || credential == "" {
		return errors.New("binding mismatch")
	}
	if a.before != nil {
		a.before()
	}
	return request(a.token)
}

func testResolved() provider.Resolved {
	return provider.Resolved{
		DocumentVersion: 2, WireContract: provider.WireResponsesSIWCV1,
		AccountBinding: "hmac-sha256:v1:" + strings.Repeat("a", 64), CredentialBinding: "hmac-sha256:v1:" + strings.Repeat("b", 64),
		ProviderID: "siwc-test", ProtocolFamily: provider.FamilyOpenAICompatible, BaseURL: "https://api.openai.com/v1", Model: "exact-model",
		Auth: provider.SecretRef("siwc:issued-client"), AuthRequirement: provider.AuthReferenceRequired, ConfigIdentity: "provider.v2:sha256:test",
		Profile: provider.CapabilityProfile{ProfileVersion: "v1", Capabilities: provider.Capabilities{provider.CapabilityTextTurn: true, provider.CapabilityRunsteadProtocol: true}, RouteSafety: provider.SafeRouteSafety(), MaxRequestBytes: 1 << 20, MaxResponseBytes: 4 << 20},
	}
}

func testClient(t *testing.T, handler http.HandlerFunc, access *testAccess) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := newClient(testResolved(), options{baseURL: server.URL + "/v1", transport: server.Client().Transport, access: access, now: time.Now})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func TestCompleteSendsResponsesWireAndRequiresCompletedSSE(t *testing.T) {
	access := &testAccess{token: "fixture-token"}
	var count atomic.Int32
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
			t.Errorf("authorization=%q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for key := range body {
			if key != "model" && key != "instructions" && key != "input" && key != "store" && key != "stream" {
				t.Errorf("unsupported field %q", key)
			}
		}
		var request struct {
			Model        string      `json:"model"`
			Instructions string      `json:"instructions"`
			Input        []wireInput `json:"input"`
			Store        bool        `json:"store"`
			Stream       bool        `json:"stream"`
		}
		_ = json.Unmarshal(mustJSON(t, body), &request)
		if request.Model != "exact-model" || request.Store || !request.Stream || request.Instructions != "system contract" || len(request.Input) != 1 || request.Input[0].Role != "user" || !strings.Contains(request.Input[0].Content, "runstead:user") {
			t.Errorf("request body=%+v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_123")
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"status\":\"completed\"}}\n\n")
	}, access)
	defer server.Close()
	response, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\nsystem contract\n=== runstead:user ===\nhello context\n", ClientRequestID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "hello" || response.Metadata.RequestID != "req_123" || response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("response=%+v", response)
	}
	if count.Load() != 1 || access.calls.Load() != 1 {
		t.Fatalf("HTTP=%d custody=%d, want one each", count.Load(), access.calls.Load())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSSERequiresOneValidTerminalAndRejectsPartialText(t *testing.T) {
	cases := []struct {
		name, body string
		want       ErrorKind
	}{
		{"failed", `event: response.failed
data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"subscription_sharing_usage_limit_exceeded"}}}

`, ErrorResponseFailed},
		{"incomplete", `event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}

`, ErrorResponseIncomplete},
		{"disconnected", `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"partial"}

`, ErrorStreamIncomplete},
		{"malformed", `event: response.output_text.delta
data: {bad}

`, ErrorMalformedResponse},
		{"duplicate key", `event: response.output_text.delta
data: {"type":"response.output_text.delta","type":"response.output_text.delta","delta":"partial"}

`, ErrorMalformedResponse},
		{"contradictory", `event: response.completed
data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}

event: response.failed
data: {"type":"response.failed","response":{"id":"r2","status":"failed"}}

`, ErrorInvalidEnvelope},
		{"unknown", `event: response.unknown
data: {"type":"response.unknown"}

`, ErrorInvalidEnvelope},
		{"unsupported tool item", `event: response.output_item.added
data: {"type":"response.output_item.added","item":{"type":"function_call","name":"shell","arguments":"{}"}}

`, ErrorInvalidEnvelope},
		{"refusal event", `event: response.refusal.delta
data: {"type":"response.refusal.delta","delta":"no"}

`, ErrorInvalidEnvelope},
		{"completed tool output", `event: response.completed
data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"function_call","name":"shell","arguments":"{}"}]}}

`, ErrorInvalidEnvelope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, err := parseSSE(strings.NewReader(tc.body), 100)
			var got *Error
			if !errors.As(err, &got) || got.Kind != tc.want {
				t.Fatalf("text=%q err=%v want %s", text, err, tc.want)
			}
			if text != "" {
				t.Fatalf("partial text escaped on failure: %q", text)
			}
		})
	}
}

func TestSSEBoundedLinesEventsAndAggregatedOutput(t *testing.T) {
	for name, body := range map[string]string{
		"line limit":             "event: " + strings.Repeat("x", maxLine) + "\n\n",
		"event limit":            strings.Repeat(":"+strings.Repeat("x", 60<<10)+"\n", 5) + "\n",
		"stream limit":           strings.Repeat(":"+strings.Repeat("x", 60<<10)+"\n\n", maxStream/(60<<10)+2),
		"aggregate output limit": "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"four\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			limit := maxOutput
			if name == "aggregate output limit" {
				limit = 3
			}
			_, err := parseSSE(strings.NewReader(body), limit)
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind != ErrorResponseTooLarge {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestPreflightAndHTTPFailuresNeverAmplify(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			access := &testAccess{token: "synthetic"}
			client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`)
			}, access)
			defer server.Close()
			_, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
			if err == nil {
				t.Fatal("expected failure")
			}
			if requests.Load() != 1 {
				t.Fatalf("requests=%d", requests.Load())
			}
		})
	}
	var calls atomic.Int32
	access := &testAccess{token: "unused", before: func() { calls.Add(1) }}
	client, server := testClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected dispatch") }, access)
	defer server.Close()
	_, err := client.Complete(context.Background(), provider.Request{Model: "different", Prompt: ""})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v custody=%d", err, calls.Load())
	}
	client.maxRequest = 2
	_, err = client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	var tooLarge *Error
	if !errors.As(err, &tooLarge) || tooLarge.Kind != ErrorRequestTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized preflight err=%v custody=%d", err, calls.Load())
	}
	var requests atomic.Int32
	badContent, badContentServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}, &testAccess{token: "synthetic"})
	defer badContentServer.Close()
	_, err = badContent.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	if !errors.As(err, &tooLarge) || tooLarge.Kind != ErrorInvalidEnvelope || requests.Load() != 1 {
		t.Fatalf("wrong content type err=%v requests=%d", err, requests.Load())
	}
}

func TestStalledStreamHonorsCallerTimeoutWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, &testAccess{token: "synthetic"})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Complete(ctx, provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorTimeout || typed.DeliveryState == provider.DeliveryNotSent || requests.Load() != 1 {
		t.Fatalf("stalled stream err=%v requests=%d", err, requests.Load())
	}
}

func TestHTTPFailurePreservesSanitizedRequestIDAndStatus(t *testing.T) {
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req_429")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"private body"}}`)
	}, &testAccess{token: "synthetic"})
	defer server.Close()
	response, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorRateCapacity || typed.Code != "subscription_sharing_usage_limit_exceeded" ||
		response.Metadata.StatusCode != http.StatusTooManyRequests || response.Metadata.RequestID != "req_429" ||
		strings.Contains(err.Error(), "private body") {
		t.Fatalf("response metadata=%+v typed=%+v err=%v", response.Metadata, typed, err)
	}
}

func TestConstructorRequiresCapabilitiesAndOpaqueBindings(t *testing.T) {
	for name, alter := range map[string]func(*provider.Resolved){
		"missing protocol capability": func(resolved *provider.Resolved) {
			delete(resolved.Profile.Capabilities, provider.CapabilityRunsteadProtocol)
		},
		"invalid account binding": func(resolved *provider.Resolved) { resolved.AccountBinding = "hmac-sha256:v1:bad" },
		"reused bindings":         func(resolved *provider.Resolved) { resolved.CredentialBinding = resolved.AccountBinding },
	} {
		t.Run(name, func(t *testing.T) {
			resolved := testResolved()
			alter(&resolved)
			if _, err := newClient(resolved, options{baseURL: "http://127.0.0.1:4444/v1", access: &testAccess{token: "unused"}}); err == nil {
				t.Fatal("invalid SIWC resolved contract was accepted")
			}
		})
	}
}

func TestCancelBeforeDispatchAndRedirectNotFollowed(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	access := &testAccess{token: "synthetic"}
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}, access)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Complete(ctx, provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	if err == nil || access.calls.Load() != 0 {
		t.Fatalf("cancel err=%v access=%d", err, access.calls.Load())
	}
	_, err = client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nu\n"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorUnsafeRedirect || targetCalls.Load() != 0 {
		t.Fatalf("redirect err=%v followed=%d", err, targetCalls.Load())
	}
}

func TestCancelAfterDispatchKeepsUncertainDeliveryAndOnePost(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	access := &testAccess{token: "offline-token"}
	client, err := newClient(testResolved(), options{baseURL: server.URL + "/v1", transport: server.Client().Transport, access: access})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		response provider.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := client.Complete(ctx, provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\nsystem\n=== runstead:user ===\ntask\n"})
		done <- result{response, err}
	}()
	<-started
	cancel()
	got := <-done
	var typed *Error
	state := got.response.Metadata.DeliveryState
	if !errors.As(got.err, &typed) || typed.Kind != ErrorCancelled ||
		(state != provider.DeliverySentUnconfirmed && state != provider.DeliveryResponseStarted) {
		t.Fatalf("result=%+v err=%v", got.response, got.err)
	}
	if requests.Load() != 1 {
		t.Fatalf("physical POSTs=%d, want one", requests.Load())
	}
}
