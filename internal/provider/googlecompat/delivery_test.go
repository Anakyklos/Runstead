package googlecompat_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
)

// TestAgentLoopNeverSeesGoogleWireTypes is the structural proof for the
// amplification invariant "the agent loop cannot know Google/Gemini": the
// packages above the provider boundary (agent, governor, tools, verifier)
// must not reference Google wire concepts, vendor endpoints or this adapter.
// The only permitted match is an environment-variable redaction fixture in
// tool tests, which proves secrets are REDACTED, not consumed.
func TestAgentLoopNeverSeesGoogleWireTypes(t *testing.T) {
	inspector := &googleSourceInspector{t: t}
	for _, dir := range []string{
		"../../../internal/agent",
		"../../../internal/governor",
		"../../../internal/tools",
		"../../../internal/verifier",
	} {
		inspector.assertNoGoogleWireConcepts(dir)
	}
}

type googleSourceInspector struct {
	t *testing.T
}

func (s *googleSourceInspector) assertNoGoogleWireConcepts(dir string) {
	s.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			s.assertNoGoogleWireConcepts(path)
			continue
		}
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			s.t.Fatal(err)
		}
		lower := strings.ToLower(string(content))
		for _, forbidden := range []string{"googlecompat", "gemini", "generatecontent", "promptfeedback", "blockreason", "candidates", "finishreason", "functioncall", "x-goog-api-key", "max_tokens"} {
			if strings.Contains(lower, forbidden) {
				s.t.Errorf("%s references Google wire concept %q across the provider boundary", path, forbidden)
			}
		}
	}
}

// amplificationProof counts physical requests at a real HTTP server and
// fails any scenario where the adapter emits more than one request per Complete
// or retries after errors/redirects.
type amplificationProof struct {
	counter atomic.Int64
	server  *httptest.Server
}

func newAmplificationProof(t *testing.T, handler http.HandlerFunc) *amplificationProof {
	t.Helper()
	proof := &amplificationProof{}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proof.counter.Add(1)
		handler(w, r)
	})
	proof.server = httptest.NewServer(wrapped)
	t.Cleanup(proof.server.Close)
	return proof
}

func (p *amplificationProof) client(t *testing.T, baseURL string) *googlecompat.Client {
	t.Helper()
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = baseURL })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (p *amplificationProof) count() int64 { return p.counter.Load() }

func TestAmplificationNormalCompleteIsExactlyOnePhysicalRequest(t *testing.T) {
	proof := newAmplificationProof(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	client := proof.client(t, proof.server.URL)
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := proof.count(); got != 1 {
		t.Fatalf("normal Complete produced %d physical requests, want exactly 1", got)
	}
}

func TestAmplificationErrorScenariosNeverFireSecondRequest(t *testing.T) {
	scenarios := map[string]http.HandlerFunc{
		"rate limited": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		},
		"400 invalid request": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid"}}`))
		},
		"401 unauthorized": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"denied"}}`))
		},
		"403 forbidden": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"forbidden"}}`))
		},
		"413 request too large": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"too large"}}`))
		},
		"504 gateway timeout": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"error":{"message":"timeout"}}`))
		},
		"529 overloaded": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(529)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
		},
		"server failure": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		},
		"redirect 301": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/moved", http.StatusMovedPermanently)
		},
		"redirect 302": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/moved", http.StatusFound)
		},
		"redirect 307": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
		},
		"redirect 308": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/moved", http.StatusPermanentRedirect)
		},
		"malformed success body": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates": [`))
		},
		"truncated generation": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"partial"}],"role":"model"},"finishReason":"MAX_TOKENS"}],"promptFeedback":{}}`))
		},
		"refused candidate": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"no"}],"role":"model"},"finishReason":"PROHIBITED_CONTENT"}],"promptFeedback":{}}`))
		},
		"blocked prompt": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[]}`))
		},
		"function call shape": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file"}}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`))
		},
		"thought part": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"thought":true,"text":"reasoning"},{"text":"final answer"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`))
		},
	}
	for name, handler := range scenarios {
		t.Run(name, func(t *testing.T) {
			proof := newAmplificationProof(t, handler)
			client := proof.client(t, proof.server.URL)
			_, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s completed successfully; expected fail-closed error", name)
			}
			if got := proof.count(); got != 1 {
				t.Fatalf("%s produced %d physical requests, want exactly 1 with zero automatic retry", name, got)
			}
		})
	}
}

func TestAmplificationTimeoutThenExplicitSecondCallIsTwoSeparateCompletions(t *testing.T) {
	var phase atomic.Int64
	proof := newAmplificationProof(t, func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			time.Sleep(300 * time.Millisecond)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	client := proof.client(t, proof.server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	first, firstErr := client.Complete(ctx, provider.Request{Prompt: "p"})
	if firstErr == nil {
		t.Fatal("first completion unexpectedly succeeded")
	}
	state := first.Metadata.DeliveryState
	if state == provider.DeliveryNotSent {
		t.Fatal("post-dispatch timeout claimed not_sent")
	}
	phase.Store(1)
	second, secondErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if secondErr != nil {
		t.Fatalf("second explicit completion failed: %v", secondErr)
	}
	if second.Text == "" {
		t.Fatal("second completion lost text")
	}
	if got := proof.count(); got != 2 {
		t.Fatalf("physical requests = %d, want 2 (one per EXPLICIT completion)", got)
	}
}

// TestAmplifyingTransportStackCannotBeInjected is the regression demanded by
// the #87 review and carried through #88 into #89: a retry/fan-out-capable
// dispatch stack must not be able to pose as SafeRouteSafety. The constructor
// accepts no http.Client and no RoundTripper at all, so an amplifying stack
// can never be installed where the adapter could not observe or account for
// it; this test pins that surface by compile-time shape (Options has no
// transport fields) plus a runtime probe showing the adapter's client keeps
// its own pinned knobs.
func TestAmplifyingTransportStackCannotBeInjected(t *testing.T) {
	// Compile-time proof: Options must expose no transport injection points.
	// If someone re-adds such a field, this struct literal stops compiling.
	options := googlecompat.Options{}
	optionsType := reflect.TypeOf(options)
	if optionsType.NumField() != 1 || optionsType.Field(0).Name != "Now" {
		t.Fatalf("googlecompat.Options must expose only Now; found %d fields with transport injection surfaces", optionsType.NumField())
	}

	// Runtime proof: the adapter owns its stack regardless of what a caller
	// would like to inject; Complete still reaches a real server through the
	// adapter-pinned transport with redirect refusal intact.
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr == nil || !strings.Contains(completeErr.Error(), "unsafe_redirect") {
		t.Fatalf("err = %v, want refused redirect through adapter-owned stack", completeErr)
	}
	if recorder.count() != 1 {
		t.Fatalf("requests = %d, want exactly 1: the refused redirect must not amplify", recorder.count())
	}
}

// TestSafeRouteUsesHTTP11AndCompletesOnce is the external, end-to-end half of
// the hidden-retry regression (the structural half lives in
// safe_route_internal_test.go): over the adapter-pinned HTTP/1.1 stack a
// normal Complete works with exactly one physical request and the response
// provably arrived over HTTP/1.1, so the h2 retry loop never ran.
func TestSafeRouteUsesHTTP11AndCompletesOnce(t *testing.T) {
	var seenProto string
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		seenProto = r.Proto
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr != nil {
		t.Fatalf("complete over pinned http/1.1 stack: %v", completeErr)
	}
	if seenProto != "HTTP/1.1" {
		t.Fatalf("upstream request protocol = %s, want HTTP/1.1 (h2 must never be negotiated)", seenProto)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted || recorder.count() != 1 {
		t.Fatalf("delivery=%v requests=%d, want completed with exactly one physical request", response.Metadata.DeliveryState, recorder.count())
	}
}

func TestAmbiguousTransportErrorIsConservativeNotNotSent(t *testing.T) {
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = "http://127.0.0.1:0" })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr == nil {
		t.Fatal("connection refusal silently swallowed")
	}
	switch response.Metadata.DeliveryState {
	case provider.DeliveryNotSent:
		t.Fatal("ambiguous transport failure must never be classified as provable not_sent")
	case provider.DeliverySentConfirmed, provider.DeliverySentUnconfirmed:
		// conservative and correct: dispatch became possible
	default:
		t.Fatalf("delivery = %v, want a conservative sent_* state", response.Metadata.DeliveryState)
	}
	var adapterErr *googlecompat.Error
	if !errors.As(completeErr, &adapterErr) {
		t.Fatalf("err = %T, want sanitized adapter error", completeErr)
	}
	if adapterErr.Kind != googlecompat.ErrorTransport {
		t.Fatalf("kind = %v, want transport", adapterErr.Kind)
	}
}

// TestDeliveryStateProgressionPinsConservativeEvidence maps every visible
// transport outcome onto the delivery-state contract: refusal before dispatch
// is not_sent, success after body processing is completed, and a transport
// failure after dispatch can never downgrade to not_sent.
func TestDeliveryStateProgressionPinsConservativeEvidence(t *testing.T) {
	t.Run("pre-dispatch refusal is not_sent", func(t *testing.T) {
		resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.Model = "configured-model" })
		client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Complete(context.Background(), provider.Request{Model: "other", Prompt: "p"})
		if err == nil {
			t.Fatal("model mismatch accepted")
		}
		if response.Metadata.DeliveryState != provider.DeliveryNotSent {
			t.Fatalf("delivery = %v, want not_sent for provable pre-dispatch refusal", response.Metadata.DeliveryState)
		}
	})
	t.Run("success is completed", func(t *testing.T) {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validGenerateBody))
		})
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
		response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
		if response.Metadata.DeliveryState != provider.DeliveryCompleted {
			t.Fatalf("delivery = %v, want completed for fully processed success", response.Metadata.DeliveryState)
		}
	})
	t.Run("upstream denial body fully read is completed", func(t *testing.T) {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
		})
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
		response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
		if err == nil {
			t.Fatal("403 accepted")
		}
		if response.Metadata.DeliveryState != provider.DeliveryCompleted {
			t.Fatalf("delivery = %v, want completed for fully-read error body", response.Metadata.DeliveryState)
		}
	})
}
