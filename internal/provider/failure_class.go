package provider

// ProviderFailureClass is the closed, sanitized taxonomy reported by a
// protocol adapter. It records what the adapter observed independently from
// the governor's conservative outcome and delivery decision.
type ProviderFailureClass string

const (
	FailureConfigRefused             ProviderFailureClass = "config_refused"
	FailureAuthUnavailable           ProviderFailureClass = "auth_unavailable"
	FailureAuthenticationDenied      ProviderFailureClass = "authentication_denied"
	FailurePermissionDenied          ProviderFailureClass = "permission_denied"
	FailureRateCapacity              ProviderFailureClass = "rate_or_capacity"
	FailureTimeout                   ProviderFailureClass = "timeout"
	FailureCancelled                 ProviderFailureClass = "cancelled"
	FailureMalformedResponse         ProviderFailureClass = "malformed_response"
	FailureInvalidEnvelope           ProviderFailureClass = "invalid_envelope"
	FailureEmptyResponse             ProviderFailureClass = "empty_response"
	FailureUnsupportedResponseFormat ProviderFailureClass = "unsupported_response_format"
	FailureIncompleteCompletion      ProviderFailureClass = "incomplete_completion"
	FailureRefusal                   ProviderFailureClass = "refusal"
	FailureResponseTooLarge          ProviderFailureClass = "response_too_large"
	FailureRequestTooLarge           ProviderFailureClass = "request_too_large"
	FailureUpstreamServer            ProviderFailureClass = "upstream_server_failure"
	FailureUpstreamHTTP              ProviderFailureClass = "upstream_http_failure"
	FailureUnsafeRedirect            ProviderFailureClass = "unsafe_redirect"
	FailureTransport                 ProviderFailureClass = "transport"
)

// ParseProviderFailureClass accepts only the adapter's documented safe
// vocabulary. Unknown or future values intentionally remain empty so raw or
// unreviewed strings can never enter durable evidence.
func ParseProviderFailureClass(value string) ProviderFailureClass {
	class := ProviderFailureClass(value)
	if class.Valid() {
		return class
	}
	return ""
}

func (c ProviderFailureClass) Valid() bool {
	switch c {
	case FailureConfigRefused, FailureAuthUnavailable, FailureAuthenticationDenied,
		FailurePermissionDenied, FailureRateCapacity, FailureTimeout, FailureCancelled,
		FailureMalformedResponse, FailureInvalidEnvelope, FailureEmptyResponse,
		FailureUnsupportedResponseFormat, FailureIncompleteCompletion, FailureRefusal,
		FailureResponseTooLarge, FailureRequestTooLarge, FailureUpstreamServer,
		FailureUpstreamHTTP, FailureUnsafeRedirect, FailureTransport:
		return true
	default:
		return false
	}
}
