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
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\",\"logprobs\":[]}\n\n")
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

func TestCompleteAcceptsOfficialResponsesSSEFieldsAndIgnoresReasoning(t *testing.T) {
	access := &testAccess{token: "fixture-token"}
	var requests atomic.Int32
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(name, data string) {
			t.Helper()
			_, _ = io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n")
		}
		writeEvent("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_123","status":"in_progress","output":[]}}`)
		writeEvent("response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`)
		writeEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","sequence_number":2,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"private rationale"}`)
		writeEvent("response.reasoning_summary_text.done", `{"type":"response.reasoning_summary_text.done","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"text":"private rationale"}`)
		writeEvent("response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"inert reasoning"}`)
		writeEvent("response.reasoning_text.done", `{"type":"response.reasoning_text.done","sequence_number":5,"item_id":"rs_1","output_index":0,"content_index":0,"text":"inert reasoning"}`)
		writeEvent("response.output_item.done", `{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"private rationale"}],"content":[{"type":"reasoning_text","text":"inert reasoning"}]}}`)
		writeEvent("response.output_item.added", `{"type":"response.output_item.added","sequence_number":7,"output_index":1,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		writeEvent("response.content_part.added", `{"type":"response.content_part.added","sequence_number":8,"item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`)
		writeEvent("response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":9,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"hello","logprobs":[]}`)
		writeEvent("response.output_text.done", `{"type":"response.output_text.done","sequence_number":10,"item_id":"msg_1","output_index":1,"content_index":0,"text":"hello","logprobs":[]}`)
		writeEvent("response.content_part.done", `{"type":"response.content_part.done","sequence_number":11,"item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}}`)
		writeEvent("response.output_item.done", `{"type":"response.output_item.done","sequence_number":12,"output_index":1,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}}`)
		writeEvent("response.completed", `{"type":"response.completed","sequence_number":13,"response":{"id":"resp_123","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"private rationale"}],"content":[{"type":"reasoning_text","text":"inert reasoning"}]},{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]}}`)
	}, access)
	defer server.Close()

	response, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\nsystem contract\n=== runstead:user ===\nhello context\n"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "hello" || strings.Contains(response.Text, "private rationale") || response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("response=%+v", response)
	}
	if requests.Load() != 1 || access.calls.Load() != 1 {
		t.Fatalf("HTTP=%d custody=%d, want one each", requests.Load(), access.calls.Load())
	}
}

func TestCompleteAcceptsRepeated70KiBOfficialTerminalText(t *testing.T) {
	const size = 70 << 10
	text := strings.Repeat("a", size)
	access := &testAccess{token: "fixture-token"}
	var posts atomic.Int32
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		sequence := 0
		writeEvent := func(name string, payload any) {
			t.Helper()
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(w, "event: "+name+"\ndata: "+string(encoded)+"\n\n"); err != nil {
				t.Error(err)
			}
			sequence++
		}
		writeEvent("response.created", map[string]any{"type": "response.created", "sequence_number": sequence, "response": map[string]any{"id": "resp_large", "status": "in_progress", "output": []any{}}})
		writeEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "sequence_number": sequence, "output_index": 0, "item": map[string]any{"id": "msg_large", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
		writeEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "sequence_number": sequence, "item_id": "msg_large", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
		for start := 0; start < len(text); start += 1024 {
			end := start + 1024
			if end > len(text) {
				end = len(text)
			}
			writeEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": sequence, "item_id": "msg_large", "output_index": 0, "content_index": 0, "delta": text[start:end], "logprobs": []any{}})
		}
		textPart := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
		writeEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "sequence_number": sequence, "item_id": "msg_large", "output_index": 0, "content_index": 0, "text": text, "logprobs": []any{}})
		writeEvent("response.content_part.done", map[string]any{"type": "response.content_part.done", "sequence_number": sequence, "item_id": "msg_large", "output_index": 0, "content_index": 0, "part": textPart})
		message := map[string]any{"id": "msg_large", "type": "message", "status": "completed", "role": "assistant", "content": []any{textPart}}
		writeEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "sequence_number": sequence, "output_index": 0, "item": message})
		writeEvent("response.completed", map[string]any{"type": "response.completed", "sequence_number": sequence, "response": map[string]any{"id": "resp_large", "status": "completed", "output": []any{message}}})
	}, access)
	defer server.Close()

	response, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nlarge local fixture\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Text) != size || response.Text != text || response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("text bytes=%d want %d delivery=%s", len(response.Text), size, response.Metadata.DeliveryState)
	}
	if posts.Load() != 1 || access.calls.Load() != 1 {
		t.Fatalf("POSTs=%d custody calls=%d, want one each", posts.Load(), access.calls.Load())
	}
}

func TestCompleteRejectsTextAboveEffectiveResponseLimitWithoutPartialTextOrRetry(t *testing.T) {
	const limit = 70 << 10
	text := strings.Repeat("b", limit+1)
	access := &testAccess{token: "fixture-token"}
	var posts atomic.Int32
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for start := 0; start < len(text); start += 1024 {
			end := start + 1024
			if end > len(text) {
				end = len(text)
			}
			payload := map[string]any{"type": "response.output_text.delta", "item_id": "msg_over", "output_index": 0, "content_index": 0, "delta": text[start:end]}
			encoded, _ := json.Marshal(payload)
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: "+string(encoded)+"\n\n")
		}
	}, access)
	defer server.Close()
	client.maxResponse = limit

	response, err := client.Complete(context.Background(), provider.Request{Model: "exact-model", Prompt: "=== runstead:system ===\ns\n=== runstead:user ===\nlarge fixture\n"})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorResponseTooLarge || response.Text != "" {
		t.Fatalf("text bytes=%d err=%v, want typed too-large error and no partial text", len(response.Text), err)
	}
	if posts.Load() != 1 || access.calls.Load() != 1 {
		t.Fatalf("POSTs=%d custody calls=%d, want one each", posts.Load(), access.calls.Load())
	}
}

func TestValidateLogprobsAcceptsDocumentedByteArrays(t *testing.T) {
	raw := json.RawMessage(`[{"token":"é","bytes":[195,169],"logprob":-0.25,"top_logprobs":[{"token":"é","bytes":[195,169],"logprob":-0.25}]}]`)
	if err := validateLogprobs(raw); err != nil {
		t.Fatalf("documented logprobs bytes were rejected: %v", err)
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
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}

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
		{"completed text mismatch", `event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"streamed"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"different","annotations":[],"logprobs":[]}]}]}}

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
		"aggregate output limit": "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"four\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			limit := 0
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
