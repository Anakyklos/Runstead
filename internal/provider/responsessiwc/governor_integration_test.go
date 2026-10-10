package responsessiwc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestTwoGovernedCompletesSendSelfContainedTranscriptOnceEach(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer-%d\"}\n\n", n)
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-%d\",\"status\":\"completed\"}}\n\n", n)
	}))
	defer server.Close()
	access := &testAccess{token: "offline-token"}
	resolved := testResolved()
	client, err := newClient(resolved, options{baseURL: server.URL + "/v1", transport: server.Client().Transport, access: access})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultInstantConfig("siwc-account", resolved.ProviderID, "instant", provider.SafeRouteSafety())
	cfg.MinimumStartInterval = time.Nanosecond
	cfg.Rolling3h, cfg.Rolling1h, cfg.Rolling10m, cfg.ManualReserve, cfg.TaskBudget, cfg.RetryBudget = 100, 90, 80, 10, 4, 2
	g, err := policy.New(cfg, policy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	firstPrompt := "=== runstead:system ===\nlocal contract\n=== runstead:user ===\ninitial task\n"
	first := g.Execute(context.Background(), policy.AttemptRequest{TaskID: "task-siwc", ClientRequestID: "request-one", ModelPool: "instant", ProviderRequest: provider.Request{Model: resolved.Model, Prompt: firstPrompt}}, client, nil)
	if first.Err != nil || first.Completion.Err != nil || first.Response.Text != "answer-1" || !first.Admission.Admitted() {
		t.Fatalf("first governed completion=%+v", first)
	}
	secondPrompt := firstPrompt + "=== runstead:assistant ===\nanswer-1\n=== runstead:observation ===\n{\"success\":true}\n"
	second := g.Execute(context.Background(), policy.AttemptRequest{TaskID: "task-siwc", ClientRequestID: "request-two", ModelPool: "instant", ProviderRequest: provider.Request{Model: resolved.Model, Prompt: secondPrompt}}, client, nil)
	if second.Err != nil || second.Completion.Err != nil || second.Response.Text != "answer-2" || !second.Admission.Admitted() {
		t.Fatalf("second governed completion=%+v", second)
	}
	mu.Lock()
	got := append([]map[string]any(nil), requests...)
	mu.Unlock()
	if len(got) != 2 || access.calls.Load() != 2 {
		t.Fatalf("requests=%d credential scopes=%d, want two governed one-request calls", len(got), access.calls.Load())
	}
	for i, body := range got {
		if body["model"] != resolved.Model || body["store"] != false || body["stream"] != true {
			t.Fatalf("wire request %d=%v", i+1, body)
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input %d=%v", i+1, body["input"])
		}
		message := input[0].(map[string]any)
		if message["role"] != "user" {
			t.Fatalf("input role %v", message["role"])
		}
		content := message["content"].(string)
		if !strings.Contains(content, "initial task") {
			t.Fatalf("request %d omitted task context: %s", i+1, content)
		}
		if i == 1 && (!strings.Contains(content, "answer-1") || !strings.Contains(content, "runstead:observation")) {
			t.Fatalf("second request omitted local history: %s", content)
		}
	}
}
