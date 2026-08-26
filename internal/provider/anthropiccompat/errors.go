// Package anthropiccompat implements the provider.Client contract for the
// anthropic_compatible protocol family (#88). It is a protocol adapter, not an
// integration with any specific vendor: the official Anthropic service, local
// gateways and third-party endpoints are all just implementations of the same
// minimal Messages-style subset this adapter speaks. Provider identity stays
// completely separate from the protocol family; two different provider IDs of
// this family are served by the same adapter path with no agent-loop branching.
//
// The adapter executes exactly one physical HTTP request per Complete call.
// It never retries, falls back, rotates providers/keys or follows redirects.
// Delivery evidence is derived from observable transport facts only; absence
// of evidence is never treated as proof that nothing was sent.
package anthropiccompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// ErrorKind is a stable, sanitized classification of one failed completion.
// Kinds describe what the adapter itself observed; they never guess at causes
// the endpoint did not prove and never carry response bodies or secrets.
type ErrorKind string

const (
	// ErrorConfigRefused covers pre-dispatch refusals: incompatible family,
	// unsafe route safety, model mismatch, unusable endpoint, unsupported or
	// invalid protocol options. Zero requests.
	ErrorConfigRefused ErrorKind = "config_refused"
	// ErrorAuthUnavailable covers required authentication that could not be
	// resolved from its non-secret reference before dispatch. Zero requests.
	ErrorAuthUnavailable ErrorKind = "auth_unavailable"
	// ErrorAuthenticationDenied is an upstream 401.
	ErrorAuthenticationDenied ErrorKind = "authentication_denied"
	// ErrorPermissionDenied is an upstream 403 (or 407 proxy denial).
	ErrorPermissionDenied ErrorKind = "permission_denied"
	// ErrorRateCapacity is an upstream 429 (rate/quota/capacity).
	ErrorRateCapacity ErrorKind = "rate_or_capacity"
	// ErrorTimeout covers context deadlines and transport timeouts.
	ErrorTimeout ErrorKind = "timeout"
	// ErrorCancelled covers caller cancellation observed by the adapter.
	ErrorCancelled ErrorKind = "cancelled"
	// ErrorMalformedResponse is a 2xx body that does not parse as JSON.
	ErrorMalformedResponse ErrorKind = "malformed_response"
	// ErrorInvalidEnvelope is parsed JSON without the minimal supported shape.
	ErrorInvalidEnvelope ErrorKind = "invalid_envelope"
	// ErrorEmptyResponse is an empty body or empty/incompatible content.
	ErrorEmptyResponse ErrorKind = "empty_response"
	// ErrorUnsupportedResponseFormat covers response shapes the baseline
	// cannot prove and consume: unsupported content block types (tool_use,
	// thinking, redacted_thinking, ...) and tool-use-only turns. These are
	// NEVER interpreted as text or as Runstead task truth.
	ErrorUnsupportedResponseFormat ErrorKind = "unsupported_response_format"
	// ErrorIncompleteCompletion covers Messages stop reasons that prove the
	// generation was truncated or otherwise not a complete natural turn
	// (max_tokens, stop_sequence, pause_turn, model_context_window_exceeded).
	// The transport delivered the body; the completion is not a complete
	// runstead.protocol.v1 turn.
	ErrorIncompleteCompletion ErrorKind = "incomplete_completion"
	// ErrorRefusal covers a provable refusal state (stop_reason "refusal" or
	// stop_details.type "refusal"), classified from typed structure, never
	// from parsing free text.
	ErrorRefusal ErrorKind = "refusal"
	// ErrorResponseTooLarge exceeds the configured response byte bound.
	ErrorResponseTooLarge ErrorKind = "response_too_large"
	// ErrorRequestTooLarge exceeds the configured request byte bound.
	ErrorRequestTooLarge ErrorKind = "request_too_large"
	// ErrorUpstreamServerFailure is an upstream 5xx.
	ErrorUpstreamServerFailure ErrorKind = "upstream_server_failure"
	// ErrorUpstreamHTTPFailure is any other non-2xx upstream status.
	ErrorUpstreamHTTPFailure ErrorKind = "upstream_http_failure"
	// ErrorUnsafeRedirect is a redirect refused instead of followed. A redirect
	// can never trigger a second physical request without new admission.
	ErrorUnsafeRedirect ErrorKind = "unsafe_redirect"
	// ErrorTransport covers remaining transport failures after dispatch became
	// possible. Ambiguity about whether bytes left is preserved through the
	// DeliveryState, never collapsed into "not sent".
	ErrorTransport ErrorKind = "transport"
)

// Error is intentionally small and sanitized. Cause is retained only for
// errors.Is/As inspection; its text is never rendered by Error(), so response
// bodies, header values and secret material cannot leak into diagnostics,
// traces or persisted evidence.
type Error struct {
	Kind            ErrorKind
	StatusCode      int
	RequestID       string
	RetryAfter      time.Duration
	DeliveryState   provider.DeliveryState
	UpstreamReached bool
	Cause           error
}

func (e *Error) Error() string {
	if e == nil {
		return "anthropic_compatible error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("anthropic_compatible %s (status %d)", e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("anthropic_compatible %s", e.Kind)
}

// GoString keeps %#v formatting of errors sanitized as well.
func (e *Error) GoString() string { return e.Error() }

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// configRefusedError marks a pre-dispatch refusal. The returned error always
// carries DeliveryNotSent: refusing configuration provably produced zero
// model-effect requests.
func configRefusedError(cause error) *Error {
	if cause == nil {
		cause = provider.ErrInvalidProviderConfig
	}
	return &Error{
		Kind:          ErrorConfigRefused,
		DeliveryState: provider.DeliveryNotSent,
		Cause:         cause,
	}
}

// authUnavailableError marks a failure to resolve required authentication
// material before dispatch. No request was built or sent.
func authUnavailableError(cause error) *Error {
	return &Error{
		Kind:          ErrorAuthUnavailable,
		DeliveryState: provider.DeliveryNotSent,
		Cause:         cause,
	}
}

// unsafeRedirectError classifies a refused redirect response. The first
// physical request reached the wire; no second request was ever made.
func unsafeRedirectError(metadata provider.ResponseMetadata, statusCode int, locationHash string) *Error {
	return &Error{
		Kind:            ErrorUnsafeRedirect,
		StatusCode:      statusCode,
		RequestID:       metadata.RequestID,
		DeliveryState:   metadata.DeliveryState,
		UpstreamReached: true,
		Cause:           fmt.Errorf("redirect to hashed target %s refused; a second model-effect request requires new governor admission", locationHash),
	}
}

// contextError maps ctx.Err() onto the timeout/cancelled classification.
func contextError(err error, delivery provider.DeliveryState) *Error {
	kind := ErrorCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorTimeout
	}
	return &Error{Kind: kind, DeliveryState: delivery, Cause: err}
}

// transportError classifies a non-nil client.Do error conservatively using the
// observed delivery state as the only source of transport evidence.
func transportError(err error, delivery provider.DeliveryState) *Error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextError(err, delivery)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: ErrorTimeout, DeliveryState: delivery, Cause: context.DeadlineExceeded}
	}
	return &Error{Kind: ErrorTransport, DeliveryState: delivery, Cause: err}
}

// httpError classifies an observed non-2xx status. The body is deliberately
// not inspected for causes the adapter cannot prove; only the status code and
// an observably valid Retry-After value are normalized.
func httpError(metadata provider.ResponseMetadata, statusCode int) *Error {
	kind := ErrorUpstreamHTTPFailure
	switch {
	case statusCode == http.StatusUnauthorized:
		kind = ErrorAuthenticationDenied
	case statusCode == http.StatusForbidden || statusCode == http.StatusProxyAuthRequired:
		kind = ErrorPermissionDenied
	case statusCode == http.StatusTooManyRequests:
		kind = ErrorRateCapacity
	case statusCode >= http.StatusInternalServerError:
		kind = ErrorUpstreamServerFailure
	}
	return &Error{
		Kind:            kind,
		StatusCode:      statusCode,
		RequestID:       metadata.RequestID,
		RetryAfter:      metadata.RetryAfter,
		DeliveryState:   metadata.DeliveryState,
		UpstreamReached: true,
	}
}

// parseRetryAfter normalizes an observably valid Retry-After header value.
// Anything absent or unparseable yields zero rather than a guess.
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

// logicalEndpoint reduces a URL to its path so sanitized metadata stays free
// of credentials-by-construction while still identifying the route.
func logicalEndpoint(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "#unparseable-endpoint"
	}
	path := parsed.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// hashOpaque renders untrusted opaque values as short hashes so they can never
// smuggle credential material or oversized junk into metadata or errors.
func hashOpaque(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:8])
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
