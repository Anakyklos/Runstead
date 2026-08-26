package googlecompat_test

// Integration-boundary validation: the real Google/Gemini-compatible adapter
// (issue #89) driven through the governor's public Execute interface against a
// real HTTP server. This is the acceptance seam the adapter will live behind,
// so it must prove, end to end:
//
//  1. a SafeRouteSafety policy admits the adapter's declared route and the
//     completion returns the upstream text with exactly ONE physical request
//     (single-attempt accounting as announced);
//  2. policy/route mismatch fails closed at the governor boundary with zero
//     physical requests (the adapter's SafeRouteSafety() must be compared,
//     not trusted by convention);
//  3. the governor passes its own ClientRequestID through to the adapter's
//     wire header (correlation survives the boundary);
//  4. the full public acceptance chain (Config -> NewRegistry -> Resolve ->
//     googlecompat.New -> Governor.Execute -> real server) resolves
//     authentication only at dispatch, lands it on the wire as the
//     x-goog-api-key header, and never leaks the secret into identity,
//     results or evidence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
)

type googleCompatCounter struct {
	mu       sync.Mutex
	requests int
	lastPath string
	lastID   string
	lastAuth string
	lastBody map[string]any
}

func (c *googleCompatCounter) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests++
		c.lastPath = r.URL.Path
		c.lastID = r.Header.Get("X-Runstead-Client-Request-ID")
		c.lastAuth = r.Header.Get("x-goog-api-key")
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&c.lastBody)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req_integration_1")
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello from upstream"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":5}}`)
	}))
}

func (c *googleCompatCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *googleCompatCounter) authorization() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAuth
}

func (c *googleCompatCounter) snapshot() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPath, c.lastID
}

func newGoogleCompatAdapter(t *testing.T, baseURL string) *googlecompat.Client {
	t.Helper()
	client, err := googlecompat.New(provider.Resolved{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyGoogleCompatible,
		BaseURL:         baseURL,
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			RouteSafety:    provider.SafeRouteSafety(),
		},
		ConfigIdentity: "identity",
	}, nil, googlecompat.Options{})
	if err != nil {
		t.Fatalf("googlecompat.New() error = %v", err)
	}
	return client
}

// integrationConfig mirrors fastConfig from the governor test package: an
// instant single-attempt policy that passes Config.Validate without pacing
// delays between scenarios.
func integrationConfig() policy.Config {
	config := policy.DefaultInstantConfig("policy-account-1", "gateway-a", "model-pool", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Rolling3h = 100
	config.Rolling1h = 90
	config.Rolling10m = 80
	config.ManualReserve = 10
	config.TaskBudget = 4
	config.RetryBudget = 2
	return config
}

func TestGoogleCompatAdapterServesGovernorExecuteWithExactlyOnePhysicalRequest(t *testing.T) {
	counter := &googleCompatCounter{}
	server := counter.server()
	defer server.Close()

	client := newGoogleCompatAdapter(t, server.URL)

	config := integrationConfig()
	config.Model = "model-a"
	governor, err := policy.New(config, policy.Options{})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}

	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model-pool",
		ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
	}, client, nil)

	if result.Err != nil {
		t.Fatalf("Execute() err = %v", result.Err)
	}
	if result.Completion.Err != nil {
		t.Fatalf("Execute() completion err = %v", result.Completion.Err)
	}
	if !result.Admission.Admitted() {
		t.Fatalf("Execute() admission = %#v, want admitted", result.Admission)
	}
	if got := result.Response.Text; got != "hello from upstream" {
		t.Fatalf("Response.Text = %q, want upstream text", got)
	}
	if got := result.Completion.DeliveryState; got != provider.DeliveryCompleted {
		t.Fatalf("DeliveryState = %s, want completed", got)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("physical requests = %d, want exactly 1", got)
	}

	path, id := counter.snapshot()
	if path != "/models/model-a:generateContent" {
		t.Fatalf("wire path = %q, want /models/model-a:generateContent", path)
	}
	if id != "request-1" {
		t.Fatalf("wire client request ID = %q, want governor-issued request-1", id)
	}
}

func TestGoogleCompatAdapterRejectedFailClosedOnRouteMismatchWithZeroRequests(t *testing.T) {
	counter := &googleCompatCounter{}
	server := counter.server()
	defer server.Close()

	client := newGoogleCompatAdapter(t, server.URL)

	// A receipt-aware policy is a valid governor config, but this adapter
	// declares/executes single-attempt safe-route semantics. The boundary
	// must refuse admission on the declaration mismatch before any request
	// can leave this process.
	config := integrationConfig()
	config.Model = "concrete-model"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AttemptProviderID = "gateway-a"
	config.AccountLaneHash = "lane"
	config.RouteSafety = provider.ReceiptRouteSafety()
	governor, err := policy.New(config, policy.Options{})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}

	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model-pool",
		ProviderRequest: provider.Request{Model: "concrete-model", Prompt: "prompt"},
	}, client, nil)

	if result.Admission.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("admission = %#v, want unsafe_provider_amplification", result.Admission)
	}
	if result.Admission.Err == nil {
		t.Fatal("admission err = nil, want fail-closed route mismatch error")
	}
	if got := counter.count(); got != 0 {
		t.Fatalf("physical requests = %d, want exactly 0 on fail-closed mismatch", got)
	}
}

// TestGoogleCompatAdapterFullPublicPathConfigToWireWithAuthSecret drives the
// whole public acceptance chain for one completion: provider.Config ->
// provider.NewRegistry -> Registry.Resolve (capability + route-safety +
// auth-reference validation) -> googlecompat.New with a SecretResolver ->
// Governor.Execute against a real server. It proves the secret reference is
// resolved only at dispatch time, lands on the wire as the x-goog-api-key
// header, and never appears in the config identity or in the execution
// result/evidence.
func TestGoogleCompatAdapterFullPublicPathConfigToWireWithAuthSecret(t *testing.T) {
	counter := &googleCompatCounter{}
	server := counter.server()
	defer server.Close()

	const secretValue = "synthetic-secret-value-not-in-identity"
	registry, err := provider.NewRegistry(provider.Config{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyGoogleCompatible,
		BaseURL:         server.URL,
		Model:           "model-a",
		Auth:            provider.SecretRef("vault://runstead/synthetic"),
		AuthRequirement: provider.AuthReferenceRequired,
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			Capabilities: provider.Capabilities{
				provider.CapabilityTextTurn:         true,
				provider.CapabilityRunsteadProtocol: true,
			},
			RouteSafety: provider.SafeRouteSafety(),
		},
		ConfigVersion: "v1",
	})
	if err != nil {
		t.Fatalf("provider.NewRegistry() error = %v", err)
	}

	resolved, err := registry.Resolve("gateway-a", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("Registry.Resolve() error = %v", err)
	}
	if strings.Contains(resolved.ConfigIdentity, secretValue) {
		t.Fatal("secret value leaked into resolved config identity")
	}
	if resolved.Auth != "vault://runstead/synthetic" {
		t.Fatalf("resolved auth reference = %q, want the non-secret reference", resolved.Auth)
	}

	client, err := googlecompat.New(*resolved, func(_ context.Context, reference provider.SecretRef) (string, error) {
		return secretValue, nil
	}, googlecompat.Options{})
	if err != nil {
		t.Fatalf("googlecompat.New() error = %v", err)
	}

	config := integrationConfig()
	config.Model = "model-a"
	governor, err := policy.New(config, policy.Options{})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}

	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-3",
		ClientRequestID: "request-3",
		ModelPool:       "model-pool",
		ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
	}, client, nil)

	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("Execute() errors = %v / %v", result.Err, result.Completion.Err)
	}
	if got := result.Response.Text; got != "hello from upstream" {
		t.Fatalf("Response.Text = %q, want upstream text", got)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("physical requests = %d, want exactly 1", got)
	}
	if got := counter.authorization(); got != secretValue {
		t.Fatalf("wire x-goog-api-key = %q, want the resolved secret material", got)
	}
	if strings.Contains(fmt.Sprintf("%#v", result), secretValue) {
		t.Fatal("secret value leaked into the execution result/evidence")
	}
}

// TestGoogleCompatAdapterRateLimitCrossesGovernorBoundaryWithSingleRequest
// proves the adapter's stable, sanitized error classification crosses the
// governor boundary through the PUBLIC OutcomeClassifier seam: a caller-owned
// classifier reads googlecompat.Error.Kind/DeliveryState via errors.As (no
// vendor coupling in governor production code), the attempt is debited exactly
// once, and the upstream saw exactly one physical request even on a 429.
func TestGoogleCompatAdapterRateLimitCrossesGovernorBoundaryWithSingleRequest(t *testing.T) {
	counter := &googleCompatCounter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.mu.Lock()
		counter.requests++
		counter.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited","status":"RESOURCE_EXHAUSTED"}}`)
	}))
	defer server.Close()

	client := newGoogleCompatAdapter(t, server.URL)

	config := integrationConfig()
	governor, err := policy.New(config, policy.Options{})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}

	var seenRetryAfter time.Duration
	classifier := func(response provider.Response, err error) policy.Outcome {
		var upstreamErr *googlecompat.Error
		if errors.As(err, &upstreamErr) {
			seenRetryAfter = upstreamErr.RetryAfter
			switch upstreamErr.Kind {
			case googlecompat.ErrorRateCapacity:
				return policy.Outcome{
					Class:           policy.OutcomeRateCapacity,
					UpstreamReached: upstreamErr.UpstreamReached,
					DeliveryState:   upstreamErr.DeliveryState,
				}
			}
		}
		return policy.Outcome{Class: policy.OutcomeUncertainReached, UpstreamReached: true, DeliveryState: response.Metadata.DeliveryState}
	}

	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-4",
		ClientRequestID: "request-4",
		ModelPool:       "model-pool",
		ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
	}, client, classifier)

	if result.Err == nil {
		t.Fatal("Execute() err = nil, want the classified rate-limit error")
	}
	if result.Completion.Outcome != policy.OutcomeRateCapacity {
		t.Fatalf("completion outcome = %q, want rate_or_capacity", result.Completion.Outcome)
	}
	if result.Completion.AttemptDebited != 1 {
		t.Fatalf("attempts debited = %d, want exactly 1", result.Completion.AttemptDebited)
	}
	if result.Completion.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("completion delivery state = %s, want completed (429 body fully read)", result.Completion.DeliveryState)
	}
	if seenRetryAfter != time.Second {
		t.Fatalf("classifier saw RetryAfter = %s, want 1s (stable duration surfaced publicly)", seenRetryAfter)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("physical requests = %d, want exactly 1 on a governed rate-limited attempt", got)
	}
}

// TestGoogleCompatAdapterRecoveryRelevantStatusesCrossGovernorBoundary
// proves, through Governor.Execute, that the statuses which influence future
// retry/cooldown policy decisions (504 timeout, 529 overloaded, 413 request
// too large) cross the public boundary with their stable adapter
// classification intact. For every case: exactly one physical request (no
// automatic retry anywhere), exactly one attempt debited by the governor, the
// caller-owned OutcomeClassifier sees the typed Kind and maps it to the
// matching public outcome, and the delivery evidence stays conservative
// (completed: the error body was fully received; never not_sent).
func TestGoogleCompatAdapterRecoveryRelevantStatusesCrossGovernorBoundary(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		wantKind    googlecompat.ErrorKind
		wantOutcome policy.OutcomeClass
	}{
		{"504 gateway timeout", http.StatusGatewayTimeout, googlecompat.ErrorTimeout, policy.OutcomeTimeout},
		{"529 overloaded", 529, googlecompat.ErrorRateCapacity, policy.OutcomeRateCapacity},
		{"413 request too large", http.StatusRequestEntityTooLarge, googlecompat.ErrorRequestTooLarge, policy.OutcomeUncertainReached},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			counter := &googleCompatCounter{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				counter.mu.Lock()
				counter.requests++
				counter.mu.Unlock()
				w.WriteHeader(testCase.status)
				fmt.Fprint(w, `{"error":{"message":"synthetic"}}`)
			}))
			defer server.Close()

			client := newGoogleCompatAdapter(t, server.URL)
			config := integrationConfig()
			governor, err := policy.New(config, policy.Options{})
			if err != nil {
				t.Fatalf("policy.New() error = %v", err)
			}

			var sawKind googlecompat.ErrorKind
			var sawUpstream bool
			classifier := func(response provider.Response, err error) policy.Outcome {
				var adapterErr *googlecompat.Error
				if errors.As(err, &adapterErr) {
					sawKind = adapterErr.Kind
					sawUpstream = adapterErr.UpstreamReached
					switch sawKind {
					case testCase.wantKind:
						return policy.Outcome{
							Class:           testCase.wantOutcome,
							UpstreamReached: adapterErr.UpstreamReached,
							DeliveryState:   adapterErr.DeliveryState,
						}
					}
				}
				return policy.Outcome{Class: policy.OutcomeUncertainReached, UpstreamReached: true, DeliveryState: response.Metadata.DeliveryState}
			}

			result := governor.Execute(context.Background(), policy.AttemptRequest{
				TaskID:          "task-rec-" + testCase.name,
				ClientRequestID: "request-rec-" + testCase.name,
				ModelPool:       "model-pool",
				ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
			}, client, classifier)

			if result.Err == nil {
				t.Fatal("Execute() err = nil, want the classified upstream error")
			}
			if sawKind != testCase.wantKind {
				t.Fatalf("adapter kind seen by classifier = %q, want %q", sawKind, testCase.wantKind)
			}
			if !sawUpstream {
				t.Fatalf("%s marked UpstreamReached = false, want true", testCase.name)
			}
			if result.Completion.Outcome != testCase.wantOutcome {
				t.Fatalf("completion outcome = %q, want %q", result.Completion.Outcome, testCase.wantOutcome)
			}
			if result.Completion.AttemptDebited != 1 {
				t.Fatalf("attempts debited = %d, want exactly 1", result.Completion.AttemptDebited)
			}
			if result.Completion.DeliveryState != provider.DeliveryCompleted {
				t.Fatalf("delivery state = %s, want completed (error body fully read, conservative evidence)", result.Completion.DeliveryState)
			}
			if got := counter.count(); got != 1 {
				t.Fatalf("physical requests = %d, want exactly 1 on %s (zero automatic retries)", got, testCase.name)
			}
		})
	}
}

// TestGoogleCompatAdapterTimeoutCrossesGovernorBoundaryConservatively proves
// deadline semantics through the public Execute interface: a context timeout
// mid-flight is classified as OutcomeTimeout by the governor's default
// classifier, the attempt is debited exactly once, the delivery evidence stays
// conservative (never degraded to not_sent: the request left this process),
// and the upstream saw exactly one physical request with zero retries.
func TestGoogleCompatAdapterTimeoutCrossesGovernorBoundaryConservatively(t *testing.T) {
	counter := &googleCompatCounter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.mu.Lock()
		counter.requests++
		counter.mu.Unlock()
		// The client returns at its own 300ms deadline; the upstream connection
		// lingers (stdlib transport behavior with a non-empty body, observed via
		// probe: the request fully left this process and no second request is
		// ever emitted). The 2s fallback bounds this test while still proving
		// the client-side deadline contract.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	client := newGoogleCompatAdapter(t, server.URL)

	config := integrationConfig()
	governor, err := policy.New(config, policy.Options{})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := governor.Execute(ctx, policy.AttemptRequest{
		TaskID:          "task-5",
		ClientRequestID: "request-5",
		ModelPool:       "model-pool",
		ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
	}, client, nil)

	if result.Err == nil {
		t.Fatal("Execute() err = nil, want the deadline classification")
	}
	if result.Completion.Outcome != policy.OutcomeTimeout {
		t.Fatalf("completion outcome = %q, want timeout", result.Completion.Outcome)
	}
	if result.Completion.AttemptDebited != 1 {
		t.Fatalf("attempts debited = %d, want exactly 1", result.Completion.AttemptDebited)
	}
	state := result.Completion.DeliveryState
	if state == provider.DeliveryNotSent {
		t.Fatal("post-dispatch timeout claimed not_sent; delivery evidence must stay conservative at the governor boundary")
	}
	if state != provider.DeliverySentConfirmed && state != provider.DeliverySentUnconfirmed && state != provider.DeliveryResponseStarted {
		t.Fatalf("delivery = %s, want a conservative sent/response state", state)
	}
	if got := counter.count(); got != 1 {
		t.Fatalf("physical requests = %d, want exactly 1 on a governed timeout (zero retries)", got)
	}
}
