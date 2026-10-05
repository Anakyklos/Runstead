package openaicompat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObserveRateLimitsParsesAllowlistedValues(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("Retry-After", "29")
	headers.Set("X-Ratelimit-Limit-Requests", "120")
	headers.Set("X-Ratelimit-Remaining-Requests", "0")
	headers.Set("X-Ratelimit-Reset-Requests", "1m30s")
	headers.Set("X-Ratelimit-Limit-Tokens", "9000")
	headers.Set("X-Ratelimit-Remaining-Tokens", "17")
	headers.Set("X-Ratelimit-Reset-Tokens", "2h")
	headers.Set("X-Provider-Secret", "do-not-retain")

	got := observeRateLimits(headers, now)
	if got.ObservedRetryAfter != 29*time.Second || value(got.LimitRequests) != 120 || value(got.RemainingRequests) != 0 || got.ResetRequests != 90*time.Second || value(got.LimitTokens) != 9000 || value(got.RemainingTokens) != 17 || got.ResetTokens != 2*time.Hour {
		t.Fatalf("unexpected observation: %+v", got)
	}
	if got.RemainingRequests == nil {
		t.Fatal("remaining requests zero must remain distinguishable from absence")
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "do-not-retain") {
		t.Fatal("arbitrary header text survived in observation")
	}
}

func TestObserveRateLimitsInvalidValuesBecomeUnknown(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		field string
		value string
	}{
		{"malformed integer", "X-Ratelimit-Limit-Requests", "12x"},
		{"negative integer", "X-Ratelimit-Limit-Requests", "-12"},
		{"overflow integer", "X-Ratelimit-Remaining-Tokens", "9223372036854775808"},
		{"oversized integer", "X-Ratelimit-Limit-Tokens", strings.Repeat("1", 129)},
		{"remaining negative", "X-Ratelimit-Remaining-Requests", "-1"},
		{"duration malformed", "X-Ratelimit-Reset-Requests", "later"},
		{"duration zero", "X-Ratelimit-Reset-Tokens", "0s"},
		{"duration unbounded", "X-Ratelimit-Reset-Requests", "744h"},
		{"duration oversized", "X-Ratelimit-Reset-Tokens", strings.Repeat("1", 129)},
		{"retry malformed", "Retry-After", "not a date"},
		{"retry overflow", "Retry-After", "18446744073709551616"},
		{"retry absurd", "Retry-After", "2592001"},
		{"retry oversized", "Retry-After", strings.Repeat("1", 129)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set(test.field, test.value)
			got := observeRateLimits(headers, now)
			if got.ObservedRetryAfter != 0 || got.LimitRequests != nil || got.RemainingRequests != nil || got.ResetRequests != 0 || got.LimitTokens != nil || got.RemainingTokens != nil || got.ResetTokens != 0 {
				t.Fatalf("invalid field was not discarded: %+v", got)
			}
		})
	}
}

func TestObserveRateLimitsRejectsDuplicateHeaders(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, values := range [][]string{{"1", "1"}, {"1", "2"}} {
		headers := http.Header{"X-Ratelimit-Remaining-Requests": values}
		if got := observeRateLimits(headers, now); got.RemainingRequests != nil {
			t.Fatalf("duplicate values %v were not rejected: %+v", values, got)
		}
	}
	headers := http.Header{"Retry-After": {"1", "2"}}
	if got := observeRateLimits(headers, now); got.ObservedRetryAfter != 0 {
		t.Fatalf("conflicting Retry-After values were not rejected: %+v", got)
	}
}

func TestObserveRateLimitsSupportsRetryAfterDateAndMissingValues(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("Retry-After", now.Add(3*time.Minute).Format(http.TimeFormat))
	got := observeRateLimits(headers, now)
	if got.ObservedRetryAfter != 3*time.Minute {
		t.Fatalf("date Retry-After = %s, want 3m", got.ObservedRetryAfter)
	}
	if got.LimitRequests != nil || got.RemainingRequests != nil || got.ResetRequests != 0 || got.LimitTokens != nil || got.RemainingTokens != nil || got.ResetTokens != 0 {
		t.Fatalf("absent values must remain unknown: %+v", got)
	}
}

func value(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
