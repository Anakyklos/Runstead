package anthropiccompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
)

// synthetic secret material. Never a real credential.
const testSecret = "synthetic-test-secret"

// defaultOptions is the strictly necessary non-secret Messages-style option
// pair every resolved test configuration needs: the generation limit and the
// versioned header semantics.
var defaultOptions = map[string]string{
	"max_tokens":        "1024",
	"anthropic_version": "2023-06-01",
}

func resolvedConfig(t *testing.T, mutate func(*provider.Config)) (provider.Resolved, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyAnthropicCompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
		Options:         defaultOptions,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			Capabilities: provider.Capabilities{
				provider.CapabilityTextTurn:         true,
				provider.CapabilityRunsteadProtocol: true,
			},
			RouteSafety: provider.SafeRouteSafety(),
		},
		ConfigVersion: "test-1",
	}
	if mutate != nil {
		mutate(&config)
	}
	registry, err := provider.NewRegistry(config)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	resolved, err := registry.Resolve(config.ProviderID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	return *resolved, config
}

// resolvedForBase resolves a standard config against an arbitrary base URL.
func resolvedForBase(t *testing.T, baseURL string) provider.Resolved {
	t.Helper()
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = baseURL })
	return resolved
}

// requestRecorder is a REAL HTTP server that counts physically arriving
// requests. Counting at a real server is what proves the one-physical-request
// contract; counting RoundTrip invocations would not, because an opaque
// transport can amplify inside one RoundTrip. The adapter no longer accepts
// injected transports for exactly that reason.
type requestRecorder struct {
	server  *httptest.Server
	counter atomic.Int64
}

func newRequestRecorder(t *testing.T, handler http.HandlerFunc) *requestRecorder {
	t.Helper()
	recorder := &requestRecorder{}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.counter.Add(1)
		if handler != nil {
			handler(w, r)
		}
	})
	recorder.server = httptest.NewServer(wrapped)
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (r *requestRecorder) count() int64 { return r.counter.Load() }

// newTestClient builds an adapter pointed at recorder.server.
func newTestClient(t *testing.T, mutate func(*provider.Config), resolver anthropiccompat.SecretResolver, recorder *requestRecorder) (*anthropiccompat.Client, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyAnthropicCompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
		Options:         defaultOptions,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			Capabilities: provider.Capabilities{
				provider.CapabilityTextTurn:         true,
				provider.CapabilityRunsteadProtocol: true,
			},
			RouteSafety: provider.SafeRouteSafety(),
		},
		ConfigVersion: "test-1",
	}
	if mutate != nil {
		mutate(&config)
	}
	resolvedConfigCopy := config
	if recorder != nil {
		resolvedConfigCopy.BaseURL = recorder.server.URL
	}
	registry, err := provider.NewRegistry(resolvedConfigCopy)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	resolved, err := registry.Resolve(resolvedConfigCopy.ProviderID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	client, err := anthropiccompat.New(*resolved, resolver, anthropiccompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return client, config
}

const validMessagesBody = `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"runstead reply"}],"model":"model-a","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`

// TestTwoProviderIDsShareOneAdapter proves that two different operator
// identities of the SAME family are served by the same adapter code path.
func TestTwoProviderIDsShareOneAdapter(t *testing.T) {
	for _, id := range []string{"gateway-a", "local-anthropic-92"} {
		resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.ProviderID = id })
		client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
		if err != nil {
			t.Fatalf("provider %q: %v", id, err)
		}
		if client == nil {
			t.Fatalf("provider %q produced no client", id)
		}
		if client.RouteSafety() != provider.SafeRouteSafety() {
			t.Fatalf("provider %q route safety mismatch", id)
		}
	}
}

func TestBaseURLIsConfigurableAndPreservesPrefixes(t *testing.T) {
	// A real server mounted at /anthropic proves that configured prefixes are
	// preserved and the family path (v1/messages) is appended exactly once.
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	}))
	defer server.Close()

	client, err := anthropiccompat.New(resolvedForBase(t, server.URL+"/anthropic"), nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenPath != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want /anthropic/v1/messages (prefix preserved)", seenPath)
	}
}

// TestCompleteSendsExactConfiguredModelAndPrompt covers the exact model +
// prompt + option wire contract and one physical request on the happy path.
func TestCompleteSendsExactConfiguredModelAndPrompt(t *testing.T) {
	var seenModel, seenPrompt, seenFirstRole, seenVersion, seenAPIKey string
	var seenMaxTokens int
	var seenStream *bool
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		seenVersion = r.Header.Get("anthropic-version")
		seenAPIKey = r.Header.Get("x-api-key")
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream *bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &payload)
		seenModel, seenMaxTokens = payload.Model, payload.MaxTokens
		if len(payload.Messages) > 0 {
			seenFirstRole = payload.Messages[0].Role
			full := ""
			for _, message := range payload.Messages {
				full += message.Content
			}
			seenPrompt = full
		}
		seenStream = payload.Stream
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hello world"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenModel != "model-a" || seenPrompt != "hello world" || seenFirstRole != "user" {
		t.Fatalf("wire model=%q prompt=%q role=%q", seenModel, seenPrompt, seenFirstRole)
	}
	if seenMaxTokens != 1024 {
		t.Fatalf("wire max_tokens = %d, want the validated protocol option 1024", seenMaxTokens)
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("wire anthropic-version = %q, want the validated protocol option", seenVersion)
	}
	if seenAPIKey != "" {
		t.Fatalf("auth-none endpoint received api key header %q", seenAPIKey)
	}
	if seenStream == nil || *seenStream {
		t.Fatalf("stream flag = %v, want explicit false", seenStream)
	}
	if response.Text != "runstead reply" {
		t.Fatalf("text = %q", response.Text)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v, want completed", response.Metadata.DeliveryState)
	}
	if response.Metadata.Model != "model-a" || response.Metadata.Endpoint != "/v1/messages" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if recorder.count() != 1 {
		t.Fatalf("physical requests = %d, want exactly 1", recorder.count())
	}
}

func TestRequestModelMismatchFailsBeforeDispatch(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may reach the wire on model mismatch")
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	_, err := client.Complete(context.Background(), provider.Request{Model: "other-model", Prompt: "p"})
	if err == nil {
		t.Fatal("model mismatch was accepted")
	}
	if recorder.count() != 0 {
		t.Fatalf("requests = %d, want 0", recorder.count())
	}
}

func TestAuthNoneSendsNoAPIKeyHeader(t *testing.T) {
	var sawAPIKey bool
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["X-Api-Key"]; present {
			sawAPIKey = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sawAPIKey {
		t.Fatal("auth-none endpoint received an api key header")
	}
}

func TestRequiredAuthResolvesSecretOnlyIntoRequest(t *testing.T) {
	var sawAPIKey string
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		sawAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	resolverCalls := 0
	resolver := func(ctx context.Context, reference provider.SecretRef) (string, error) {
		resolverCalls++
		if reference.String() != "TEST_SECRET_REF" {
			t.Errorf("resolver got ref %q", reference.String())
		}
		return testSecret, nil
	}
	client, _ := newTestClient(t, func(c *provider.Config) {
		c.BaseURL = recorder.server.URL
		c.AuthRequirement = provider.AuthReferenceRequired
		c.Auth = provider.SecretRef("TEST_SECRET_REF").Normalize()
	}, resolver, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sawAPIKey != testSecret {
		t.Fatalf("api key header = %q, want the resolved secret material", sawAPIKey)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want exactly 1 before dispatch", resolverCalls)
	}
	errText := fmt.Sprintf("%v", err)
	if strings.Contains(errText, testSecret) {
		t.Fatal("secret leaked into error text")
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v", response.Metadata.DeliveryState)
	}
}

// TestSecretResolverConsultedOnlyAtDispatch proves the auth-none path never
// consults the secret resolver and the required-auth path consults it exactly
// once, immediately before the model-effect request is built.
func TestSecretResolverConsultedOnlyAtDispatch(t *testing.T) {
	t.Run("auth none never consults resolver", func(t *testing.T) {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validMessagesBody))
		})
		resolverCalls := 0
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, func(ctx context.Context, reference provider.SecretRef) (string, error) {
			resolverCalls++
			return testSecret, nil
		}, recorder)
		if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if resolverCalls != 0 {
			t.Fatalf("resolver calls = %d, want 0 for auth-none", resolverCalls)
		}
	})
	t.Run("required auth resolves once at dispatch", func(t *testing.T) {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validMessagesBody))
		})
		resolverCalls := 0
		client, _ := newTestClient(t, func(c *provider.Config) {
			c.BaseURL = recorder.server.URL
			c.AuthRequirement = provider.AuthReferenceRequired
			c.Auth = provider.SecretRef("TEST_SECRET_REF")
		}, func(ctx context.Context, reference provider.SecretRef) (string, error) {
			resolverCalls++
			return testSecret, nil
		}, recorder)
		if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if resolverCalls != 1 {
			t.Fatalf("resolver calls = %d, want exactly 1 (nothing before dispatch needs the secret)", resolverCalls)
		}
	})
}

func TestRequiredAuthWithoutResolutionProducesZeroRequests(t *testing.T) {
	cases := []struct {
		name     string
		resolver anthropiccompat.SecretResolver
	}{
		{"failing resolver", func(ctx context.Context, reference provider.SecretRef) (string, error) {
			return "", errors.New("secret store unreachable")
		}},
		{"empty secret", func(ctx context.Context, reference provider.SecretRef) (string, error) {
			return "", nil
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("zero requests expected when auth cannot be resolved")
			})
			client, _ := newTestClient(t, func(c *provider.Config) {
				c.BaseURL = recorder.server.URL
				c.AuthRequirement = provider.AuthReferenceRequired
				c.Auth = provider.SecretRef("TEST_SECRET_REF")
			}, testCase.resolver, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatal("unresolved auth was accepted")
			}
			if response.Metadata.DeliveryState != provider.DeliveryNotSent {
				t.Fatalf("delivery = %v, want not_sent", response.Metadata.DeliveryState)
			}
			if recorder.count() != 0 {
				t.Fatalf("requests = %d, want 0", recorder.count())
			}
			if !strings.Contains(strings.ToLower(err.Error()), "auth") {
				t.Fatalf("error = %v, want auth classification without secret content", err)
			}
		})
	}
}

func TestValidResponseNormalizesTextAndMetadata(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req-123")
		w.Header().Set("Retry-After", "not-a-date")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"first part "},{"type":"text","text":"second part"}],"model":"model-a","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2}}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Multi-block text concatenation must be deterministic and in wire order.
	if response.Text != "first part second part" {
		t.Fatalf("text = %q, want ordered concatenation", response.Text)
	}
	if response.Metadata.StatusCode != 200 || response.Metadata.Model != "model-a" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if response.Metadata.RequestID == "" {
		t.Fatal("request-id header must be normalized into metadata (hashed, not raw)")
	}
	if response.Metadata.RetryAfter != 0 {
		t.Fatalf("garbage retry-after must normalize to zero, got %v", response.Metadata.RetryAfter)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v", response.Metadata.DeliveryState)
	}
}

func TestMalformedJSONFailsClosed(t *testing.T) {
	for _, body := range []string{`{"content":[`, `not json at all`, ``, `   `} {
		t.Run(fmt.Sprintf("%q", body), func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("body %q was accepted as success", body)
			}
			if response.Text != "" {
				t.Fatalf("body %q leaked text %q", body, response.Text)
			}
		})
	}
}

func TestEmptyOrMissingContentFailClosed(t *testing.T) {
	bodies := map[string]string{
		"missing content":   `{"stop_reason":"end_turn"}`,
		"null content":      `{"content":null,"stop_reason":"end_turn"}`,
		"empty content":     `{"content":[],"stop_reason":"end_turn"}`,
		"blank text block":  `{"content":[{"type":"text","text":"   "}],"stop_reason":"end_turn"}`,
		"missing stop":      `{"content":[{"type":"text","text":"x"}]}`,
		"unknown stop":      `{"content":[{"type":"text","text":"x"}],"stop_reason":"mystery_stop"}`,
		"blank stop":        `{"content":[{"type":"text","text":"x"}],"stop_reason":"  "}`,
		"wrong block shape": `{"content":["not-an-object"],"stop_reason":"end_turn"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if response.Text != "" {
				t.Fatalf("%s leaked text %q", name, response.Text)
			}
			if !strings.Contains(err.Error(), "anthropic_compatible") {
				t.Fatalf("error = %v, want sanitized adapter error", err)
			}
		})
	}
}

func TestUnsupportedContentBlockFailsClosed(t *testing.T) {
	bodies := map[string]string{
		"thinking block":     `{"content":[{"type":"thinking","thinking":"reasoning"}],"stop_reason":"end_turn"}`,
		"redacted block":     `{"content":[{"type":"redacted_thinking","data":"x"}],"stop_reason":"end_turn"}`,
		"tool use with text": `{"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}],"stop_reason":"end_turn"}`,
		"unknown block type": `{"content":[{"type":"server_tool_use","id":"x"}],"stop_reason":"end_turn"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			var adapterErr *anthropiccompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != anthropiccompat.ErrorUnsupportedResponseFormat {
				t.Fatalf("%s classified as %v, want unsupported_response_format", name, err)
			}
			if response.Text != "" {
				t.Fatalf("%s leaked text %q; unsupported blocks must never become text", name, response.Text)
			}
		})
	}
}

func TestToolUseOnlyFailsClosed(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"tool_use","id":"toolu_01","name":"read_file","input":{"path":"README.md"}}],"stop_reason":"tool_use"}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("tool-use-only response was accepted as a text turn")
	}
	var adapterErr *anthropiccompat.Error
	if !errors.As(err, &adapterErr) || adapterErr.Kind != anthropiccompat.ErrorUnsupportedResponseFormat {
		t.Fatalf("classified as %v, want unsupported_response_format", err)
	}
	if response.Text != "" {
		t.Fatalf("tool-use-only response leaked text %q", response.Text)
	}
	// The tool call and its input must never surface anywhere.
	rendered := fmt.Sprintf("%v", err) + fmt.Sprintf("%#v", response)
	if strings.Contains(rendered, "read_file") || strings.Contains(rendered, "README.md") {
		t.Fatal("tool-use wire details leaked into errors/metadata")
	}
}

func TestTruncatedStopReasonFailsClosed(t *testing.T) {
	cases := map[string]string{
		"max_tokens":                    "max_tokens",
		"stop_sequence":                 "stop_sequence",
		"pause_turn":                    "pause_turn",
		"model_context_window_exceeded": "model_context_window_exceeded",
	}
	for name, stopReason := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"message","content":[{"type":"text","text":"partial answer"}],"stop_reason":%q}`, stopReason)))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("stop_reason %q was treated as a complete turn", stopReason)
			}
			var adapterErr *anthropiccompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != anthropiccompat.ErrorIncompleteCompletion {
				t.Fatalf("stop_reason %q classified as %v, want incomplete_completion", stopReason, err)
			}
			if response.Text != "" {
				t.Fatalf("truncated completion leaked partial text %q into task truth", response.Text)
			}
			if response.Metadata.DeliveryState != provider.DeliveryCompleted {
				t.Fatalf("delivery = %v, want completed: the body WAS fully received, the completion is what is incomplete", response.Metadata.DeliveryState)
			}
		})
	}
}

func TestRefusalClassifiedWhenProvable(t *testing.T) {
	cases := map[string]string{
		"stop_reason refusal":  `{"type":"message","content":[],"stop_reason":"refusal"}`,
		"stop_details refusal": `{"type":"message","content":[],"stop_reason":"end_turn","stop_details":{"type":"refusal","explanation":"declined"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s was accepted as success", name)
			}
			var adapterErr *anthropiccompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != anthropiccompat.ErrorRefusal {
				t.Fatalf("%s classified as %v, want refusal", name, err)
			}
			if response.Text != "" {
				t.Fatalf("refusal leaked text %q", response.Text)
			}
			rendered := fmt.Sprintf("%v", err) + fmt.Sprintf("%#v", response)
			if strings.Contains(rendered, "declined") {
				t.Fatal("refusal explanation (free text) leaked into errors/metadata")
			}
		})
	}
}

func TestUnauthorizedAndForbiddenClassifiedWithoutLeaks(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "authentication_denied"},
		{http.StatusForbidden, "permission_denied"},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprint(testCase.status), func(t *testing.T) {
			leak := "upstream says: bad key sk-live-DO-NOT-LEAK and " + testSecret
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(leak))
			})
			client, _ := newTestClient(t, func(c *provider.Config) {
				c.BaseURL = recorder.server.URL
				c.AuthRequirement = provider.AuthReferenceRequired
				c.Auth = provider.SecretRef("TEST_SECRET_REF")
			}, func(ctx context.Context, reference provider.SecretRef) (string, error) { return testSecret, nil }, recorder)
			_, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatal("upstream denial accepted as success")
			}
			rendered := err.Error() + fmt.Sprintf("%#v", err)
			if !strings.Contains(rendered, testCase.want) {
				t.Fatalf("error = %q, want kind %s", rendered, testCase.want)
			}
			if strings.Contains(rendered, testSecret) || strings.Contains(rendered, "sk-live") || strings.Contains(rendered, leak) {
				t.Fatalf("error leaked upstream body or secret: %q", rendered)
			}
		})
	}
}

func TestRateLimitWithObservableRetryAfter(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("429 accepted as success")
	}
	if response.Metadata.RetryAfter != 7*time.Second {
		t.Fatalf("retry-after = %v, want 7s normalized from observable header", response.Metadata.RetryAfter)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v, want completed for fully-read error body", response.Metadata.DeliveryState)
	}
	var adapterErr *anthropiccompat.Error
	if !errors.As(err, &adapterErr) || adapterErr.Kind != anthropiccompat.ErrorRateCapacity {
		t.Fatalf("classified as %v, want rate_or_capacity", err)
	}
}

// TestAnthropicStatusClassificationIsDeterministic pins the review blocker:
// the Messages family carries statuses whose recovery semantics differ from a
// generic 5xx. They are classified deterministically by status code, never by
// parsing free message text, with DeliveryState preserved exactly as observed
// and zero retries (retry remains outside the adapter, under governor
// authority):
//   - 413 request_too_large -> request_too_large;
//   - 504 timeout_error -> timeout;
//   - 529 overloaded_error -> rate_or_capacity.
func TestAnthropicStatusClassificationIsDeterministic(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantKind   anthropiccompat.ErrorKind
		wantStatus int
	}{
		{"413 request too large", http.StatusRequestEntityTooLarge, anthropiccompat.ErrorRequestTooLarge, 413},
		{"504 gateway timeout", http.StatusGatewayTimeout, anthropiccompat.ErrorTimeout, 504},
		{"529 overloaded", 529, anthropiccompat.ErrorRateCapacity, 529},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// The body deliberately tries to lie with free text: classification
			// must come from the status, never from message parsing.
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_a_hint","message":"synthetic body must be ignored"}}`))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("status %d accepted as success", testCase.status)
			}
			var adapterErr *anthropiccompat.Error
			if !errors.As(err, &adapterErr) {
				t.Fatalf("err = %T, want sanitized adapter error", err)
			}
			if adapterErr.Kind != testCase.wantKind {
				t.Fatalf("status %d classified as %q, want %q", testCase.status, adapterErr.Kind, testCase.wantKind)
			}
			if adapterErr.StatusCode != testCase.wantStatus {
				t.Fatalf("status %d surfaced as %d, want the observed status", testCase.status, adapterErr.StatusCode)
			}
			// The body was fully received: delivery evidence stays completed,
			// never degraded by the classification.
			if response.Metadata.DeliveryState != provider.DeliveryCompleted {
				t.Fatalf("status %d delivery = %v, want completed (body fully read)", testCase.status, response.Metadata.DeliveryState)
			}
			if adapterErr.DeliveryState != provider.DeliveryCompleted {
				t.Fatalf("status %d error delivery = %v, want completed", testCase.status, adapterErr.DeliveryState)
			}
			if adapterErr.UpstreamReached != true {
				t.Fatalf("status %d UpstreamReached = false, want true", testCase.status)
			}
			if adapterErr.RetryAfter != 0 {
				t.Fatalf("status %d RetryAfter = %v, want zero (no retry hint fabricated)", testCase.status, adapterErr.RetryAfter)
			}
			// Exactly one physical request: classification is not a retry.
			if recorder.count() != 1 {
				t.Fatalf("status %d requests = %d, want exactly 1 with zero automatic retries", testCase.status, recorder.count())
			}
			if strings.Contains(fmt.Sprintf("%v", err), "synthetic body") {
				t.Fatalf("status %d leaked the response body", testCase.status)
			}
		})
	}
}

func TestServerErrorClassified(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"boom"}}`))
		})
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
		_, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
		if err == nil {
			t.Fatalf("status %d accepted", status)
		}
		if !strings.Contains(err.Error(), "upstream_server_failure") {
			t.Fatalf("status %d classified as %v", status, err)
		}
		if recorder.count() != 1 {
			t.Fatalf("status %d requests = %d, want exactly 1 with zero retries", status, recorder.count())
		}
	}
}

func TestCancelBeforeTransportMeansZeroRequests(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cancelled context must not dispatch")
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := client.Complete(ctx, provider.Request{Prompt: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("delivery = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if recorder.count() != 0 {
		t.Fatalf("requests = %d, want 0", recorder.count())
	}
}

// blockingServer never answers; used to prove conservative states after
// dispatch with cancellation/timeout.
func blockingServer(t *testing.T) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		server.Close()
	})
	return server
}

func TestTimeoutAfterDispatchStaysConservativeWithZeroRetry(t *testing.T) {
	server := blockingServer(t)
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = server.URL })
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	response, err := client.Complete(ctx, provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("timeout not reported")
	}
	state := response.Metadata.DeliveryState
	if state == provider.DeliveryNotSent {
		t.Fatalf("post-dispatch timeout claimed not_sent; delivery evidence must stay conservative")
	}
	if state != provider.DeliverySentConfirmed && state != provider.DeliverySentUnconfirmed && state != provider.DeliveryResponseStarted {
		t.Fatalf("delivery = %v, want a conservative sent/response state", state)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline classification", err)
	}
}

func TestCancelledMidFlightStaysConservative(t *testing.T) {
	server := blockingServer(t)
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = server.URL })
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	response, err := client.Complete(ctx, provider.Request{Prompt: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancelled", err)
	}
	if response.Metadata.DeliveryState == provider.DeliveryNotSent {
		t.Fatal("post-dispatch cancel claimed not_sent")
	}
}

func TestRedirectIsRefusedNotFollowed(t *testing.T) {
	redirectTargetHit := false
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHit = true
		_, _ = w.Write([]byte(validMessagesBody))
	}))
	defer target.Close()

	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = first.URL })
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("redirect accepted as success")
	}
	if redirectTargetHit {
		t.Fatal("adapter followed the redirect: second physical request without admission")
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery after refused redirect = %v, want completed (first response fully read)", response.Metadata.DeliveryState)
	}
	if !strings.Contains(err.Error(), "unsafe_redirect") {
		t.Fatalf("err = %v, want unsafe_redirect", err)
	}
}

func TestExactlyOnePhysicalRequestPerNormalComplete(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
	if got := recorder.count(); got != 5 {
		t.Fatalf("physical requests = %d, want exactly 5 (one per Complete)", got)
	}
}

func TestNoHiddenRetryOnTransportError(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {})
	recorder.server.Close() // refuse connections: pure transport failure
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr == nil {
		t.Fatal("connection failure silently swallowed")
	}
	if response.Metadata.DeliveryState == provider.DeliveryNotSent {
		t.Fatal("ambiguous transport failure claimed not_sent")
	}
	if recorder.count() != 0 {
		t.Fatalf("requests observed = %d, want 0 (closed server)", recorder.count())
	}
}

func TestIncompatibleCapabilityConfigPreventsDispatch(t *testing.T) {
	config := provider.Config{
		ProviderID:      "bad-profile",
		ProtocolFamily:  provider.FamilyAnthropicCompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "m",
		AuthRequirement: provider.AuthNone,
		Options:         defaultOptions,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			Capabilities: provider.Capabilities{
				provider.CapabilityTextTurn: true,
				// runstead_protocol missing: resolution must fail.
			},
			RouteSafety: provider.SafeRouteSafety(),
		},
		ConfigVersion: "test-1",
	}
	badRegistry, regErr := provider.NewRegistry(config)
	if regErr != nil {
		t.Fatal(regErr)
	}
	if _, err := badRegistry.Resolve(config.ProviderID, provider.RequiredCapabilities(), provider.SafeRouteSafety()); err == nil {
		t.Fatal("resolution accepted an endpoint without runstead_protocol capability")
	}

	// A resolved config whose declared route safety diverges from the safe
	// route this adapter implements must be refused by New itself.
	unsafe := provider.Resolved{
		ProviderID:     "receipt-lane",
		ProtocolFamily: provider.FamilyAnthropicCompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Options:        defaultOptions,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			RouteSafety:    provider.ReceiptRouteSafety(),
		},
		ConfigIdentity: "identity",
	}
	if _, err := anthropiccompat.New(unsafe, nil, anthropiccompat.Options{}); err == nil {
		t.Fatal("New accepted receipt-route safety it does not implement")
	}

	// Wrong family is refused by New even if resolution were bypassed.
	wrongFamily := provider.Resolved{
		ProviderID:     "wrong-family",
		ProtocolFamily: provider.FamilyOpenAICompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Options:        defaultOptions,
		Profile:        provider.CapabilityProfile{ProfileVersion: "v1", RouteSafety: provider.SafeRouteSafety()},
		ConfigIdentity: "identity",
	}
	if _, err := anthropiccompat.New(wrongFamily, nil, anthropiccompat.Options{}); err == nil {
		t.Fatal("New accepted a non-anthropic_compatible family")
	}
}

// TestInvalidProtocolOptionsRefusedAtConstruction covers the closed option
// vocabulary of the Messages baseline: missing, unknown or invalid options
// must refuse the adapter with zero requests and no silent defaults.
func TestInvalidProtocolOptionsRefusedAtConstruction(t *testing.T) {
	optionCases := map[string]map[string]string{
		"no options at all":         nil,
		"missing max_tokens":        {"anthropic_version": "2023-06-01"},
		"missing anthropic_version": {"max_tokens": "1024"},
		"max_tokens not a number":   {"max_tokens": "many", "anthropic_version": "2023-06-01"},
		"max_tokens zero":           {"max_tokens": "0", "anthropic_version": "2023-06-01"},
		"max_tokens negative":       {"max_tokens": "-5", "anthropic_version": "2023-06-01"},
		"max_tokens absurd":         {"max_tokens": "999999999999999999", "anthropic_version": "2023-06-01"},
		"version not a date":        {"max_tokens": "1024", "anthropic_version": "latest"},
		"version credential shaped": {"max_tokens": "1024", "anthropic_version": "Bearer sk-123"},
		"unknown option key":        {"max_tokens": "1024", "anthropic_version": "2023-06-01", "temperature": "0.7"},
		"unknown option key only":   {"temperature": "0.7"},
		"unknown beta option":       {"max_tokens": "1024", "anthropic_version": "2023-06-01", "beta_header": "true"},
	}
	for name, options := range optionCases {
		t.Run(name, func(t *testing.T) {
			resolved := provider.Resolved{
				ProviderID:      "option-gate",
				ProtocolFamily:  provider.FamilyAnthropicCompatible,
				BaseURL:         "http://127.0.0.1:1",
				Model:           "m",
				AuthRequirement: provider.AuthNone,
				Options:         options,
				Profile: provider.CapabilityProfile{
					ProfileVersion: "v1",
					RouteSafety:    provider.SafeRouteSafety(),
				},
				ConfigIdentity: "identity",
			}
			if _, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{}); err == nil {
				t.Fatalf("New accepted invalid options %v", options)
			}
			// Conservative state: refused constructors can never dispatch.
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("zero requests expected for invalid options")
			})
			resolved.BaseURL = recorder.server.URL
			if _, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{}); err == nil {
				t.Fatalf("New accepted invalid options against live server %v", options)
			}
			if recorder.count() != 0 {
				t.Fatalf("requests = %d, want 0", recorder.count())
			}
		})
	}
}

func TestSecretNeverAppearsInSanitizedIdentityOrMetadata(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	resolved, config := resolvedConfig(t, func(c *provider.Config) {
		c.BaseURL = recorder.server.URL
		c.AuthRequirement = provider.AuthReferenceRequired
		c.Auth = provider.SecretRef("TEST_SECRET_REF")
	})
	client, err := anthropiccompat.New(resolved, func(ctx context.Context, reference provider.SecretRef) (string, error) {
		return testSecret, nil
	}, anthropiccompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr != nil {
		t.Fatalf("complete: %v", completeErr)
	}
	rendered := fmt.Sprintf("%v %v %#v %#v", config.Sanitized(), response.Metadata, response.Metadata, config)
	if strings.Contains(rendered, testSecret) {
		t.Fatal("secret leaked into sanitized identity or metadata")
	}
	if !strings.Contains(config.Sanitized(), "AuthRef:true") {
		t.Fatalf("sanitized identity = %q, want auth presence boolean only", config.Sanitized())
	}
	// Option VALUES must never render into the identity either.
	for key, value := range defaultOptions {
		if strings.Contains(config.Sanitized(), key+"="+value) || strings.Contains(config.Sanitized(), value) {
			t.Fatalf("option value %q leaked into sanitized identity", value)
		}
	}
}
