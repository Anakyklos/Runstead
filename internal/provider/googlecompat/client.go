package googlecompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const (
	defaultTimeout       = 60 * time.Second
	defaultResponseLimit = 8 << 20
	headerAPIKey         = "x-goog-api-key"
	headerClientRequest  = "X-Runstead-Client-Request-ID"
	headerRequestID      = "x-request-id"
	actionSuffix         = ":generateContent"
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
// google_compatible family. One Complete call performs exactly one physical
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

// New builds a Google/Gemini-compatible protocol adapter around a resolved
// provider configuration. Every check here re-verifies, defensively, what #79
// resolution already proved: wrong family, unsafe route safety, unsupported
// protocol options or missing authentication plumbing fail closed before any
// dispatch is possible.
func New(resolved provider.Resolved, resolver SecretResolver, options Options) (*Client, error) {
	if resolved.ProviderID == "" || resolved.ConfigIdentity == "" {
		return nil, configRefusedError(errors.New("adapter requires a fully resolved provider configuration"))
	}
	if resolved.ProtocolFamily != provider.FamilyGoogleCompatible {
		return nil, configRefusedError(fmt.Errorf("protocol family %q is not %q", string(resolved.ProtocolFamily), string(provider.FamilyGoogleCompatible)))
	}
	if err := resolved.Profile.RouteSafety.Validate(); err != nil {
		return nil, configRefusedError(err)
	}
	if !resolved.Profile.RouteSafety.Equal(provider.SafeRouteSafety()) {
		return nil, configRefusedError(errors.New("this initial adapter implements only the safe single-attempt route; receipt accounting would be unproven pretense"))
	}
	if _, err := generateContentURL(resolved.BaseURL, resolved.Model); err != nil {
		return nil, configRefusedError(fmt.Errorf("invalid generateContent endpoint: %w", err))
	}
	// The google_compatible baseline carries the prompt through
	// contents[].parts[].text and the exact model through the URL resource
	// path, so provider.Request already carries everything the minimal
	// generateContent wire needs. The protocol-option vocabulary is therefore
	// EMPTY: any configured option is unknown and refuses the adapter before
	// any dispatch. No silent defaults exist.
	if err := resolveProtocolOptions(resolved.Options); err != nil {
		return nil, configRefusedError(err)
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
	// Implicit proxy support is disabled fail-closed. The cloned
	// DefaultTransport inherits ProxyFromEnvironment, and HTTP_PROXY/
	// HTTPS_PROXY could insert an opaque infrastructure between Runstead and
	// the configured endpoint (with downstream retries, fallbacks or route
	// changes) that this single-attempt route cannot observe or account for.
	// A baseline route that owns its dispatch stack must never inherit
	// transport behavior from the ambient environment; explicit proxy
	// support, if ever needed, is future configuration plus safety evidence,
	// not inherited environment.
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
	endpointURL, err := generateContentURL(c.resolved.BaseURL, c.resolved.Model)
	if err != nil {
		return notSent(ErrorConfigRefused, err)
	}

	var apiKey string
	switch c.resolved.AuthRequirement {
	case provider.AuthNone:
		// No resolver consultation, no api key header.
	case provider.AuthReferenceRequired:
		secret, resolveErr := c.secret(ctx, c.resolved.Auth)
		if resolveErr != nil || strings.TrimSpace(secret) == "" {
			return notSent(ErrorAuthUnavailable, errors.New("authentication material could not be resolved from its non-secret reference"))
		}
		apiKey = secret
	default:
		return notSent(ErrorConfigRefused, fmt.Errorf("auth requirement %q is unknown", string(c.resolved.AuthRequirement)))
	}

	payload, err := encodeGenerateRequest(request.Prompt)
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
	if apiKey != "" {
		httpReq.Header.Set(headerAPIKey, apiKey)
	}
	if request.ClientRequestID != "" {
		httpReq.Header.Set(headerClientRequest, sanitizeHeaderToken(request.ClientRequestID))
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
	// exist after new governor admission. Delivery evidence stays honest: the
	// body counts as fully read ONLY when the read completed without error AND
	// the configured size bound was not exceeded. A read error, premature EOF
	// or any other uncertainty after the response started preserves a
	// conservative state (response_started), never completed (#97 review).
	if statusCode := response.StatusCode; statusCode >= http.StatusMultipleChoices && statusCode < http.StatusBadRequest {
		locationHash := hashOpaque(response.Header.Get("Location"))
		body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(c.maxResponse)+1))
		readComplete := readErr == nil && len(body) <= c.maxResponse
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

	text, parseErr := decodeGenerateResponse(body)
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
		RequestID:  hashOpaque(response.Header.Get(headerRequestID)),
		Duration:   duration,
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), c.now()),
		Endpoint:   logicalEndpoint(endpointURL),
		Model:      c.resolved.Model,
	}
}

// resolveProtocolOptions enforces the closed protocol-option vocabulary of the
// google_compatible baseline: the minimal generateContent wire carries the
// prompt through contents[].parts[].text and the exact model through the URL
// resource path, so provider.Request already carries everything the baseline
// needs. No protocol option is supported; any configured option is unknown and
// refuses construction (zero requests) instead of being silently ignored.
func resolveProtocolOptions(options map[string]string) error {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fmt.Errorf("unsupported protocol option(s) %v: the google_compatible baseline implements an empty option vocabulary (the prompt travels via contents[].parts[].text and the exact model via the URL path)", keys)
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

// generateContentURL derives the family endpoint from the validated base URL.
// The generateContent family defines POST models/{exact-model}:generateContent
// as its canonical resource path, so the configured base URL is the API root
// and the family path is appended, preserving any operator prefix (for example
// /v1beta or /google behind a gateway). The exact configured model is escaped
// per path segment: model resource names that contain slashes (for example
// publishers/google/models/...) keep their structure, and no component can be
// concatenated unsafely. Dot segments are refused so the models/ prefix can
// never be escaped through path traversal.
func generateContentURL(rawBaseURL, model string) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", errors.New("base URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return "", errors.New("base URL scheme must be http or https")
	}
	rawModel := strings.TrimSpace(model)
	if rawModel == "" {
		return "", errors.New("model name must not be empty")
	}
	segments := strings.Split(rawModel, "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("model name must not contain empty or dot path segments")
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/models/" + strings.Join(escaped, "/") + actionSuffix
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
