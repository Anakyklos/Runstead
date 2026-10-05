package openaicompat

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const maxRateLimitHeaderValueBytes = 128

// observeRateLimits parses only the fixed OpenAI-compatible rate-limit header
// allowlist. Malformed or ambiguous values become unknown independently and
// never affect response delivery or classification.
func observeRateLimits(headers http.Header, now time.Time) provider.RateLimitObservation {
	return provider.RateLimitObservation{
		ObservedRetryAfter: parseObservedRetryAfter(headers, now),
		LimitRequests:      parseRateLimitCounter(headers, "X-Ratelimit-Limit-Requests", false),
		RemainingRequests:  parseRateLimitCounter(headers, "X-Ratelimit-Remaining-Requests", true),
		ResetRequests:      parseRateLimitDuration(headers, "X-Ratelimit-Reset-Requests"),
		LimitTokens:        parseRateLimitCounter(headers, "X-Ratelimit-Limit-Tokens", false),
		RemainingTokens:    parseRateLimitCounter(headers, "X-Ratelimit-Remaining-Tokens", true),
		ResetTokens:        parseRateLimitDuration(headers, "X-Ratelimit-Reset-Tokens"),
	}.Sanitized()
}

func singleRateLimitHeader(headers http.Header, name string) (string, bool) {
	values := headers.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	if value == "" || len(value) > maxRateLimitHeaderValueBytes {
		return "", false
	}
	return value, true
}

func parseRateLimitCounter(headers http.Header, name string, allowZero bool) *int64 {
	value, ok := singleRateLimitHeader(headers, name)
	if !ok {
		return nil
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return nil
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || (!allowZero && parsed == 0) {
		return nil
	}
	return &parsed
}

func parseRateLimitDuration(headers http.Header, name string) time.Duration {
	value, ok := singleRateLimitHeader(headers, name)
	if !ok {
		return 0
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 || parsed > 30*24*time.Hour {
		return 0
	}
	return parsed
}

func parseObservedRetryAfter(headers http.Header, now time.Time) time.Duration {
	value, ok := singleRateLimitHeader(headers, "Retry-After")
	if !ok {
		return 0
	}
	digits := true
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64((30*24*time.Hour)/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	duration := when.Sub(now)
	if duration <= 0 || duration > 30*24*time.Hour {
		return 0
	}
	return duration
}
