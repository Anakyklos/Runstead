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
	defaultTimeout              = 30 * time.Second
	defaultBodyLimit            = 1 << 20
	defaultChatEndpoint         = "chat/completions"
	resiliencePath              = "/api/resilience"
	rateLimitsPath              = "/api/rate-limits"
	settingsPath                = "/api/settings"
	modelAliasesPath            = "/api/models/alias"
	settingsModelAliasesPath    = "/api/settings/model-aliases"
	fallbackChainsPath          = "/api/fallback/chains"
	combosPath                  = "/api/combos"
	modelComboMappingsPath      = "/api/model-combo-mappings"
	providersPath               = "/api/providers"
	noCacheHeader               = "X-OmniRoute-No-Cache"
	requestIDHeader             = "X-Request-Id"
	clientRequestIDHeader       = "X-Runstead-Client-Request-Id"
	attemptReceiptRequestHeader = "X-Runstead-Attempt-Receipts"
	attemptReceiptHeader        = "X-OmniRoute-Attempt-Receipts"
	sessionIDHeader             = "X-OmniRoute-Session-Id"
	maxOpaqueHeaderBytes        = 128
)

// Doer is retained as a narrow HTTP seam, but New accepts only *http.Client
// values backed by the standard *http.Transport so the adapter can enforce
// redirect and one-attempt behavior.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	BaseURL               string
	ManagementBaseURL     string
	APIKey                string `json:"-"`
	Model                 string
	Provider              string
	AccountLaneHash       string
	EnableAttemptReceipts bool
	ChatEndpoint          string
	Timeout               time.Duration
	MaxRequestBytes       int
	MaxResponseBytes      int
	RouteSafety           provider.RouteSafety
}

func (c Config) String() string {
	return fmt.Sprintf("omniroute.Config{BaseURL:%q ManagementBaseURL:%q APIKey:<redacted> Model:%q Provider:%q AccountLaneHash:%q EnableAttemptReceipts:%t ChatEndpoint:%q Timeout:%s MaxRequestBytes:%d MaxResponseBytes:%d RouteSafety:%#v}", c.BaseURL, c.ManagementBaseURL, c.Model, c.Provider, c.AccountLaneHash, c.EnableAttemptReceipts, c.ChatEndpoint, c.Timeout, c.MaxRequestBytes, c.MaxResponseBytes, c.RouteSafety)
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
	config.Provider = strings.TrimSpace(config.Provider)
	config.AccountLaneHash = strings.TrimSpace(config.AccountLaneHash)
	if config.APIKey == "" {
		return nil, errors.New("OmniRoute API key must not be empty")
	}
	if config.Model == "" {
		return nil, errors.New("OmniRoute model must not be empty")
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
	if config.EnableAttemptReceipts && config.RouteSafety.AttemptAccounting != provider.AttemptAccountingReceipts {
		return nil, unsafeError(errors.New("attempt receipts require receipt-aware route safety"))
	}
	if config.EnableAttemptReceipts && config.Provider == "" {
		return nil, unsafeError(errors.New("attempt receipts require a provider identity"))
	}
	if config.EnableAttemptReceipts && config.AccountLaneHash == "" {
		return nil, unsafeError(errors.New("attempt receipts require an account lane hash"))
	}
	if _, err := chatURL(config.BaseURL, config.ChatEndpoint); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute chat endpoint: %w", err)
	}
	if _, err := joinURL(config.ManagementBaseURL, resiliencePath); err != nil {
		return nil, fmt.Errorf("invalid OmniRoute management endpoint: %w", err)
	}
	for _, endpoint := range []string{settingsPath, modelAliasesPath, settingsModelAliasesPath, fallbackChainsPath, combosPath, modelComboMappingsPath, providersPath} {
		if _, err := joinURL(config.ManagementBaseURL, endpoint); err != nil {
			return nil, fmt.Errorf("invalid OmniRoute management endpoint: %w", err)
		}
	}

	client := options.HTTPClient
	if client == nil {
		if !safeTransport(options.Transport) {
			return nil, unsafeError(errors.New("custom OmniRoute transport cannot be constrained"))
		}
		httpClient := &http.Client{Transport: options.Transport}
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = httpClient
	} else if httpClient, ok := client.(*http.Client); ok {
		if !safeTransport(httpClient.Transport) {
			return nil, unsafeError(errors.New("custom OmniRoute HTTP client cannot be constrained"))
		}
		// A redirect can replay a POST. Clone injected clients so the adapter
		// retains its one-attempt contract.
		clone := *httpClient
		clone.Jar = nil
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &clone
	} else {
		return nil, unsafeError(errors.New("opaque OmniRoute HTTP client cannot be constrained"))
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
	if !c.verified && !c.config.EnableAttemptReceipts {
		return provider.RouteSafety{}
	}
	return c.config.RouteSafety
}

func (c *Client) AttemptReceiptsEnabled() bool {
	return c != nil && c.config.EnableAttemptReceipts
}

// Preflight validates observable management settings for diagnostics, but it
// never authorizes protected execution. Until authoritative attempt receipts
// are available, it returns ErrUnsafeRoute even when those settings are safe.
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
	evidence := make(map[string][]byte, 1+7)
	for _, endpoint := range []string{resiliencePath, settingsPath, modelAliasesPath, settingsModelAliasesPath, fallbackChainsPath, combosPath, modelComboMappingsPath, providersPath} {
		body, err := c.managementEvidence(ctx, endpoint)
		if err != nil {
			return err
		}
		evidence[endpoint] = body
	}
	if !safeResilience(evidence[resiliencePath]) {
		return unsafeError(errors.New("OmniRoute resilience evidence is missing or unsafe"))
	}
	if !safeRouteEvidence(c.config.Model, evidence) {
		return unsafeError(errors.New("OmniRoute route evidence is missing or unsafe"))
	}
	return unsafeError(errAttemptReceiptsUnavailable)
}

func (c *Client) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	if c == nil {
		return provider.Response{}, unsafeError(nil)
	}
	if !c.config.EnableAttemptReceipts {
		return provider.Response{}, unsafeError(errAttemptReceiptsUnavailable)
	}
	if c.config.Provider == "" || c.config.AccountLaneHash == "" {
		return provider.Response{}, &Error{Kind: ErrorAttemptReceiptsInvalid}
	}
	if strings.TrimSpace(request.ClientRequestID) == "" {
		return provider.Response{}, &Error{Kind: ErrorAttemptReceiptsInvalid}
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		request.Model = c.config.Model
	}
	if request.Model != c.config.Model {
		return provider.Response{}, &Error{Kind: ErrorAttemptReceiptsInvalid}
	}
	response, callErr := c.completeOnce(ctx, request)
	var receiptValidationErr *Error
	if errors.As(callErr, &receiptValidationErr) && receiptValidationErr.Kind == ErrorAttemptReceiptsInvalid {
		return response, callErr
	}
	if response.Metadata.AttemptReceipts == nil {
		return response, &Error{
			Kind:            ErrorAttemptReceiptsMissing,
			StatusCode:      response.Metadata.StatusCode,
			RequestID:       response.Metadata.RequestID,
			UpstreamReached: response.Metadata.StatusCode != 0,
			Cause:           callErr,
		}
	}
	err := provider.ValidateAttemptReceiptSet(*response.Metadata.AttemptReceipts, provider.AttemptReceiptExpectation{
		ClientRequestID: request.ClientRequestID,
		Provider:        c.config.Provider,
		Model:           c.config.Model,
		AccountLaneHash: c.config.AccountLaneHash,
		Now:             c.now(),
	})
	if err != nil {
		return response, &Error{Kind: ErrorAttemptReceiptsInvalid, StatusCode: response.Metadata.StatusCode, RequestID: response.Metadata.RequestID, UpstreamReached: response.Metadata.StatusCode != 0, Cause: err}
	}
	return response, callErr
}

func (c *Client) clearVerification() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.verified = false
	c.mu.Unlock()
}

func (c *Client) managementEvidence(ctx context.Context, endpoint string) ([]byte, error) {
	body, _, err := c.getTelemetry(ctx, endpoint)
	if err == nil {
		return body, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return nil, unsafeError(err)
}

func safeTransport(transport http.RoundTripper) bool {
	if transport == nil {
		return true
	}
	_, ok := transport.(*http.Transport)
	return ok
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
