package omniroute

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func httpError(metadata provider.ResponseMetadata, body []byte) *Error {
	signal := errorSignal(body)
	kind := classifySignal(signal)
	if kind == "" {
		switch {
		case metadata.StatusCode == http.StatusUnauthorized:
			kind = ErrorAuthenticationDenied
		case metadata.StatusCode == http.StatusForbidden:
			kind = ErrorHTTP403
		case metadata.StatusCode == http.StatusRequestTimeout:
			kind = ErrorTimeout
		case metadata.StatusCode == http.StatusTooManyRequests:
			kind = ErrorRateCapacity
		case metadata.StatusCode >= http.StatusInternalServerError:
			kind = ErrorUpstreamServerFailure
		default:
			kind = ErrorHTTPStatus
		}
	}
	return &Error{
		Kind:            kind,
		StatusCode:      metadata.StatusCode,
		RequestID:       metadata.RequestID,
		RetryAfter:      metadata.RetryAfter,
		ResetAt:         metadata.ResetAt,
		UpstreamReached: true,
	}
}

func errorSignal(body []byte) string {
	var value any
	if json.Unmarshal(body, &value) != nil {
		if len(body) <= 256 {
			return strings.ToLower(string(body))
		}
		return ""
	}
	var parts []string
	collectSignal(&parts, value)
	return strings.ToLower(strings.Join(parts, " "))
}

func collectSignal(parts *[]string, value any) {
	switch typed := value.(type) {
	case string:
		if len(typed) <= 256 {
			*parts = append(*parts, typed)
		}
	case []any:
		for _, item := range typed {
			collectSignal(parts, item)
		}
	case map[string]any:
		for key, item := range typed {
			lowerKey := strings.ToLower(key)
			switch lowerKey {
			case "code", "type", "message", "detail", "error", "status":
				collectSignal(parts, item)
			}
		}
	}
}

func classifySignal(signal string) ErrorKind {
	switch {
	case containsSignal(signal, "captcha", "recaptcha", "hcaptcha"):
		return ErrorCAPTCHA
	case containsSignal(signal, "suspicious activity", "suspicious_activity", "suspicious"):
		return ErrorSuspiciousActivity
	case containsSignal(signal, "login challenge", "login_challenge", "login_required", "reauthenticate", "verify your identity", "challenge_required", "authentication_challenge"):
		return ErrorLoginChallenge
	case containsSignal(signal, "account warning", "account_warning", "account_warn"):
		return ErrorAccountWarning
	case containsSignal(signal, "feature restriction", "feature_restricted", "feature_restriction"):
		return ErrorFeatureRestriction
	case containsSignal(signal, "connection reset", "connection_reset"):
		return ErrorConnectionReset
	case containsSignal(signal, "token expired", "token_expired", "expired_token", "session_expired"):
		return ErrorAuthenticationExpired
	case containsSignal(signal, "invalid api key", "invalid_api_key", "invalid token", "invalid_token", "unauthorized"):
		return ErrorAuthenticationDenied
	case containsSignal(signal, "rate limit", "rate_limit", "too many requests", "capacity", "quota exceeded", "quota_exceeded"):
		return ErrorRateCapacity
	}
	return ""
}

func containsSignal(signal string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(signal, value) {
			return true
		}
	}
	return false
}
