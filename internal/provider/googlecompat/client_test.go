package googlecompat_test

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
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
)

// synthetic secret material. Never a real credential.
const testSecret = "synthetic-test-secret"

func resolvedConfig(t *testing.T, mutate func(*provider.Config)) (provider.Resolved, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyGoogleCompatible,
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
func newTestClient(t *testing.T, mutate func(*provider.Config), resolver googlecompat.SecretResolver, recorder *requestRecorder) (*googlecompat.Client, provider.Config) {
	t.Helper()
	config := provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyGoogleCompatible,
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
	client, err := googlecompat.New(*resolved, resolver, googlecompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return client, config
}

const validGenerateBody = `{"candidates":[{"content":{"parts":[{"text":"runstead reply"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`

// TestTwoProviderIDsShareOneAdapter proves that two different operator
// identities of the SAME family are served by the same adapter code path.
func TestTwoProviderIDsShareOneAdapter(t *testing.T) {
	for _, id := range []string{"gateway-a", "local-gemini-92"} {
		resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.ProviderID = id })
		client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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
	// A real server mounted at /google proves that configured prefixes are
	// preserved and the family resource path is appended exactly once, with
	// the exact configured model in the canonical position.
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	}))
	defer server.Close()

	client, err := googlecompat.New(resolvedForBase(t, server.URL+"/google"), nil, googlecompat.Options{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenPath != "/google/models/model-a:generateContent" {
		t.Fatalf("path = %q, want /google/models/model-a:generateContent (prefix preserved)", seenPath)
	}
}

// TestModelWithSlashKeepsResourceStructure proves the exact model travels in
// the URL resource path without escaping its path structure, so
// publisher-style model names survive the composition.
func TestModelWithSlashKeepsResourceStructure(t *testing.T) {
	var seenPath string
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) {
		c.BaseURL = recorder.server.URL
		c.Model = "publishers/google/models/gemini-2.0-flash"
	})
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), provider.Request{Prompt: "p"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenPath != "/models/publishers/google/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("path = %q, want slash-preserving model resource path", seenPath)
	}
}

// TestCompleteSendsExactConfiguredModelAndPrompt covers the exact model
// resource path + prompt + one physical request on the happy path. The model
// travels in the URL, never in the body; the body carries exactly one explicit
// user content with one text part.
func TestCompleteSendsExactConfiguredModelAndPrompt(t *testing.T) {
	var seenPath, seenPrompt, seenFirstRole, seenAPIKey string
	var seenParts int
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAPIKey = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.Unmarshal(body, &payload)
		if len(payload.Contents) > 0 {
			seenFirstRole = payload.Contents[0].Role
			seenParts = len(payload.Contents[0].Parts)
			full := ""
			for _, part := range payload.Contents[0].Parts {
				full += part.Text
			}
			seenPrompt = full
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hello world"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if seenPath != "/models/model-a:generateContent" {
		t.Fatalf("wire path = %q, want /models/model-a:generateContent", seenPath)
	}
	if seenPrompt != "hello world" || seenFirstRole != "user" || seenParts != 1 {
		t.Fatalf("wire prompt=%q role=%q parts=%d", seenPrompt, seenFirstRole, seenParts)
	}
	if seenAPIKey != "" {
		t.Fatalf("auth-none endpoint received api key header %q", seenAPIKey)
	}
	if response.Text != "runstead reply" {
		t.Fatalf("text = %q", response.Text)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v, want completed", response.Metadata.DeliveryState)
	}
	if response.Metadata.Model != "model-a" || response.Metadata.Endpoint != "/models/model-a:generateContent" {
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
		if _, present := r.Header["X-Goog-Api-Key"]; present {
			sawAPIKey = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
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
		sawAPIKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
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
			_, _ = w.Write([]byte(validGenerateBody))
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
			_, _ = w.Write([]byte(validGenerateBody))
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
		resolver googlecompat.SecretResolver
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
		w.Header().Set("x-request-id", "req-123")
		w.Header().Set("Retry-After", "not-a-date")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"first part "},{"text":"second part"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":2}}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Multi-part text concatenation must be deterministic and in wire order.
	if response.Text != "first part second part" {
		t.Fatalf("text = %q, want ordered concatenation", response.Text)
	}
	if response.Metadata.StatusCode != 200 || response.Metadata.Model != "model-a" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if response.Metadata.RequestID == "" {
		t.Fatal("x-request-id header must be normalized into metadata (hashed, not raw)")
	}
	if response.Metadata.RetryAfter != 0 {
		t.Fatalf("garbage retry-after must normalize to zero, got %v", response.Metadata.RetryAfter)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v", response.Metadata.DeliveryState)
	}
}

func TestMalformedJSONFailsClosed(t *testing.T) {
	for _, body := range []string{`{"candidates": [`, `not json at all`, ``, `   `} {
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
		"missing candidates":     `{"promptFeedback":{}}`,
		"null candidates":        `{"candidates":null,"promptFeedback":{}}`,
		"empty candidates":       `{"candidates":[],"promptFeedback":{}}`,
		"no candidate content":   `{"candidates":[{"finishReason":"STOP"}],"promptFeedback":{}}`,
		"null candidate content": `{"candidates":[{"content":null,"finishReason":"STOP"}],"promptFeedback":{}}`,
		"null parts":             `{"candidates":[{"content":{"parts":null},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"empty parts":            `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"blank text part":        `{"candidates":[{"content":{"parts":[{"text":"   "}]},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"missing finish":         `{"candidates":[{"content":{"parts":[{"text":"x"}]}}],"promptFeedback":{}}`,
		"unknown finish":         `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"mystery_finish"}],"promptFeedback":{}}`,
		"blank finish":           `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"  "}],"promptFeedback":{}}`,
		"unspecified finish":     `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"FINISH_REASON_UNSPECIFIED"}],"promptFeedback":{}}`,
		"wrong part shape":       `{"candidates":[{"content":{"parts":["not-an-object"]},"finishReason":"STOP"}],"promptFeedback":{}}`,
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
			if !strings.Contains(err.Error(), "google_compatible") {
				t.Fatalf("error = %v, want sanitized adapter error", err)
			}
		})
	}
}

// TestPromptBlockedClassifiedAsRefusal proves promptFeedback.blockReason is a
// typed refusal/safety outcome, never an empty response, and that the free
// text around it never leaks.
func TestPromptBlockedClassifiedAsRefusal(t *testing.T) {
	cases := map[string]string{
		"blocked safety":     `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}]},"candidates":[]}`,
		"blocked blocklist":  `{"promptFeedback":{"blockReason":"BLOCKLIST"},"candidates":[]}`,
		"blocked prohibited": `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"candidates":[]}`,
		"blocked other":      `{"promptFeedback":{"blockReason":"OTHER"},"candidates":[]}`,
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
			var adapterErr *googlecompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorRefusal {
				t.Fatalf("%s classified as %v, want refusal", name, err)
			}
			if response.Text != "" {
				t.Fatalf("blocked prompt leaked text %q", response.Text)
			}
			rendered := fmt.Sprintf("%v", err) + fmt.Sprintf("%#v", response)
			if strings.Contains(rendered, "HARM_CATEGORY") || strings.Contains(rendered, "HIGH") {
				t.Fatal("free-text safety details leaked into errors/metadata")
			}
		})
	}
}

// TestSafetyFinishReasonsClassifiedExplicitly pins the candidate-level
// safety/refusal finish reasons as typed outcomes. Accompanying text is never
// surfaced, and classification never relies on free-text message parsing.
func TestSafetyFinishReasonsClassifiedExplicitly(t *testing.T) {
	for _, finishReason := range []string{"SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "LANGUAGE", "SPII"} {
		t.Run(finishReason, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":"do not surface this"}],"role":"model"},"finishReason":%q}],"promptFeedback":{}}`, finishReason)))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("finishReason %q was accepted as success", finishReason)
			}
			var adapterErr *googlecompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorRefusal {
				t.Fatalf("finishReason %q classified as %v, want refusal", finishReason, err)
			}
			if response.Text != "" {
				t.Fatalf("safety finishReason %q leaked text %q", finishReason, response.Text)
			}
			if recorder.count() != 1 {
				t.Fatalf("finishReason %q requests = %d, want exactly 1", finishReason, recorder.count())
			}
		})
	}
}

// TestMaxTokensIncomplete proves MAX_TOKENS is a truncated completion, never
// silent success and never partial task truth.
func TestMaxTokensIncomplete(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"partial answer"}],"role":"model"},"finishReason":"MAX_TOKENS"}],"promptFeedback":{}}`))
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("MAX_TOKENS was treated as a complete turn")
	}
	var adapterErr *googlecompat.Error
	if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorIncompleteCompletion {
		t.Fatalf("classified as %v, want incomplete_completion", err)
	}
	if response.Text != "" {
		t.Fatalf("truncated completion leaked partial text %q into task truth", response.Text)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery = %v, want completed: the body WAS fully received, the completion is what is incomplete", response.Metadata.DeliveryState)
	}
}

// TestFunctionCallShapesFailClosed proves tool-shape responses are never
// reinterpreted as text when native tools are not enabled: functionCall parts
// and function-call finish reasons are unsupported formats.
func TestFunctionCallShapesFailClosed(t *testing.T) {
	bodies := map[string]string{
		"functionCall part":              `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"README.md"}}}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"text then functionCall part":    `{"candidates":[{"content":{"parts":[{"text":"ok"},{"functionCall":{"name":"read_file"}}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"functionCall finish":            `{"candidates":[{"content":{"parts":[{"text":"tool time"}],"role":"model"},"finishReason":"FUNCTION_CALL"}],"promptFeedback":{}}`,
		"malformed function call finish": `{"candidates":[{"content":{"parts":[{"text":"tool time"}],"role":"model"},"finishReason":"MALFORMED_FUNCTION_CALL"}],"promptFeedback":{}}`,
		"inlineData part":                `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aaaa"}}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
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
			var adapterErr *googlecompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorUnsupportedResponseFormat {
				t.Fatalf("%s classified as %v, want unsupported_response_format", name, err)
			}
			if response.Text != "" {
				t.Fatalf("%s leaked text %q; tool shapes must never become text", name, response.Text)
			}
			rendered := fmt.Sprintf("%v", err) + fmt.Sprintf("%#v", response)
			if strings.Contains(rendered, "read_file") || strings.Contains(rendered, "README.md") || strings.Contains(rendered, "image/png") {
				t.Fatal("tool/multimodal wire details leaked into errors/metadata")
			}
		})
	}
}

// TestThoughtPartsFailClosed is the #97 review regression for the Part.thought
// blocker: GenerateContent parts marked thought:true carry reasoning/summary
// content, never the model's normal completion text. The baseline does not
// support thought summaries, so any thought part fails closed as an
// unsupported response format and never enters provider.Response.Text, while
// explicitly non-thought parts (thought:false, with or without
// thoughtSignature metadata) remain normal text. thoughtSignature and other
// unused metadata never leave this package; this is not thinking support.
func TestThoughtPartsFailClosed(t *testing.T) {
	unsupportedBodies := map[string]string{
		"thought then final text": `{"candidates":[{"content":{"parts":[{"thought":true,"text":"internal reasoning summary"},{"text":"final answer"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"text then thought part":  `{"candidates":[{"content":{"parts":[{"text":"lead-in"},{"thought":true,"text":"retrospective reasoning"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"thought only":            `{"candidates":[{"content":{"parts":[{"thought":true,"text":"reasoning"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
		"thought with signature":  `{"candidates":[{"content":{"parts":[{"thought":true,"text":"reasoning","thoughtSignature":"tok_sig_123"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`,
	}
	for name, body := range unsupportedBodies {
		t.Run(name, func(t *testing.T) {
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("%s was accepted as a normal completion", name)
			}
			var adapterErr *googlecompat.Error
			if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorUnsupportedResponseFormat {
				t.Fatalf("%s classified as %v, want unsupported_response_format", name, err)
			}
			if response.Text != "" {
				t.Fatalf("%s leaked text %q; thought content must never become task truth", name, response.Text)
			}
			rendered := fmt.Sprintf("%v", err) + fmt.Sprintf("%#v", response)
			if strings.Contains(rendered, "reasoning") || strings.Contains(rendered, "lead-in") || strings.Contains(rendered, "final answer") || strings.Contains(rendered, "tok_sig_123") {
				t.Fatalf("%s leaked thought/response content or signature into errors/metadata", name)
			}
		})
	}

	t.Run("explicit non-thought parts remain normal text", func(t *testing.T) {
		recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"thought":false,"text":"first "},{"text":"second","thoughtSignature":"sig_ignored"}],"role":"model"},"finishReason":"STOP"}],"promptFeedback":{}}`))
		})
		client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
		response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
		if err != nil {
			t.Fatalf("explicit non-thought parts must complete: %v", err)
		}
		if response.Text != "first second" {
			t.Fatalf("text = %q, want ordered concatenation of non-thought parts", response.Text)
		}
		rendered := fmt.Sprintf("%#v", response)
		if strings.Contains(rendered, "sig_ignored") {
			t.Fatal("thoughtSignature metadata leaked into the response surface")
		}
	})
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
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`))
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
	var adapterErr *googlecompat.Error
	if !errors.As(err, &adapterErr) || adapterErr.Kind != googlecompat.ErrorRateCapacity {
		t.Fatalf("classified as %v, want rate_or_capacity", err)
	}
}

// TestStatusClassificationIsDeterministic pins the review blocker carried
// from #88/#96 into #89: recovery-relevant HTTP statuses are classified
// deterministically by status code, never by parsing free message text, with
// DeliveryState preserved exactly as observed and zero retries (retry remains
// outside the adapter, under governor authority):
//   - 400 invalid argument -> upstream_http_failure;
//   - 413 request too large -> request_too_large;
//   - 504 deadline/gateway timeout -> timeout;
//   - 529 overloaded -> rate_or_capacity.
func TestStatusClassificationIsDeterministic(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantKind   googlecompat.ErrorKind
		wantStatus int
	}{
		{"400 invalid request", http.StatusBadRequest, googlecompat.ErrorUpstreamHTTPFailure, 400},
		{"413 request too large", http.StatusRequestEntityTooLarge, googlecompat.ErrorRequestTooLarge, 413},
		{"504 gateway timeout", http.StatusGatewayTimeout, googlecompat.ErrorTimeout, 504},
		{"529 overloaded", 529, googlecompat.ErrorRateCapacity, 529},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// The body deliberately tries to lie with free text: classification
			// must come from the status, never from message parsing.
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"error":{"code":12345,"message":"synthetic body must be ignored","status":"NOT_A_HINT"}}`))
			})
			client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
			response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
			if err == nil {
				t.Fatalf("status %d accepted as success", testCase.status)
			}
			var adapterErr *googlecompat.Error
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
			_, _ = w.Write([]byte(`{"error":{"code":13,"message":"boom","status":"INTERNAL"}}`))
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
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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
		_, _ = w.Write([]byte(validGenerateBody))
	}))
	defer target.Close()

	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = first.URL })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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

// TestRedirectWithTruncatedBodyNeverClaimsCompleted is the #97 review
// regression for the 3xx delivery-evidence blocker: the redirect path must
// count the body as fully read ONLY when the read completed without error. The
// server declares Content-Length 128, sends a few bytes and closes the
// connection, so the client observes a premature EOF: the delivery state must
// stay conservative (response_started), NEVER completed, while the redirect
// itself is still refused with zero replay.
func TestRedirectWithTruncatedBodyNeverClaimsCompleted(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 307 Temporary Redirect\r\nLocation: /elsewhere\r\nContent-Length: 128\r\nContent-Type: text/plain\r\n\r\npartial")
		// The connection closes right after "partial": the declared 128-byte
		// body is never delivered, so the client read ends in unexpected EOF.
	})
	client, _ := newTestClient(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL }, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "p"})
	if err == nil {
		t.Fatal("redirect accepted as success")
	}
	if !strings.Contains(err.Error(), "unsafe_redirect") {
		t.Fatalf("err = %v, want unsafe_redirect", err)
	}
	if response.Metadata.DeliveryState == provider.DeliveryCompleted {
		t.Fatal("truncated redirect body claimed completed despite premature EOF")
	}
	if response.Metadata.DeliveryState != provider.DeliveryResponseStarted {
		t.Fatalf("delivery = %v, want response_started (response began but never completed)", response.Metadata.DeliveryState)
	}
	if recorder.count() != 1 {
		t.Fatalf("physical requests = %d, want exactly 1: the refused redirect must not amplify", recorder.count())
	}
}

func TestExactlyOnePhysicalRequestPerNormalComplete(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved, _ := resolvedConfig(t, func(c *provider.Config) { c.BaseURL = recorder.server.URL })
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
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
		ProtocolFamily:  provider.FamilyGoogleCompatible,
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
		ProtocolFamily: provider.FamilyGoogleCompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			RouteSafety:    provider.ReceiptRouteSafety(),
		},
		ConfigIdentity: "identity",
	}
	if _, err := googlecompat.New(unsafe, nil, googlecompat.Options{}); err == nil {
		t.Fatal("New accepted receipt-route safety it does not implement")
	}

	// Wrong family is refused by New even if resolution were bypassed.
	wrongFamily := provider.Resolved{
		ProviderID:     "wrong-family",
		ProtocolFamily: provider.FamilyOpenAICompatible,
		BaseURL:        "http://127.0.0.1:1",
		Model:          "m",
		Profile:        provider.CapabilityProfile{ProfileVersion: "v1", RouteSafety: provider.SafeRouteSafety()},
		ConfigIdentity: "identity",
	}
	if _, err := googlecompat.New(wrongFamily, nil, googlecompat.Options{}); err == nil {
		t.Fatal("New accepted a non-google_compatible family")
	}
}

// TestInvalidProtocolOptionsRefusedAtConstruction covers the closed option
// vocabulary of the generateContent baseline: it is EMPTY, so any configured
// option must refuse the adapter with zero requests and no silent defaults.
func TestInvalidProtocolOptionsRefusedAtConstruction(t *testing.T) {
	optionCases := map[string]map[string]string{
		"max_tokens":        {"max_tokens": "1024"},
		"generation_config": {"generationConfig": "0.7"},
		"unknown single":    {"temperature": "0.7"},
		"unknown mixed":     {"maxOutputTokens": "128", "anthropic_version": "2023-06-01"},
		"credential shaped": {"some_option": "Bearer sk-123"},
	}
	for name, options := range optionCases {
		t.Run(name, func(t *testing.T) {
			resolved := provider.Resolved{
				ProviderID:      "option-gate",
				ProtocolFamily:  provider.FamilyGoogleCompatible,
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
			if _, err := googlecompat.New(resolved, nil, googlecompat.Options{}); err == nil {
				t.Fatalf("New accepted invalid options %v", options)
			}
			// Conservative state: refused constructors can never dispatch.
			recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("zero requests expected for invalid options")
			})
			resolved.BaseURL = recorder.server.URL
			if _, err := googlecompat.New(resolved, nil, googlecompat.Options{}); err == nil {
				t.Fatalf("New accepted invalid options against live server %v", options)
			}
			if recorder.count() != 0 {
				t.Fatalf("requests = %d, want 0", recorder.count())
			}
		})
	}

	// The empty vocabulary is exercised through the resolving path: a nil or
	// empty Options map is the ONLY valid option set for this baseline.
	resolved, _ := resolvedConfig(t, nil)
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{})
	if err != nil {
		t.Fatalf("New refused the empty option vocabulary: %v", err)
	}
	if client == nil {
		t.Fatal("empty vocabulary produced no client")
	}
}

func TestSecretNeverAppearsInSanitizedIdentityOrMetadata(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved, config := resolvedConfig(t, func(c *provider.Config) {
		c.BaseURL = recorder.server.URL
		c.AuthRequirement = provider.AuthReferenceRequired
		c.Auth = provider.SecretRef("TEST_SECRET_REF")
	})
	client, err := googlecompat.New(resolved, func(ctx context.Context, reference provider.SecretRef) (string, error) {
		return testSecret, nil
	}, googlecompat.Options{})
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
}
