package openaicompat_test

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
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

// synthetic secret material. Never a real credential.
const testSecret = "synthetic-test-secret"

func resolvedConfig(t *testing.T, mutate func(*provider.Config)) (provider.Resolved, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
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

// requestRecorder is a REAL HTTP server that counts physically arriving
// requests. Counting at a real server is what proves the one-physical-request
// contract; counting RoundTrip invocations would not, because an opaque
// transport can amplify inside one RoundTrip. The adapter no longer accepts
// injected transports for exactly that reason.
// resolvedForBase resolves a standard config against an arbitrary base URL.
func resolvedForBase(t *testing.T, baseURL string) provider.Resolved {
	t.Helper()
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = baseURL })
	return resolved
}

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

// newTestClient builds an adapter pointed at recorder.server. The handler may
// be nil for tests that only construct clients.
func newTestClient(t *testing.T, mutate func(*provider.Config), resolver openaicompat.SecretResolver, recorder *requestRecorder) (*openaicompat.Client, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
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
	if recorder != nil && mutate != nil {
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
	client, err := openaicompat.New(*resolved, resolver, openaicompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return client, config
}

const validCompletionBody = `{"choices":[{"message":{"role":"assistant","content":"runstead reply"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

// TestTwoProviderIDsShareOneAdapter proves that two different operator
// identities of the SAME family are served by the same adapter code path.
func TestTwoProviderIDsShareOneAdapter(t *testing.T) {
	for _, id := range []string{"gateway-a", "local-vllm"} {
		resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.ProviderID = id })
		client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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
	// A real server mounted at /v1 proves that configured prefixes are
	// preserved and the family path is appended exactly once.
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
	}))
	defer server.Close()

	client, err := openaicompat.New(resolvedForBase(t, server.URL+"/v1"), nil, openaicompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions (prefix preserved)", seenPath)
	}
}

// TestCompleteSendsExactConfiguredModelAndPrompt covers exact model + prompt
// contract and one physical request on the happy path.
func TestCompleteSendsExactConfiguredModelAndPrompt(t *testing.T) {
	var seenModel, seenPrompt, seenFirstRole string
	var seenStream *bool
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream *bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &payload)
		seenModel, seenPrompt = payload.Model, ""
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"synthetic-reply"}}]}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hello world"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenModel != "model-a" || seenPrompt != "hello world" || seenFirstRole != "user" {
		t.Fatalf("wire model=%q prompt=%q role=%q", seenModel, seenPrompt, seenFirstRole)
	}
	if seenStream == nil || *seenStream {
		t.Fatalf("stream flag = %v, want explicit false", seenStream)
	}
	if response.Text != "synthetic-reply" {
		t.Fatalf("text = %q", response.Text)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v, want completed", response.Metadata.DeliveryState)
	}
	if response.Metadata.Model != "model-a" || response.Metadata.Endpoint != "/chat/completions" {
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

func TestAuthNoneSendsNoAuthorizationHeader(t *testing.T) {
	var sawAuthorization bool
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["Authorization"]; present {
			sawAuthorization = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if sawAuthorization {
		t.Fatal("auth-none endpoint received an Authorization header")
	}
}

func TestRequiredAuthResolvesSecretOnlyIntoRequest(t *testing.T) {
	var sawAuthorization string
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
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
	if sawAuthorization != "Bearer "+testSecret {
		t.Fatalf("authorization header = %q", sawAuthorization)
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

func TestRequiredAuthWithoutResolutionProducesZeroRequests(t *testing.T) {
	cases := []struct {
		name     string
		resolver openaicompat.SecretResolver
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
		w.Header().Set("X-Request-Id", "req-123")
		w.Header().Set("Retry-After", "not-a-date")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Text != "the answer" {
		t.Fatalf("text = %q", response.Text)
	}
	if response.Metadata.StatusCode != 200 || response.Metadata.Model != "model-a" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v", response.Metadata.DeliveryState)
	}
}

func TestMalformedJSONFailsClosed(t *testing.T) {
	for _, body := range []string{`{"choices":[`, `not json at all`, ``, `   `} {
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

func TestEmptyOrMissingChoicesFailClosed(t *testing.T) {
	bodies := map[string]string{
		"missing choices": `{}`,
		"null choices":    `{"choices":null}`,
		"empty choices":   `{"choices":[]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			_, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "openai_compatible") {
				t.Fatalf("error = %v, want sanitized adapter error", err)
			}
		})
	}
}

func TestEmptyOrIncompatibleContentFailsClosed(t *testing.T) {
	bodies := map[string]string{
		"blank content":      `{"choices":[{"message":{"content":"   "}}]}`,
		"missing message":    `{"choices":[{"finish_reason":"stop"}]}`,
		"null content":       `{"choices":[{"message":{"content":null}}]}`,
		"content wrong type": `{"choices":[{"message":{"content":42}}]}`,
		"tool call only":     `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"x"}]}}]}`,
		"choices not a list": `{"choices":{"0":{}}}`,
		"text field instead": `{"choices":[{"text":"legacy completion"}]}`,
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
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
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
}

func TestServerErrorClassified(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		})
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
		_, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
		if err == nil {
			t.Fatalf("status %d accepted", status)
		}
		if !strings.Contains(err.Error(), "upstream_server_failure") {
			t.Fatalf("status %d classified as %v", status, err)
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
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = client.Complete(ctx, provider.Request{Prompt: "p"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancelled", err)
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
		_, _ = w.Write([]byte(validCompletionBody))
	}))
	defer target.Close()

	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = first.URL })
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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
		_, _ = w.Write([]byte(validCompletionBody))
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
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
	client, err := openaicompat.New(resolved, nil, openaicompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr == nil {
		t.Fatal("connection failure silently swallowed")
	}
	if recorder.count() != 0 {
		t.Fatalf("requests observed = %d, want 0 (closed server)", recorder.count())
	}
}

func TestIncompatibleCapabilityConfigPreventsDispatch(t *testing.T) {
	config := provider.Config{
		ProviderID:      "bad-profile",
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "m",
		AuthRequirement: provider.AuthNone,
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
		ProtocolFamily: provider.FamilyOpenAICompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			RouteSafety:    provider.ReceiptRouteSafety(),
		},
		ConfigIdentity: "identity",
	}
	if _, err := openaicompat.New(unsafe, nil, openaicompat.Options{}); err == nil {
		t.Fatal("New accepted receipt-route safety it does not implement")
	}

	// Wrong family is refused by New even if resolution were bypassed.
	wrongFamily := provider.Resolved{
		ProviderID:     "wrong-family",
		ProtocolFamily: provider.FamilyAnthropicCompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Profile:        provider.CapabilityProfile{ProfileVersion: "v1", RouteSafety: provider.SafeRouteSafety()},
		ConfigIdentity: "identity",
	}
	if _, err := openaicompat.New(wrongFamily, nil, openaicompat.Options{}); err == nil {
		t.Fatal("New accepted a non-openai_compatible family")
	}
}

func TestSecretNeverAppearsInSanitizedIdentityOrMetadata(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validCompletionBody))
	})
	resolved, config := resolvedConfig(t, func(c *provider.Config) {
		c.BaseURL = recorder.server.URL
		c.AuthRequirement = provider.AuthReferenceRequired
		c.Auth = provider.SecretRef("TEST_SECRET_REF")
	})
	client, err := openaicompat.New(resolved, func(ctx context.Context, reference provider.SecretRef) (string, error) {
		return testSecret, nil
	}, openaicompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, completeErr := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if completeErr != nil {
		t.Fatalf("complete: %v", completeErr)
	}
	rendered := fmt.Sprintf("%v %v %#v", config.Sanitized(), response.Metadata, response.Metadata)
	if strings.Contains(rendered, testSecret) {
		t.Fatal("secret leaked into sanitized identity or metadata")
	}
	if !strings.Contains(config.Sanitized(), "AuthRef:true") {
		t.Fatalf("sanitized identity = %q, want auth presence boolean only", config.Sanitized())
	}
}
