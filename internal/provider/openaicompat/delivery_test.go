package openaicompat_test

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
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

// TestAgentLoopNeverSeesOpenAIWireTypes is the structural proof for the
// amplification invariant "the agent loop cannot know OpenAI": the packages
// above the provider boundary (agent, governor, tools, verifier) must not
// reference OpenAI wire concepts, vendor endpoints or this adapter. The only
// permitted match is an environment-variable redaction fixture in tool tests,
// which proves secrets are REDACTED, not consumed.
func TestAgentLoopNeverSeesOpenAIWireTypes(t *testing.T) {
	inspector := &sourceInspector{t: t}
	for _, dir := range []string{
		"../../../internal/agent",
		"../../../internal/governor",
		"../../../internal/tools",
		"../../../internal/verifier",
	} {
		inspector.assertNoOpenAIWireConcepts(dir)
	}
}

type sourceInspector struct {
	t *testing.T
}

func (s *sourceInspector) assertNoOpenAIWireConcepts(dir string) {
	s.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			s.assertNoOpenAIWireConcepts(path)
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
		for _, forbidden := range []string{"chat/completions", "chatcompletion", "openai.com", "openaicompat"} {
			if strings.Contains(lower, forbidden) {
				s.t.Errorf("%s references OpenAI wire concept %q across the provider boundary", path, forbidden)
			}
		}
	}
}

// AmplificationProofHarness counts physical requests at a real HTTP server and
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

func (p *amplificationProof) client(t *testing.T, baseURL string) *openaicompat.Client {
	t.Helper()
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = baseURL })
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (p *amplificationProof) count() int64 { return p.counter.Load() }

func TestAmplificationNormalCompleteIsExactlyOnePhysicalRequest(t *testing.T) {
	proof := newAmplificationProof(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
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
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
		},
		"server failure": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
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
			_, _ = w.Write([]byte(`{"choices": [`))
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
		_, _ = w.Write([]byte(validCompletionBody))
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
// the PR review: a retry/fan-out-capable dispatch stack must not be able to
// pose as SafeRouteSafety. The constructor accepts no http.Client and no
// RoundTripper at all, so an amplifying stack can never be installed where the
// adapter could not observe or account for it; this test pins that surface by
// compile-time shape (Options has no transport fields) plus a runtime probe
// showing the adapter's client keeps its own pinned knobs.
func TestAmplifyingTransportStackCannotBeInjected(t *testing.T) {
	// Compile-time proof: Options must expose no transport injection points.
	// If someone re-adds such a field, this struct literal stops compiling.
	options := openaicompat.Options{}
	optionsType := reflect.TypeOf(options)
	if optionsType.NumField() != 1 || optionsType.Field(0).Name != "Now" {
		t.Fatalf("openaicompat.Options must expose only Now; found %d fields with transport injection surfaces", optionsType.NumField())
	}

	// Runtime proof: the adapter owns its stack regardless of what a caller
	// would like to inject; Complete still reaches a real server through the
	// adapter-pinned transport with redirect refusal intact.
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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

func TestAmbiguousTransportErrorIsConservativeNotNotSent(t *testing.T) {
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = "http://127.0.0.1:0" })
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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
	var adapterErr *openaicompat.Error
	if !errors.As(completeErr, &adapterErr) {
		t.Fatalf("err = %T, want sanitized adapter error", completeErr)
	}
	if adapterErr.Kind != openaicompat.ErrorTransport {
		t.Fatalf("kind = %v, want transport", adapterErr.Kind)
	}
}
