package provider

import (
	"testing"
	"time"
)

func TestRateLimitObservationSanitizedPreservesZeroAndDropsInvalidFields(t *testing.T) {
	zero, negative, max := int64(0), int64(-1), int64(1<<63-1)
	got := (RateLimitObservation{
		ObservedRetryAfter: 30*24*time.Hour + time.Nanosecond,
		LimitRequests:      &zero,
		RemainingRequests:  &zero,
		ResetRequests:      time.Nanosecond,
		LimitTokens:        &max,
		RemainingTokens:    &negative,
		ResetTokens:        31 * 24 * time.Hour,
	}).Sanitized()
	if got.ObservedRetryAfter != 0 || got.LimitRequests != nil || got.RemainingRequests == nil || *got.RemainingRequests != 0 || got.ResetRequests != time.Nanosecond || got.LimitTokens != nil || got.RemainingTokens != nil || got.ResetTokens != 0 {
		t.Fatalf("Sanitized() = %+v, invalid fields were not independently cleared", got)
	}
}

func TestRateLimitObservationSanitizedEnforcesFiniteCounterBound(t *testing.T) {
	maxAllowed := int64(1_000_000_000)
	maxPlusOne, maxInt := maxAllowed+1, int64(1<<63-1)
	zero := int64(0)
	got := (RateLimitObservation{
		LimitRequests:     &maxAllowed,
		RemainingRequests: &zero,
		LimitTokens:       &maxPlusOne,
		RemainingTokens:   &maxInt,
	}).Sanitized()
	if got.LimitRequests == nil || *got.LimitRequests != maxAllowed {
		t.Fatalf("maximum supported counter = %v, want %d", got.LimitRequests, maxAllowed)
	}
	if got.LimitRequests == &maxAllowed {
		t.Fatal("Sanitized() retained a caller-owned pointer")
	}
	if got.RemainingRequests == nil || *got.RemainingRequests != 0 {
		t.Fatalf("observed remaining=0 lost distinction from unknown: %v", got.RemainingRequests)
	}
	if got.LimitTokens != nil || got.RemainingTokens != nil {
		t.Fatalf("over-bound or MaxInt64 counters survived shared sanitization: %+v", got)
	}
}
