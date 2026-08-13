package omniroute

import (
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type ErrorKind string

const (
	ErrorTransport              ErrorKind = "transport"
	ErrorTimeout                ErrorKind = "timeout"
	ErrorCancelled              ErrorKind = "cancelled"
	ErrorAuthenticationExpired  ErrorKind = "authentication_expired"
	ErrorAuthenticationDenied   ErrorKind = "authentication_denied"
	ErrorHTTP403                ErrorKind = "http_403"
	ErrorRateCapacity           ErrorKind = "rate_or_capacity"
	ErrorLoginChallenge         ErrorKind = "login_challenge"
	ErrorCAPTCHA                ErrorKind = "captcha"
	ErrorSuspiciousActivity     ErrorKind = "suspicious_activity"
	ErrorAccountWarning         ErrorKind = "account_warning"
	ErrorFeatureRestriction     ErrorKind = "feature_restriction"
	ErrorConnectionReset        ErrorKind = "connection_reset"
	ErrorEmptyResponse          ErrorKind = "empty_response"
	ErrorMalformedJSON          ErrorKind = "malformed_json"
	ErrorInvalidEnvelope        ErrorKind = "invalid_envelope"
	ErrorUpstreamServerFailure  ErrorKind = "upstream_server_failure"
	ErrorHTTPStatus             ErrorKind = "http_status"
	ErrorResponseTooLarge       ErrorKind = "response_too_large"
	ErrorRequestTooLarge        ErrorKind = "request_too_large"
	ErrorUnsafeRoute            ErrorKind = "unsafe_route"
	ErrorTelemetry              ErrorKind = "telemetry"
	ErrorAttemptReceiptsMissing ErrorKind = "attempt_receipts_missing"
	ErrorAttemptReceiptsInvalid ErrorKind = "attempt_receipts_invalid"
)

var errAttemptReceiptsUnavailable = errors.New("OmniRoute authoritative attempt receipts are unavailable")

// Error is intentionally small and sanitized. Cause is retained only for
// errors.Is checks; its text is never included in Error().
type Error struct {
	Kind            ErrorKind
	StatusCode      int
	RequestID       string
	RetryAfter      time.Duration
	ResetAt         time.Time
	UpstreamReached bool
	Cause           error
}

func (e *Error) Error() string {
	if e == nil {
		return "omniroute error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("omniroute %s (status %d)", e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("omniroute %s", e.Kind)
}

func (e *Error) GoString() string { return e.Error() }

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func unsafeError(cause error) error {
	if cause == nil {
		cause = provider.ErrUnsafeRoute
	}
	return &Error{Kind: ErrorUnsafeRoute, Cause: errors.Join(provider.ErrUnsafeRoute, cause)}
}
