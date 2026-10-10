// Package responsessiwc implements the direct, single-attempt HTTP/SSE wire
// contract for the Runstead SIWC Responses lane.
package responsessiwc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/siwcauth"
)

const (
	endpointPath = "/v1/responses"
	transportID  = "responses-siwc-sse"
	timeout      = 90 * time.Second
	maxLine      = 64 << 10
	maxEvent     = 256 << 10
	maxOutput    = 4 << 20
	maxStream    = 8 << 20
)

type ErrorKind string

const (
	ErrorConfigRefused         ErrorKind = "config_refused"
	ErrorAuthUnavailable       ErrorKind = "auth_unavailable"
	ErrorAuthenticationDenied  ErrorKind = "authentication_denied"
	ErrorPermissionDenied      ErrorKind = "permission_denied"
	ErrorRateCapacity          ErrorKind = "rate_or_capacity"
	ErrorUpstreamServerFailure ErrorKind = "upstream_server_failure"
	ErrorCancelled             ErrorKind = "cancelled"
	ErrorTimeout               ErrorKind = "timeout"
	ErrorTransport             ErrorKind = "transport"
	ErrorMalformedResponse     ErrorKind = "malformed_response"
	ErrorInvalidEnvelope       ErrorKind = "invalid_envelope"
	ErrorResponseTooLarge      ErrorKind = "response_too_large"
	ErrorRequestTooLarge       ErrorKind = "request_too_large"
	ErrorUnsafeRedirect        ErrorKind = "unsafe_redirect"
	ErrorStreamIncomplete      ErrorKind = "stream_incomplete"
	ErrorResponseFailed        ErrorKind = "response_failed"
	ErrorResponseIncomplete    ErrorKind = "response_incomplete"
)

// Error contains only bounded, sanitized observations. Code is copied only
// from the documented machine-readable SIWC error vocabulary.
type Error struct {
	Kind          ErrorKind
	Code          string
	StatusCode    int
	RequestID     string
	DeliveryState provider.DeliveryState
	Cause         error
}

func (e *Error) Error() string {
	if e == nil {
		return "responses_siwc error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("responses_siwc %s (status %d)", e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("responses_siwc %s", e.Kind)
}
func (e *Error) GoString() string { return e.Error() }
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AccessTokenSource is the one-request custody boundary used by the adapter.
// Production construction uses the protected SIWC store; the interface is
// exposed only to support internal loopback transport tests.
type AccessTokenSource interface {
	WithAccessToken(ctx context.Context, clientID, model, accountBinding, credentialBinding string, request func(string) error) error
}

type options struct {
	baseURL   string
	transport http.RoundTripper
	access    AccessTokenSource
	now       func() time.Time
}

type Client struct {
	resolved    provider.Resolved
	client      *http.Client
	access      AccessTokenSource
	now         func() time.Time
	endpoint    string
	maxRequest  int
	maxResponse int
}

var _ provider.Client = (*Client)(nil)
var _ provider.SafetyAware = (*Client)(nil)

// New constructs a production adapter. The SIWC auth reference is resolved
// only through protected credential custody, never as an environment name.
func New(resolved provider.Resolved) (*Client, error) {
	store, err := openCustody(resolved)
	if err != nil {
		return nil, refused("SIWC credential store is unavailable")
	}
	return newClient(resolved, options{access: store})
}

func openCustody(resolved provider.Resolved) (*siwcauth.Store, error) {
	if resolved.AuthRequirement != provider.AuthReferenceRequired || resolved.Auth == "" {
		return nil, errors.New("issued client credential reference required")
	}
	clientID, ok := strings.CutPrefix(string(resolved.Auth), "siwc:")
	if !ok || strings.TrimSpace(clientID) == "" || strings.ContainsAny(clientID, "/\\\x00\r\n") {
		return nil, errors.New("invalid SIWC credential reference")
	}
	home, err := osUserHomeDir()
	if err != nil {
		return nil, err
	}
	path, err := siwcauth.DefaultStorePath(home, os.Getenv("XDG_CONFIG_HOME"))
	if err != nil {
		return nil, err
	}
	return siwcauth.OpenStore(path, false)
}

// NewLoopbackForTest creates a client with an internal loopback HTTP endpoint
// backed by the real protected SIWC store. Production construction never calls
// this path, and responsesURL refuses non-loopback hosts or HTTPS bypasses.
func NewLoopbackForTest(resolved provider.Resolved, baseURL string) (*Client, error) {
	if _, err := responsesURL(baseURL, true); err != nil {
		return nil, refused("test endpoint must be loopback HTTP")
	}
	store, err := openCustody(resolved)
	if err != nil {
		return nil, refused("SIWC credential store is unavailable")
	}
	return newClient(resolved, options{baseURL: baseURL, access: store})
}

func osUserHomeDir() (string, error) { return os.UserHomeDir() }

func newClient(resolved provider.Resolved, opts options) (*Client, error) {
	if resolved.DocumentVersion != 2 || resolved.WireContract != provider.WireResponsesSIWCV1 || resolved.ProtocolFamily != provider.FamilyOpenAICompatible || resolved.ProviderID == "" || resolved.ConfigIdentity == "" || resolved.Model == "" {
		return nil, refused("resolved provider is not responses_siwc_v1")
	}
	if resolved.Profile.ProfileVersion != "v1" || !resolved.Profile.Capabilities[provider.CapabilityTextTurn] || !resolved.Profile.Capabilities[provider.CapabilityRunsteadProtocol] {
		return nil, refused("SIWC requires the validated text-turn and Runstead protocol capabilities")
	}
	if resolved.BaseURL != "https://api.openai.com/v1" {
		return nil, refused("SIWC production endpoint is fixed")
	}
	if err := resolved.Profile.RouteSafety.Validate(); err != nil || !resolved.Profile.RouteSafety.Equal(provider.SafeRouteSafety()) {
		return nil, refused("SIWC requires the proven single-attempt route")
	}
	clientID, hasSIWCRef := strings.CutPrefix(string(resolved.Auth), "siwc:")
	if !hasSIWCRef || strings.TrimSpace(clientID) == "" || strings.ContainsAny(clientID, "/\\\x00\r\n") ||
		!validBinding(resolved.AccountBinding) || !validBinding(resolved.CredentialBinding) || resolved.AccountBinding == resolved.CredentialBinding {
		return nil, refused("SIWC credential and identity bindings are required")
	}
	base := "https://api.openai.com/v1"
	if opts.baseURL != "" {
		base = opts.baseURL
	}
	endpoint, err := responsesURL(base, opts.baseURL != "")
	if err != nil {
		return nil, refused("SIWC endpoint is invalid")
	}
	if opts.access == nil {
		return nil, refused("SIWC credential custody is unavailable")
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	tr := opts.transport
	if tr == nil {
		owned := http.DefaultTransport.(*http.Transport).Clone()
		owned.Proxy = nil
		owned.DisableKeepAlives = true
		owned.ForceAttemptHTTP2 = false
		if owned.TLSClientConfig == nil {
			owned.TLSClientConfig = &tls.Config{}
		} else {
			owned.TLSClientConfig = owned.TLSClientConfig.Clone()
		}
		owned.TLSClientConfig.NextProtos = []string{"http/1.1"}
		tr = owned
	}
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	maxReq, maxResp := resolved.Profile.MaxRequestBytes, resolved.Profile.MaxResponseBytes
	if maxReq <= 0 || maxResp <= 0 {
		return nil, refused("SIWC request and response limits must be explicit")
	}
	if maxResp > maxOutput {
		maxResp = maxOutput
	}
	return &Client{resolved: resolved, client: client, access: opts.access, now: opts.now, endpoint: endpoint, maxRequest: maxReq, maxResponse: maxResp}, nil
}

func (c *Client) RouteSafety() provider.RouteSafety {
	if c == nil {
		return provider.RouteSafety{}
	}
	return provider.SafeRouteSafety()
}

func (c *Client) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	meta := provider.ResponseMetadata{AdapterVersion: provider.CompatAdapterVersion, Transport: transportID, Endpoint: endpointPath, Model: c.resolved.Model, DeliveryState: provider.DeliveryNotSent}
	fail := func(kind ErrorKind, cause error) (provider.Response, error) {
		return provider.Response{Metadata: meta}, &Error{Kind: kind, Cause: cause, DeliveryState: provider.DeliveryNotSent}
	}
	if err := ctx.Err(); err != nil {
		return fail(ErrorCancelled, err)
	}
	if request.Model != "" && request.Model != c.resolved.Model {
		return fail(ErrorConfigRefused, errors.New("request model differs from selected model"))
	}
	if len(request.Prompt) > c.maxRequest {
		return fail(ErrorRequestTooLarge, errors.New("request exceeds configured bound"))
	}
	payload, _, err := encodeRequest(c.resolved.Model, request.Prompt)
	if err != nil {
		return fail(ErrorInvalidEnvelope, err)
	}
	if len(payload) > c.maxRequest {
		return fail(ErrorRequestTooLarge, errors.New("request exceeds configured bound"))
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := c.now()
	var result provider.Response
	var resultErr error
	err = c.access.WithAccessToken(callCtx, strings.TrimPrefix(string(c.resolved.Auth), "siwc:"), c.resolved.Model, c.resolved.AccountBinding, c.resolved.CredentialBinding, func(token string) error {
		if err := callCtx.Err(); err != nil {
			resultErr = contextFailure(err, provider.DeliveryNotSent)
			return resultErr
		}
		req, err := newRequest(callCtx, c.endpoint, payload)
		if err != nil {
			resultErr = &Error{Kind: ErrorConfigRefused, DeliveryState: provider.DeliveryNotSent}
			return resultErr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		if request.ClientRequestID != "" {
			req.Header.Set("X-Runstead-Client-Request-ID", safeHeader(request.ClientRequestID))
		}
		observation := &delivery{}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), observation.trace(c.now)))
		resp, doErr := c.client.Do(req)
		if doErr != nil {
			state := observation.state()
			result.Metadata = meta
			result.Metadata.DeliveryState = state
			result.Metadata.Duration = c.now().Sub(started)
			resultErr = transportFailure(doErr, state)
			return resultErr
		}
		if resp == nil {
			result.Metadata = meta
			result.Metadata.DeliveryState = observation.state()
			resultErr = &Error{Kind: ErrorTransport, DeliveryState: result.Metadata.DeliveryState}
			return resultErr
		}
		defer resp.Body.Close()
		result.Metadata = meta
		result.Metadata.StatusCode = resp.StatusCode
		result.Metadata.RequestID = validRequestID(resp.Header.Get("X-Request-Id"))
		result.Metadata.DeliveryState = provider.DeliveryResponseStarted
		result.Metadata.Duration = c.now().Sub(started)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resultErr = &Error{Kind: ErrorUnsafeRedirect, StatusCode: resp.StatusCode, RequestID: result.Metadata.RequestID, DeliveryState: result.Metadata.DeliveryState}
			return resultErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			result.Metadata.DeliveryState = provider.DeliveryCompleted
			resultErr = httpFailure(resp, result.Metadata)
			return resultErr
		}
		contentType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if mediaErr != nil || !strings.EqualFold(contentType, "text/event-stream") {
			result.Metadata.DeliveryState = provider.DeliveryCompleted
			resultErr = &Error{Kind: ErrorInvalidEnvelope, StatusCode: resp.StatusCode, RequestID: result.Metadata.RequestID, DeliveryState: result.Metadata.DeliveryState}
			return resultErr
		}
		text, parseErr := parseSSE(resp.Body, c.maxResponse)
		result.Metadata.Duration = c.now().Sub(started)
		result.Metadata.DeliveryState = observation.stateAfterBody(parseErr == nil)
		if parseErr != nil {
			if contextErr := callCtx.Err(); contextErr != nil {
				resultErr = contextFailure(contextErr, result.Metadata.DeliveryState)
				if typed, ok := resultErr.(*Error); ok {
					typed.StatusCode = resp.StatusCode
					typed.RequestID = result.Metadata.RequestID
				}
				return resultErr
			}
			var streamErr *Error
			if errors.As(parseErr, &streamErr) {
				if streamErr.Kind == ErrorResponseFailed || streamErr.Kind == ErrorResponseIncomplete {
					result.Metadata.DeliveryState = provider.DeliveryCompleted
				}
				streamErr.StatusCode = resp.StatusCode
				streamErr.RequestID = result.Metadata.RequestID
				streamErr.DeliveryState = result.Metadata.DeliveryState
				resultErr = streamErr
			} else {
				resultErr = &Error{Kind: ErrorMalformedResponse, StatusCode: resp.StatusCode, RequestID: result.Metadata.RequestID, DeliveryState: result.Metadata.DeliveryState}
			}
			return resultErr
		}
		result.Text = text
		result.Metadata.DeliveryState = provider.DeliveryCompleted
		return nil
	})
	if err != nil && resultErr == nil {
		result.Metadata = meta
		result.Metadata.Duration = c.now().Sub(started)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			resultErr = contextFailure(err, provider.DeliveryNotSent)
		} else {
			// Custody errors intentionally collapse to a fixed sanitized outcome;
			// their text and any file-system details never cross this boundary.
			resultErr = &Error{Kind: ErrorAuthUnavailable, DeliveryState: provider.DeliveryNotSent}
		}
	}
	return result, resultErr
}

func refused(message string) error {
	return &Error{Kind: ErrorConfigRefused, Cause: errors.New(message), DeliveryState: provider.DeliveryNotSent}
}

func validBinding(value string) bool {
	const prefix = "hmac-sha256:v1:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, r := range value[len(prefix):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func responsesURL(raw string, allowLoopback bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid endpoint")
	}
	if allowLoopback {
		if u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" && u.Hostname() != "localhost") {
			return "", errors.New("test endpoint must be loopback HTTP")
		}
	} else if u.String() != "https://api.openai.com/v1" {
		return "", errors.New("production endpoint is fixed")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/responses"
	u.RawPath = ""
	return u.String(), nil
}

type wireRequest struct {
	Model        string      `json:"model"`
	Instructions string      `json:"instructions,omitempty"`
	Input        []wireInput `json:"input"`
	Store        bool        `json:"store"`
	Stream       bool        `json:"stream"`
}
type wireInput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func encodeRequest(model, prompt string) ([]byte, string, error) {
	instructions, body, err := splitTranscript(prompt)
	if err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(wireRequest{Model: model, Instructions: instructions, Input: []wireInput{{Role: "user", Content: body}}, Store: false, Stream: true})
	return payload, instructions, err
}

// The transcript remains locally rendered by the agent. Its first system
// section moves to Responses instructions; the remaining tagged history is
// sent as self-contained user input, avoiding unsupported system input items.
func splitTranscript(prompt string) (string, string, error) {
	prefix := "=== runstead:system ===\n"
	if !strings.HasPrefix(prompt, prefix) {
		return "", "", errors.New("Runstead transcript has no leading system section")
	}
	end := strings.Index(prompt[len(prefix):], "\n=== runstead:")
	if end < 0 {
		return "", "", errors.New("Runstead transcript has no model input section")
	}
	cut := len(prefix) + end + 1
	return strings.TrimSpace(prompt[len(prefix) : cut-1]), prompt[cut:], nil
}

func newRequest(ctx context.Context, endpoint string, payload []byte) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &nonReplayableBody{Reader: bytes.NewReader(payload)})
}

type nonReplayableBody struct{ io.Reader }

func safeHeader(s string) string {
	if len(s) > 128 {
		return hash(s)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("._:-", r) {
			return hash(s)
		}
	}
	return s
}
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:8])
}
func validRequestID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("._:-", r) {
			return ""
		}
	}
	return s
}

func contextFailure(err error, state provider.DeliveryState) error {
	kind := ErrorCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorTimeout
	}
	return &Error{Kind: kind, DeliveryState: state, Cause: err}
}
func transportFailure(err error, state provider.DeliveryState) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextFailure(err, state)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: ErrorTimeout, DeliveryState: state}
	}
	return &Error{Kind: ErrorTransport, DeliveryState: state}
}

func httpFailure(resp *http.Response, meta provider.ResponseMetadata) error {
	kind := ErrorInvalidEnvelope
	switch resp.StatusCode {
	case 401:
		kind = ErrorAuthenticationDenied
	case 403:
		kind = ErrorPermissionDenied
	case 429:
		kind = ErrorRateCapacity
	default:
		if resp.StatusCode >= 500 {
			kind = ErrorUpstreamServerFailure
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	code := ""
	if err == nil {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &envelope) == nil && knownCode(envelope.Error.Code) {
			code = envelope.Error.Code
		}
	}
	return &Error{Kind: kind, Code: code, StatusCode: resp.StatusCode, RequestID: meta.RequestID, DeliveryState: meta.DeliveryState}
}

func knownCode(code string) bool {
	switch code {
	case "subscription_sharing_user_not_eligible", "subscription_sharing_usage_limit_exceeded", "subscription_sharing_usage_unavailable", "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported", "subscription_sharing_invalid_user", "subscription_sharing_user_unavailable", "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context":
		return true
	}
	return false
}
