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
	if got.ObservedRetryAfter != 0 || got.LimitRequests != nil || got.RemainingRequests == nil || *got.RemainingRequests != 0 || got.ResetRequests != time.Nanosecond || got.LimitTokens == nil || *got.LimitTokens != max || got.RemainingTokens != nil || got.ResetTokens != 0 {
		t.Fatalf("Sanitized() = %+v, invalid fields were not independently cleared", got)
	}
	if got.LimitTokens == &max {
		t.Fatal("Sanitized() retained a caller-owned pointer")
	}
}
