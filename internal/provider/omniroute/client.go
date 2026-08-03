package omniroute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const (
	defaultTimeout       = 30 * time.Second
	defaultBodyLimit     = 1 << 20
	defaultChatEndpoint  = "chat/completions"
	resiliencePath       = "/api/resilience"
	rateLimitsPath       = "/api/rate-limits"
	noCacheHeader        = "X-OmniRoute-No-Cache"
	requestIDHeader      = "X-Request-Id"
	sessionIDHeader      = "X-OmniRoute-Session-Id"
	maxOpaqueHeaderBytes = 128
)

// Doer is the narrow HTTP seam used by the adapter tests and callers that
// need a custom transport.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	BaseURL           string
	ManagementBaseURL string
	APIKey            string `json:"-"`
	Model             string
	ChatEndpoint      string
	Timeout           time.Duration
	MaxRequestBytes   int
	MaxResponseBytes  int
	RouteSafety       provider.RouteSafety
}

func (c Config) String() string {
	return fmt.Sprintf("omniroute.Config{BaseURL:%q ManagementBaseURL:%q APIKey:<redacted> Model:%q ChatEndpoint:%q Timeout:%s MaxRequestBytes:%d MaxResponseBytes:%d RouteSafety:%#v}", c.BaseURL, c.ManagementBaseURL, c.Model, c.ChatEndpoint, c.Timeout, c.MaxRequestBytes, c.MaxResponseBytes, c.RouteSafety)
}

func (c Config) GoString() string { return c.String() }

type Options struct {
	HTTPClient Doer
	Transport  http.RoundTripper
	Now        func() time.Time
}

type Client struct {
	config     Config
	httpClient Doer
	now        func() time.Time

	mu       sync.RWMutex
	verified bool
}

var _ provider.Client = (*Client)(nil)
var _ provider.SafetyAware = (*Client)(nil)

func New(config Config, options Options) (*Client, error) {
	baseURL, err := normalizeBaseURL(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid OmniRoute base URL: %w", err)
	}
	managementURL := config.ManagementBaseURL
	if strings.TrimSpace(managementURL) == "" {
		managementURL = managementURLFromBase(baseURL)
	} else if managementURL, err = normalizeBaseURL(managementURL); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute management URL: %w", err)
	}

	config.BaseURL = baseURL
	config.ManagementBaseURL = managementURL
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	if config.APIKey == "" {
		return nil, errors.New("OmniRoute API key must not be empty")
	}
	if config.Model == "" {
		return nil, errors.New("OmniRoute model must not be empty")
	}
	if !routeModelSafe(config.Model) {
		return nil, unsafeError(errors.New("model route is not a single explicit target"))
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultBodyLimit
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultBodyLimit
	}
	if config.MaxRequestBytes < 0 || config.MaxResponseBytes < 0 {
		return nil, errors.New("OmniRoute body limits must not be negative")
	}
	if err := config.RouteSafety.Validate(); err != nil {
		return nil, unsafeError(err)
	}
	if _, err := chatURL(config.BaseURL, config.ChatEndpoint); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute chat endpoint: %w", err)
	}
	if _, err := joinURL(config.ManagementBaseURL, resiliencePath); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute management endpoint: %w", err)
	}

	client := options.HTTPClient
	if client == nil {
		httpClient := &http.Client{Transport: options.Transport}
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = httpClient
	} else if httpClient, ok := client.(*http.Client); ok {
		// A redirect can replay a POST. Clone injected clients so the adapter
		// retains its one-attempt contract.
		clone := *httpClient
		clone.Jar = nil
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &clone
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Client{config: config, httpClient: client, now: now}, nil
}

func (c *Client) RouteSafety() provider.RouteSafety {
	if c == nil {
		return provider.RouteSafety{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.verified {
		return provider.RouteSafety{}
	}
	return c.config.RouteSafety
}

func (c *Client) Preflight(ctx context.Context) error {
	if c == nil {
		return unsafeError(nil)
	}
	c.mu.Lock()
	c.verified = false
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contextError(err, false)
	}
	requestURL, err := joinURL(c.config.ManagementBaseURL, resiliencePath)
	if err != nil {
		return unsafeError(err)
	}
	preflightCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(preflightCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return unsafeError(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	started := c.now()
	response, callErr := c.httpClient.Do(req)
	if callErr != nil {
		return unsafeError(transportError(callErr, false))
	}
	if response == nil {
		return unsafeError(errors.New("empty preflight response"))
	}
	body, readErr := readBody(response, c.config.MaxResponseBytes)
	metadata := responseMetadata(response, c.now().Sub(started), resiliencePath, c.config.Model, c.now())
	if readErr != nil {
		if errors.Is(readErr, errResponseTooLarge) {
			return unsafeError(&Error{Kind: ErrorResponseTooLarge, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID})
		}
		return unsafeError(readErr)
	}
	if metadata.StatusCode < http.StatusOK || metadata.StatusCode >= http.StatusMultipleChoices {
		return unsafeError(httpError(metadata, body))
	}
	if !safeResilience(body) {
		return unsafeError(errors.New("OmniRoute resilience settings are missing or unsafe"))
	}
	c.mu.Lock()
	c.verified = true
	c.mu.Unlock()
	return nil
}

func (c *Client) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	if c == nil || !c.isVerified() {
		return provider.Response{}, unsafeError(nil)
	}
	if err := ctx.Err(); err != nil {
		return provider.Response{}, contextError(err, false)
	}
	body, err := json.Marshal(struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}{
		Model: c.config.Model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: request.Prompt}},
		Stream: false,
	})
	if err != nil {
		return provider.Response{}, &Error{Kind: ErrorTransport, Cause: err}
	}
	if len(body) > c.config.MaxRequestBytes {
		return provider.Response{}, &Error{Kind: ErrorRequestTooLarge}
	}
	requestURL, err := chatURL(c.config.BaseURL, c.config.ChatEndpoint)
	if err != nil {
		return provider.Response{}, unsafeError(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, requestURL, strings.NewReader(string(body)))
	if err != nil {
		return provider.Response{}, &Error{Kind: ErrorTransport, Cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set(noCacheHeader, "true")
	started := c.now()
	response, callErr := c.httpClient.Do(req)
	if callErr != nil {
		return provider.Response{Metadata: provider.ResponseMetadata{Endpoint: logicalEndpoint(requestURL), Model: c.config.Model, Duration: c.now().Sub(started)}}, transportError(callErr, true)
	}
	if response == nil {
		return provider.Response{Metadata: provider.ResponseMetadata{Endpoint: logicalEndpoint(requestURL), Model: c.config.Model, Duration: c.now().Sub(started)}}, &Error{Kind: ErrorTransport, UpstreamReached: true}
	}
	metadata := responseMetadata(response, c.now().Sub(started), requestURL, c.config.Model, c.now())
	responseBody, readErr := readBody(response, c.config.MaxResponseBytes)
	result := provider.Response{Metadata: metadata}
	if readErr != nil {
		if errors.Is(readErr, errResponseTooLarge) {
			return result, &Error{Kind: ErrorResponseTooLarge, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID, UpstreamReached: true}
		}
		return result, &Error{Kind: ErrorTransport, StatusCode: metadata.StatusCode, RequestID: metadata.RequestID, UpstreamReached: true}
	}
	if metadata.StatusCode < http.StatusOK || metadata.StatusCode >= http.StatusMultipleChoices {
		return result, httpError(metadata, responseBody)
	}
	text, parseErr := responseText(responseBody)
	if parseErr != nil {
		parseErr.StatusCode = metadata.StatusCode
		parseErr.RequestID = metadata.RequestID
		parseErr.UpstreamReached = true
		return result, parseErr
	}
	result.Text = text
	return result, nil
}

func (c *Client) isVerified() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.verified
}

var errResponseTooLarge = errors.New("response body exceeds configured limit")

func readBody(response *http.Response, limit int) ([]byte, error) {
	if response.Body == nil {
		return nil, errors.New("response body is nil")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errResponseTooLarge
	}
	return body, nil
}

type completionEnvelope struct {
	Choices []struct {
		Message *struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func responseText(body []byte) (string, *Error) {
	if len(body) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var envelope completionEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", &Error{Kind: ErrorMalformedJSON}
	}
	if len(envelope.Choices) == 0 || envelope.Choices[0].Message == nil || envelope.Choices[0].Message.Content == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	text := *envelope.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	return text, nil
}

func responseMetadata(response *http.Response, duration time.Duration, endpoint, model string, now time.Time) provider.ResponseMetadata {
	return provider.ResponseMetadata{
		StatusCode: response.StatusCode,
		RequestID:  sanitizeOpaque(response.Header.Get(requestIDHeader)),
		SessionID:  hashOpaque(response.Header.Get(sessionIDHeader)),
		Duration:   duration,
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), now),
		ResetAt:    parseResetAt(response.Header.Get("X-RateLimit-Reset")),
		Endpoint:   logicalEndpoint(endpoint),
		Model:      model,
	}
}

func contextError(err error, reached bool) *Error {
	kind := ErrorCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorTimeout
	}
	return &Error{Kind: kind, UpstreamReached: reached, Cause: err}
}

func transportError(err error, reached bool) *Error {
	if errors.Is(err, context.Canceled) {
		return contextError(err, reached)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return contextError(err, reached)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: ErrorTimeout, UpstreamReached: reached, Cause: context.DeadlineExceeded}
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "connection reset") {
		return &Error{Kind: ErrorConnectionReset, UpstreamReached: reached}
	}
	return &Error{Kind: ErrorTransport, UpstreamReached: reached}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64((24*time.Hour)/time.Second) {
			return 24 * time.Hour
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func parseResetAt(value string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func sanitizeOpaque(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= maxOpaqueHeaderBytes {
		for _, char := range value {
			if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._:-", char) {
				return hashOpaque(value)
			}
		}
		return value
	}
	return hashOpaque(value)
}

func hashOpaque(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:8])
}

func logicalEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "omniroute"
	}
	path := parsed.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must be an absolute URL without credentials, query or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("scheme must be http or https")
	}
	return raw, nil
}

func managementURLFromBase(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/v1")
	}
	return strings.TrimRight(parsed.String(), "/")
}

func chatURL(base, endpoint string) (string, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultChatEndpoint
	}
	return joinURL(base, endpoint)
}

func joinURL(base, endpoint string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", errors.New("base URL is invalid")
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil || endpointURL.User != nil || endpointURL.RawQuery != "" || endpointURL.Fragment != "" {
		return "", errors.New("endpoint must not contain credentials, query or fragment")
	}
	if endpointURL.IsAbs() {
		if endpointURL.Scheme != baseURL.Scheme || endpointURL.Host != baseURL.Host {
			return "", errors.New("endpoint host must match configured base URL")
		}
		return strings.TrimRight(endpointURL.String(), "/"), nil
	}
	path := endpointURL.Path
	if strings.HasPrefix(path, "/") {
		baseURL.Path = path
	} else {
		baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/" + path
	}
	baseURL.RawPath = ""
	return strings.TrimRight(baseURL.String(), "/"), nil
}
