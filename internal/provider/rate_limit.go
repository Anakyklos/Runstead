package provider

import "time"

const (
	maxRateLimitObservationValueBytes = 128
	maxRateLimitObservationDuration   = 30 * 24 * time.Hour
)

// MaxRateLimitCounter bounds provider-reported request and token counts at one
// billion per reported window. This provider-neutral ceiling is far above
// practical quota values while ensuring implausibly large integers remain
// unknown instead of becoming durable evidence.
const MaxRateLimitCounter int64 = 1_000_000_000

// RateLimitObservation is the closed, sanitized rate-limit subset supported by
// compatible provider adapters. Nil counters mean unknown; pointers preserve
// an observed remaining value of zero. Durations of zero mean unknown.
//
// This is diagnostic evidence only. It does not grant admission, authorize a
// retry, or replace ResponseMetadata.RetryAfter, which remains the existing
// governor input.
type RateLimitObservation struct {
	ObservedRetryAfter time.Duration
	LimitRequests      *int64
	RemainingRequests  *int64
	ResetRequests      time.Duration
	LimitTokens        *int64
	RemainingTokens    *int64
	ResetTokens        time.Duration
}

// Sanitized returns a copy containing only values valid under the shared
// provider-boundary limits. Each invalid field independently becomes unknown.
func (o RateLimitObservation) Sanitized() RateLimitObservation {
	return RateLimitObservation{
		ObservedRetryAfter: sanitizeRateLimitDuration(o.ObservedRetryAfter),
		LimitRequests:      sanitizeRateLimitCounter(o.LimitRequests, false),
		RemainingRequests:  sanitizeRateLimitCounter(o.RemainingRequests, true),
		ResetRequests:      sanitizeRateLimitDuration(o.ResetRequests),
		LimitTokens:        sanitizeRateLimitCounter(o.LimitTokens, false),
		RemainingTokens:    sanitizeRateLimitCounter(o.RemainingTokens, true),
		ResetTokens:        sanitizeRateLimitDuration(o.ResetTokens),
	}
}

func sanitizeRateLimitCounter(value *int64, allowZero bool) *int64 {
	if value == nil || *value < 0 || *value > MaxRateLimitCounter || (!allowZero && *value == 0) {
		return nil
	}
	copy := *value
	return &copy
}

func sanitizeRateLimitDuration(value time.Duration) time.Duration {
	if value <= 0 || value > maxRateLimitObservationDuration {
		return 0
	}
	return value
}
