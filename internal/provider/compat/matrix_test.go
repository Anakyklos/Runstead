package compat

// The shared deterministic compatibility suite (#14). One provider-neutral
// harness exercises the same contract properties across all three supported
// protocol families through local httptest servers that speak each family's
// wire subset. CI needs no Internet and no credentials. The servers are
// intentionally provider-shaped: they parse the family request, validate the
// exact configured model, count physical requests and wrap the deterministic
// runstead.protocol.v1 text in the family response envelope. Everything after
// the provider boundary is the real runtime contract (provider.Client,
// RouteSafety, delivery evidence, governor accounting).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

var contractFamilies = []provider.ProtocolFamily{
	provider.FamilyOpenAICompatible,
	provider.FamilyAnthropicCompatible,
	provider.FamilyGoogleCompatible,
}

const testModel = "deterministic-model"

// ------------------------------ synthetic family wire ------------------------------

// familyWire is the provider-shaped httptest double for one protocol family.
type familyWire struct {
	family provider.ProtocolFamily
	// responses are returned in order; the last one repeats. They are the
	// runstead.protocol.v1 text envelopes the runtime must see after the
	// adapter decodes the family response.
	responses []string
	// validate inspects the raw family request (method, path, wire body).
	validate func(method string, path string, raw []byte) error
	mu       sync.Mutex
	requests int
	// block, when non-nil, makes the handler stall until closed (used for
	// possible-dispatch cancellation evidence).
	block chan struct{}
	// redirect, when non-nil, answers the next request with a 3xx Location.
	redirect *string
}

func newFamilyWire(family provider.ProtocolFamily, responses ...string) *familyWire {
	return &familyWire{family: family, responses: append([]string(nil), responses...)}
}

func (w *familyWire) handler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		w.mu.Lock()
		w.requests++
		count := w.requests
		redirect := w.redirect
		w.redirect = nil
		block := w.block
		w.mu.Unlock()

		if redirect != nil {
			response.Header().Set("Location", *redirect)
			response.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		raw, _ := io.ReadAll(request.Body)
		if w.validate != nil {
			if err := w.validate(request.Method, request.URL.Path, raw); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if block != nil {
			// Stall until the test releases the attempt (close). A released
			// or closed channel lets the handler continue and answer: the
			// release is not a new dispatch decision.
			<-block
		}
		prompt := extractPrompt(w.family, raw)
		index := count - 1
		if index >= len(w.responses) {
			index = len(w.responses) - 1
		}
		text := w.responses[index]
		if prompt == "" && text == "" {
			http.Error(response, "unexpected empty prompt", http.StatusBadRequest)
			return
		}
		body := wrapResponse(w.family, text)
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-Id", fmt.Sprintf("req-%s-%d", w.family, count))
		response.Header().Set("x-goog-request-id", fmt.Sprintf("req-%s-%d", w.family, count))
		response.Header().Set("request-id", fmt.Sprintf("req-%s-%d", w.family, count))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(body)
	})
}

func (w *familyWire) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

func endpointPath(family provider.ProtocolFamily) string {
	switch family {
	case provider.FamilyOpenAICompatible:
		return "/chat/completions"
	case provider.FamilyAnthropicCompatible:
		return "/v1/messages"
	case provider.FamilyGoogleCompatible:
		return "/v1beta/models/" + testModel + ":generateContent"
	default:
		return ""
	}
}

func extractPrompt(family provider.ProtocolFamily, raw []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if messages, ok := parsed["messages"].([]any); ok {
		for _, entry := range messages {
			message, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := message["role"].(string); role != "user" {
				continue
			}
			if content, ok := message["content"].(string); ok {
				return content
			}
		}
		return ""
	}
	if contents, ok := parsed["contents"].([]any); ok && len(contents) > 0 {
		first, ok := contents[0].(map[string]any)
		if !ok {
			return ""
		}
		parts, ok := first["parts"].([]any)
		if !ok {
			return ""
		}
		for _, part := range parts {
			entry, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := entry["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func wrapResponse(family provider.ProtocolFamily, text string) []byte {
	var envelope string
	switch family {
	case provider.FamilyOpenAICompatible:
		envelope = fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}]}`, mustJSONString(text))
	case provider.FamilyAnthropicCompatible:
		envelope = fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"stop_reason":"end_turn"}`, mustJSONString(text))
	case provider.FamilyGoogleCompatible:
		envelope = fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":%s}]},"finishReason":"STOP"}]}`, mustJSONString(text))
	}
	return []byte(envelope)
}

func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// ------------------------------ fixture construction ------------------------------

// familyOptions returns the non-secret protocol options each family baseline
// requires (empty where the adapter implements an empty option vocabulary).
func familyOptions(family provider.ProtocolFamily) map[string]string {
	switch family {
	case provider.FamilyAnthropicCompatible:
		return map[string]string{"max_tokens": "256", "anthropic_version": "2023-06-01"}
	default:
		return nil
	}
}

func testProfile() provider.CapabilityProfile {
	return provider.CapabilityProfile{
		ProfileVersion: "v1",
		Capabilities: provider.Capabilities{
			provider.CapabilityTextTurn:         true,
			provider.CapabilityRunsteadProtocol: true,
		},
		RouteSafety: provider.SafeRouteSafety(),
	}
}

func resolveFixture(t *testing.T, family provider.ProtocolFamily, baseURL, providerID, model string, authRequirement provider.AuthRequirement, authRef provider.SecretRef, mutate func(*provider.Config)) *provider.Resolved {
	t.Helper()
	config := provider.Config{
		ProviderID:      providerID,
		ProtocolFamily:  family,
		BaseURL:         baseURL,
		Model:           model,
		AuthRequirement: authRequirement,
		Auth:            authRef,
		Options:         familyOptions(family),
		Profile:         testProfile(),
		ConfigVersion:   "v1",
	}
	if mutate != nil {
		mutate(&config)
	}
	registry, err := provider.NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, err := registry.Resolve(providerID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return resolved
}

func buildClient(t *testing.T, family provider.ProtocolFamily, providerID string, responses ...string) (*provider.Resolved, provider.Client, *familyWire) {
	t.Helper()
	wire := newFamilyWire(family, responses...)
	wire.validate = func(method string, path string, raw []byte) error {
		if method != http.MethodPost {
			return fmt.Errorf("method = %s, want POST", method)
		}
		if !strings.HasSuffix(path, endpointPath(family)) {
			return fmt.Errorf("path = %s, want suffix %s", path, endpointPath(family))
		}
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("invalid family request JSON: %v", err)
		}
		declared, _ := parsed["model"].(string)
		if declared != "" && declared != testModel {
			return fmt.Errorf("model = %q, want %q", declared, testModel)
		}
		return nil
	}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	resolver := func(context.Context, provider.SecretRef) (string, error) {
		return "synthetic-token", nil
	}
	resolved := resolveFixture(t, family, server.URL+"/v1", providerID, testModel, provider.AuthNone, "", nil)
	client, err := New(*resolved, resolver)
	if err != nil {
		t.Fatalf("compat.New: %v", err)
	}
	return resolved, client, wire
}

// ------------------------------ contract properties ------------------------------

// TestMatrixIdentityDistinctFromFamilyAndSingleRequestPerAdmission proves, for
// every family, that two different provider_ids share the same protocol
// adapter/family with no identity-to-family coupling, and that every normal
// governed completion produces exactly one physical HTTP request.
func TestMatrixIdentityDistinctFromFamilyAndSingleRequestPerAdmission(t *testing.T) {
	script := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"ok","evidence":[]}</runstead_final>`,
	}
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			wireA := newFamilyWire(family, script...)
			serverA := httptest.NewServer(wireA.handler())
			t.Cleanup(serverA.Close)
			wireB := newFamilyWire(family, script...)
			serverB := httptest.NewServer(wireB.handler())
			t.Cleanup(serverB.Close)

			// Two distinct provider identities, same family, same adapter path.
			resolvedA := resolveFixture(t, family, serverA.URL+"/v1", "identity-a", "model-a", provider.AuthNone, "", nil)
			resolvedB := resolveFixture(t, family, serverB.URL+"/v1", "identity-b", "model-b", provider.AuthNone, "", nil)
			clientA, err := New(*resolvedA, nil)
			if err != nil {
				t.Fatal(err)
			}
			clientB, err := New(*resolvedB, nil)
			if err != nil {
				t.Fatal(err)
			}

			// Identity and family are distinct concepts: neither derives from
			// the other, and both are observable in sanitized form.
			if resolvedA.ProviderID == string(resolvedA.ProtocolFamily) {
				t.Fatalf("provider identity must not equal the protocol family")
			}
			if resolvedA.ProtocolFamily != family || resolvedB.ProtocolFamily != family {
				t.Fatalf("both providers must resolve to family %q", family)
			}
			if resolvedA.ProviderID == resolvedB.ProviderID {
				t.Fatalf("provider ids must be distinct")
			}
			if resolvedA.ConfigIdentity == "" || !strings.Contains(resolvedA.ConfigIdentity, "identity-a") {
				t.Fatalf("config identity must expose the sanitized provider identity: %s", resolvedA.ConfigIdentity)
			}

			// Two completions per provider: exactly two physical requests each,
			// and each response carries the deterministic protocol text.
			for turn := 0; turn < 2; turn++ {
				pairs := []struct {
					client provider.Client
					id     string
					model  string
					wire   *familyWire
				}{
					{clientA, "identity-a", resolvedA.Model, wireA},
					{clientB, "identity-b", resolvedB.Model, wireB},
				}
				for _, pair := range pairs {
					response, err := pair.client.Complete(context.Background(), provider.Request{
						Model:           pair.model,
						Prompt:          "turn " + strconv.Itoa(turn),
						ClientRequestID: pair.id + "-" + strconv.Itoa(turn),
					})
					if err != nil {
						t.Fatalf("Complete(%s): %v", pair.id, err)
					}
					if response.Text != script[turn] {
						t.Fatalf("response text = %q, want %q", response.Text, script[turn])
					}
				}
			}
			if got := wireA.count(); got != 2 {
				t.Fatalf("physical requests for identity-a = %d, want exactly 2", got)
			}
			if got := wireB.count(); got != 2 {
				t.Fatalf("physical requests for identity-b = %d, want exactly 2", got)
			}
		})
	}
}

// TestMatrixRedirectRefusedWithoutSecondRequest proves redirect-following
// never amplifies model-effect requests: a 3xx response is refused and the
// count stays at exactly one physical request.
func TestMatrixRedirectRefusedWithoutSecondRequest(t *testing.T) {
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			wire := newFamilyWire(family, "ignored")
			location := "https://evil.invalid/model-effect"
			wire.redirect = &location
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			resolved := resolveFixture(t, family, server.URL, "redirect-provider", testModel, provider.AuthNone, "", nil)
			client, err := New(*resolved, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Complete(context.Background(), provider.Request{Model: testModel, Prompt: "turn"})
			if err == nil {
				t.Fatalf("redirect must be refused, got success %q", response.Text)
			}
			if response.Metadata.DeliveryState == provider.DeliveryNotSent {
				t.Fatalf("redirect refusal must keep the observed delivery state, got not_sent")
			}
			if got := wire.count(); got != 1 {
				t.Fatalf("physical requests = %d, want exactly 1 (a redirect must not be followed)", got)
			}
		})
	}
}

// TestMatrixCancellationBeforeDispatchZeroRequests proves a cancel before any
// dispatch produces provably zero physical requests.
func TestMatrixCancellationBeforeDispatchZeroRequests(t *testing.T) {
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			wire := newFamilyWire(family, "ignored")
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			resolved := resolveFixture(t, family, server.URL, "cancel-provider", testModel, provider.AuthNone, "", nil)
			client, err := New(*resolved, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			response, err := client.Complete(ctx, provider.Request{Model: testModel, Prompt: "turn"})
			if err == nil {
				t.Fatalf("canceled context must fail, got success")
			}
			if response.Metadata.DeliveryState != provider.DeliveryNotSent {
				t.Fatalf("pre-dispatch cancel must be not_sent, got %s", response.Metadata.DeliveryState)
			}
			if got := wire.count(); got != 0 {
				t.Fatalf("physical requests = %d, want 0", got)
			}
		})
	}
}

// TestMatrixCancellationAfterPossibleDispatchIsConservativeAndNeverReplays
// proves timeout/cancel after possible dispatch stays conservatively uncertain
// (never not_sent) and never auto-replays the same attempt.
func TestMatrixCancellationAfterPossibleDispatchIsConservativeAndNeverReplays(t *testing.T) {
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			wire := newFamilyWire(family, "ignored")
			wire.block = make(chan struct{})
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			resolved := resolveFixture(t, family, server.URL, "timeout-provider", testModel, provider.AuthNone, "", nil)
			client, err := New(*resolved, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			response, err := client.Complete(ctx, provider.Request{Model: testModel, Prompt: "turn"})
			if err == nil {
				t.Fatalf("stalled upstream must fail, got success")
			}
			if response.Metadata.DeliveryState == provider.DeliveryNotSent {
				t.Fatalf("possible-dispatch cancel must never claim not_sent")
			}
			// Exactly one physical request: the adapter must not retry the
			// same attempt on its own.
			if got := wire.count(); got != 1 {
				t.Fatalf("physical requests after cancel = %d, want exactly 1 (no hidden retry or replay)", got)
			}
			close(wire.block)
			// A NEW admission (new client call, new context) is a separate
			// governed attempt and makes exactly one more request.
			response, err = client.Complete(context.Background(), provider.Request{Model: testModel, Prompt: "next"})
			if err != nil {
				t.Fatalf("new attempt after release: %v", err)
			}
			_ = response
			if got := wire.count(); got != 2 {
				t.Fatalf("physical requests = %d, want 2 (one per admission)", got)
			}
		})
	}
}

// TestMatrixFailClosedBeforeDispatch covers the pre-dispatch fail-closed
// matrix: unknown provider, unknown family, missing capability, missing
// model, invalid endpoint, required-but-missing auth reference and
// incompatible route safety. No client is ever built, so zero dispatch.
func TestMatrixFailClosedBeforeDispatch(t *testing.T) {
	base := func() provider.Config {
		return provider.Config{
			ProviderID:      "fail-provider",
			ProtocolFamily:  provider.FamilyOpenAICompatible,
			BaseURL:         "http://127.0.0.1:1/v1",
			Model:           "model",
			AuthRequirement: provider.AuthNone,
			Profile:         testProfile(),
		}
	}
	cases := []struct {
		name   string
		mutate func(*provider.Config)
	}{
		{"unknown provider id", func(config *provider.Config) {}},
		{"unknown protocol family", func(config *provider.Config) {
			config.ProtocolFamily = "bogus_family"
		}},
		{"missing required capability", func(config *provider.Config) {
			config.Profile.Capabilities = provider.Capabilities{provider.CapabilityTextTurn: true}
		}},
		{"missing model", func(config *provider.Config) {
			config.Model = ""
		}},
		{"invalid endpoint", func(config *provider.Config) {
			config.BaseURL = "ftp://user:pass@host/"
		}},
		{"endpoint with credentials", func(config *provider.Config) {
			config.BaseURL = "https://user:pass@host/v1"
		}},
		{"auth required without reference", func(config *provider.Config) {
			config.AuthRequirement = provider.AuthReferenceRequired
			config.Auth = ""
		}},
		{"unknown auth requirement", func(config *provider.Config) {
			config.AuthRequirement = "maybe"
		}},
		{"incompatible route safety", func(config *provider.Config) {
			config.Profile.RouteSafety = provider.ReceiptRouteSafety()
		}},
		{"credential-shaped option", func(config *provider.Config) {
			config.Options = map[string]string{"x": "Bearer abc"}
		}},
	}
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			for _, testCase := range cases {
				testCase := testCase
				t.Run(testCase.name, func(t *testing.T) {
					config := base()
					config.ProtocolFamily = family
					testCase.mutate(&config)
					registry, err := provider.NewRegistry(config)
					if err != nil {
						t.Fatalf("registry construction must accept operator input; validation happens at resolve: %v", err)
					}
					resolveID := "fail-provider"
					if testCase.name == "unknown provider id" {
						resolveID = "missing"
					}
					_, err = registry.Resolve(resolveID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
					if err == nil {
						t.Fatalf("configuration must fail closed, resolved successfully")
					}
					if errorContains(err, "identity-a") || errorContains(err, "sk-") || errorContains(err, "Bearer") {
						t.Fatalf("error must stay sanitized, got: %v", err)
					}
				})
			}
		})
	}
}

func errorContains(err error, fragment string) bool {
	return strings.Contains(err.Error(), fragment)
}

// TestMatrixSecretMinimization proves credentials never appear in errors,
// sanitized identity, response metadata or the test's own fixtures.
func TestMatrixSecretMinimization(t *testing.T) {
	const secret = "sk-super-secret-value-1234567890"
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			wire := newFamilyWire(family, "<runstead_final>ok</runstead_final>")
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			config := provider.Config{
				ProviderID:      "secret-provider",
				ProtocolFamily:  family,
				BaseURL:         server.URL + "/v1",
				Model:           testModel,
				Auth:            "TEST_TOKEN_ENV",
				AuthRequirement: provider.AuthReferenceRequired,
				Options:         familyOptions(family),
				Profile:         testProfile(),
				ConfigVersion:   "v1",
			}
			registry, err := provider.NewRegistry(config)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := registry.Resolve("secret-provider", provider.RequiredCapabilities(), provider.SafeRouteSafety())
			if err != nil {
				t.Fatal(err)
			}
			identity := provider.IdentityFromResolved(*resolved, AdapterVersion)
			if strings.Contains(resolved.ConfigIdentity, secret) || strings.Contains(identity.ConfigIdentity, secret) {
				t.Fatalf("sanitized identity leaked the credential value")
			}
			client, err := New(*resolved, func(context.Context, provider.SecretRef) (string, error) {
				return secret, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Complete(context.Background(), provider.Request{Model: testModel, Prompt: "turn"})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if strings.Contains(response.Metadata.Endpoint, secret) || strings.Contains(response.Metadata.RequestID, secret) {
				t.Fatalf("response metadata leaked the credential value")
			}
			// Missing-secret dispatch fails before any request with a
			// sanitized error.
			failing, err := New(*resolved, func(context.Context, provider.SecretRef) (string, error) {
				return "", errors.New("unavailable")
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err = failing.Complete(context.Background(), provider.Request{Model: testModel, Prompt: "turn"})
			if err == nil {
				t.Fatalf("missing secret must fail")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked the credential value: %v", err)
			}
			if response.Metadata.DeliveryState != provider.DeliveryNotSent {
				t.Fatalf("missing secret must refuse before dispatch, got %s", response.Metadata.DeliveryState)
			}
		})
	}
}

// TestMatrixAdapterCompatibilitySurface proves the composition layer selects
// each family adapter through the shared contract and that provider-neutral
// identity carries the adapter version.
func TestMatrixAdapterCompatibilitySurface(t *testing.T) {
	for _, family := range contractFamilies {
		family := family
		t.Run(string(family), func(t *testing.T) {
			resolved := resolveFixture(t, family, "http://127.0.0.1:1", "surface-provider", testModel, provider.AuthNone, "", nil)
			identity := provider.IdentityFromResolved(*resolved, AdapterVersion)
			if identity.ProviderID != "surface-provider" {
				t.Fatalf("identity provider id = %q", identity.ProviderID)
			}
			if identity.ProtocolFamily != family {
				t.Fatalf("identity family = %q, want %q", identity.ProtocolFamily, family)
			}
			if identity.AdapterVersion != AdapterVersion {
				t.Fatalf("identity adapter version = %q, want %q", identity.AdapterVersion, AdapterVersion)
			}
			if !strings.Contains(identity.ConfigIdentity, string(family)) {
				t.Fatalf("config identity must name the protocol family: %s", identity.ConfigIdentity)
			}
			client, err := New(*resolved, nil)
			if err != nil {
				t.Fatalf("client construction through the shared surface: %v", err)
			}
			switch family {
			case provider.FamilyOpenAICompatible:
				if _, ok := client.(*openaicompat.Client); !ok {
					t.Fatalf("expected openaicompat client")
				}
			case provider.FamilyAnthropicCompatible:
				if _, ok := client.(*anthropiccompat.Client); !ok {
					t.Fatalf("expected anthropiccompat client")
				}
			case provider.FamilyGoogleCompatible:
				if _, ok := client.(*googlecompat.Client); !ok {
					t.Fatalf("expected googlecompat client")
				}
			}
		})
	}
}
