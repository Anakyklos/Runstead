package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const (
	defaultTimeout        = 60 * time.Second
	defaultResponseLimit  = 8 << 20
	familyChatCompletions = "chat/completions"
)

// SecretResolver turns a non-secret provider.SecretRef into the actual
// authentication material at dispatch time. It is the single narrow seam
// between this adapter and whatever secret source the operator uses (env var,
// secret store, credential file); it is NOT a universal secret manager.
// Implementations must never log or persist resolved values.
type SecretResolver func(ctx context.Context, reference provider.SecretRef) (string, error)

// Options configures one adapter instance from an already validated and
// resolved configuration (provider.Resolved, contract #79). The struct holds
// no secret material: references are resolved through SecretResolver only when
// AuthReferenceRequired demands it, immediately before the model-effect
// request is built.
//
// There is deliberately NO way to inject an http.Client or RoundTripper. An
// opaque transport can amplify attempts (retries, fallbacks, fan-out) inside a
// single RoundTrip call where this adapter cannot observe or account for them,
// so any injected stack would make the SafeRouteSafety declaration unprovable.
// The adapter owns the whole dispatch stack; Now exists solely to make time
// deterministic in tests and cannot amplify requests.
type Options struct {
	Now func() time.Time
}

// Client implements provider.Client for any endpoint of the
// openai_compatible family. One Complete call performs exactly one physical
// HTTP request; retries, fallbacks, rotation and redirect following do not
// exist in this adapter. The zero value is not usable.
type Client struct {
	resolved    provider.Resolved
	secret      SecretResolver
	httpClient  *http.Client
	now         func() time.Time
	maxResponse int
}

var (
	_ provider.Client      = (*Client)(nil)
	_ provider.SafetyAware = (*Client)(nil)
)

// New builds an OpenAI-compatible protocol adapter around a resolved provider
// configuration. Every check here re-verifies, defensively, what #79
// resolution already proved: wrong family, unsafe route safety or missing
// authentication plumbing fail closed before any dispatch is possible.
func New(resolved provider.Resolved, resolver SecretResolver, options Options) (*Client, error) {
	if resolved.ProviderID == "" || resolved.ConfigIdentity == "" {
		return nil, configRefusedError(errors.New("adapter requires a fully resolved provider configuration"))
	}
	if resolved.ProtocolFamily != provider.FamilyOpenAICompatible {
		return nil, configRefusedError(fmt.Errorf("protocol family %q is not %q", string(resolved.ProtocolFamily), string(provider.FamilyOpenAICompatible)))
	}
	if err := resolved.Profile.RouteSafety.Validate(); err != nil {
		return nil, configRefusedError(err)
	}
	if !resolved.Profile.RouteSafety.Equal(provider.SafeRouteSafety()) {
		return nil, configRefusedError(errors.New("this initial adapter implements only the safe single-attempt route; receipt accounting would be unproven pretense"))
	}
	if _, err := chatCompletionsURL(resolved.BaseURL); err != nil {
		return nil, configRefusedError(fmt.Errorf("invalid base URL for chat completions: %w", err))
	}
	if resolved.AuthRequirement == provider.AuthReferenceRequired && resolver == nil {
		return nil, authUnavailableError(errors.New("endpoint requires authentication but no secret resolver is configured"))
	}
	client := &Client{
		resolved:    resolved,
		secret:      resolver,
		now:         options.Now,
		maxResponse: defaultResponseLimit,
	}
	if bound := resolved.Profile.MaxResponseBytes; bound > 0 {
		client.maxResponse = bound
	}
	if options.Now == nil {
		client.now = time.Now
	}
	// The adapter OWNS the complete dispatch stack. Every knob that could
	// amplify physical model-effect requests is pinned here:
	//   - CheckRedirect refuses every redirect, so a 3xx can never become a
	//     second request without new governor admission;
	//   - Jar is nil, so no cookie-driven replay;
	//   - Timeout bounds one attempt; there is no retry wrapper anywhere.
	// HTTP/2 is disabled EXPLICITLY and STRUCTURALLY: the stdlib h2 transport
	// contains an internal request-retry loop (h2_bundle.go, retry <= 6) that
	// re-emits replayable requests on GOAWAY/REFUSED_STREAM/protocol errors.
	// One governed Complete must never be able to produce multiple physical
	// transmissions, so this adapter speaks HTTP/1.1 only.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.Proxy = nil
	// Implicit proxy support is disabled fail-closed. The cloned DefaultTransport
	// inherits ProxyFromEnvironment, and HTTP_PROXY/HTTPS_PROXY could insert an
	// opaque infrastructure between Runstead and the configured endpoint (with
	// downstream retries, fallbacks or route changes) that this single-attempt
	// route cannot observe or account for. A baseline route that owns its
	// dispatch stack must never inherit transport behavior from the ambient
	// environment; explicit proxy support, if ever needed, is future
	// configuration plus safety evidence, not inherited environment.
	client.httpClient = &http.Client{
		Transport: transport,
		Jar:       nil,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, nil
}

// RouteSafety exposes the executable declaration this adapter was proven
// against. The governor compares it before admission.
func (c *Client) RouteSafety() provider.RouteSafety {
	if c == nil {
		return provider.RouteSafety{}
	}
	return provider.SafeRouteSafety()
}

// Complete performs exactly one governed logical completion as one physical
// HTTP request against the configured endpoint.
func (c *Client) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	notSent := func(kind ErrorKind, cause error) (provider.Response, error) {
		return provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryNotSent}}, &Error{Kind: kind, DeliveryState: provider.DeliveryNotSent, Cause: cause}
	}

	// Pre-dispatch refusals: provably zero model-effect requests.
	if err := ctx.Err(); err != nil {
		return notSent(ErrorCancelled, err)
	}
	if request.Model != "" && request.Model != c.resolved.Model {
		return notSent(ErrorConfigRefused, fmt.Errorf("request model %q differs from the resolved configured model", request.Model))
	}
	endpointURL, err := chatCompletionsURL(c.resolved.BaseURL)
	if err != nil {
		return notSent(ErrorConfigRefused, err)
	}

	var authorization string
	switch c.resolved.AuthRequirement {
	case provider.AuthNone:
		// No resolver consultation, no Authorization header.
	case provider.AuthReferenceRequired:
		secret, resolveErr := c.secret(ctx, c.resolved.Auth)
		if resolveErr != nil || strings.TrimSpace(secret) == "" {
			return notSent(ErrorAuthUnavailable, errors.New("authentication material could not be resolved from its non-secret reference"))
		}
		authorization = "Bearer " + secret
	default:
		return notSent(ErrorConfigRefused, fmt.Errorf("auth requirement %q is unknown", string(c.resolved.AuthRequirement)))
	}

	payload, err := encodeChatCompletionRequest(c.resolved.Model, request.Prompt)
	if err != nil {
		return notSent(ErrorConfigRefused, err)
	}
	if bound := c.resolved.Profile.MaxRequestBytes; bound > 0 && len(payload) > bound {
		return notSent(ErrorRequestTooLarge, fmt.Errorf("request body exceeds the configured bound of %d bytes", bound))
	}

	callCtx, cancel := context.WithTimeout(ctx, c.resolvedTimeout())
	defer cancel()
	observation := &deliveryObservation{}
	callCtx = httptrace.WithClientTrace(callCtx, observation.trace())

	httpReq, err := newModelEffectRequest(callCtx, endpointURL, payload)
	if err != nil {
		return notSent(ErrorConfigRefused, err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		httpReq.Header.Set("Authorization", authorization)
	}
	if request.ClientRequestID != "" {
		httpReq.Header.Set("X-Runstead-Client-Request-ID", sanitizeHeaderToken(request.ClientRequestID))
	}
	started := c.now()

	// From here on, delivery evidence comes only from observation; nothing may
	// claim DeliveryNotSent anymore.
	response, callErr := c.httpClient.Do(httpReq)
	if callErr != nil {
		state := observation.stateAfterError()
		metadata := provider.ResponseMetadata{Endpoint: logicalEndpoint(endpointURL), Model: c.resolved.Model, Duration: c.now().Sub(started), DeliveryState: state}
		return provider.Response{Metadata: metadata}, transportError(callErr, state)
	}
	if response == nil {
		state := observation.stateAfterError()
		metadata := provider.ResponseMetadata{Endpoint: logicalEndpoint(endpointURL), Model: c.resolved.Model, Duration: c.now().Sub(started), DeliveryState: state}
		return provider.Response{Metadata: metadata}, &Error{Kind: ErrorTransport, DeliveryState: state, UpstreamReached: true}
	}
	defer response.Body.Close()
	observation.markResponseStarted()
	metadata := c.responseMetadata(response, c.now().Sub(started), endpointURL)
	metadata.DeliveryState = provider.DeliveryResponseStarted

	// A 3xx is refused instead of followed: a second physical request can only
	// exist after new governor admission.
	if statusCode := response.StatusCode; statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest {
		locationHash := hashOpaque(response.Header.Get("Location"))
		body, _ := io.ReadAll(io.LimitReader(response.Body, int64(c.maxResponse)+1))
		readComplete := len(body) <= c.maxResponse
		metadata.DeliveryState = observation.stateAfterBody(readComplete)
		return provider.Response{Metadata: metadata}, unsafeRedirectError(metadata, statusCode, locationHash)
	}

	body, readErr := readBody(response, c.maxResponse)
	if readErr != nil {
		state := observation.stateAfterBody(false)
		metadata.DeliveryState = state
		kind := ErrorTransport
		if errors.Is(readErr, errResponseTooLarge) {
			kind = ErrorResponseTooLarge
		}
		return provider.Response{Metadata: metadata}, &Error{Kind: kind, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID, DeliveryState: state, UpstreamReached: true, Cause: readErr}
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		metadata.DeliveryState = provider.DeliveryCompleted
		return provider.Response{Metadata: metadata}, httpError(metadata, response.StatusCode)
	}

	text, parseErr := decodeChatCompletionResponse(body)
	if parseErr != nil {
		metadata.DeliveryState = provider.DeliveryCompleted
		var adapterErr *Error
		if errors.As(parseErr, &adapterErr) {
			adapterErr.StatusCode = response.StatusCode
			adapterErr.RequestID = metadata.RequestID
			adapterErr.DeliveryState = metadata.DeliveryState
			adapterErr.UpstreamReached = true
		}
		return provider.Response{Metadata: metadata}, parseErr
	}

	metadata.DeliveryState = provider.DeliveryCompleted
	return provider.Response{Text: text, Metadata: metadata}, nil
}

func (c *Client) resolvedTimeout() time.Duration {
	return defaultTimeout
}

func (c *Client) responseMetadata(response *http.Response, duration time.Duration, endpointURL string) provider.ResponseMetadata {
	return provider.ResponseMetadata{
		StatusCode: response.StatusCode,
		RequestID:  hashOpaque(response.Header.Get("X-Request-Id")),
		Duration:   duration,
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), c.now()),
		Endpoint:   logicalEndpoint(endpointURL),
		Model:      c.resolved.Model,
	}
}

// sanitizeHeaderToken keeps only conservative identifier characters so an
// attacker-controlled ClientRequestID cannot smuggle header content.
func sanitizeHeaderToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return hashOpaque(value)
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._:-", char) {
			return hashOpaque(value)
		}
	}
	return value
}

// nonReplayableReader hides the concrete *bytes.Reader type from the stdlib.
// http.NewRequestWithContext populates Request.GetBody only for recognized
// replayable types (*bytes.Reader, *strings.Reader, *bytes.Buffer); wrapped in
// this plain io.Reader, GetBody stays nil, so no layer below the governor can
// re-emit the model-effect request from buffered bytes.
type nonReplayableReader struct {
	io.Reader
}

// newModelEffectRequest builds the single POST request with a provably
// non-replayable body: Request.GetBody is always nil for this reader type.
func newModelEffectRequest(ctx context.Context, endpointURL string, payload []byte) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, &nonReplayableReader{Reader: bytes.NewReader(payload)})
}

// chatCompletionsURL derives the family endpoint from the validated base URL,
// preserving prefixes such as /v1 instead of rewriting them.
func chatCompletionsURL(rawBaseURL string) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", errors.New("base URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return "", errors.New("base URL scheme must be http or https")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/" + familyChatCompletions
	baseURL.RawPath = ""
	return strings.TrimRight(baseURL.String(), "/"), nil
}

func readBody(response *http.Response, limit int) ([]byte, error) {
	if response.Body == nil {
		return nil, errors.New("response body is nil")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errResponseTooLarge
	}
	return body, nil
}

// encodeChatCompletionRequest renders the minimal wire payload. The exact
// configured model travels with every request so a silently diverging model
// name is impossible.
func encodeChatCompletionRequest(model, prompt string) ([]byte, error) {
	payload, err := json.Marshal(chatCompletionRequest{
		Model:    model,
		Messages: []wireMessage{{Role: "user", Content: prompt}},
		Stream:   false,
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}
