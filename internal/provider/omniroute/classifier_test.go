package omniroute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestClassifyMapsProviderFailuresAndPreservesHints(t *testing.T) {
	reset := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		kind  ErrorKind
		class governor.OutcomeClass
	}{
		{name: "expired auth", kind: ErrorAuthenticationExpired, class: governor.OutcomeAuthenticationExpired},
		{name: "denied auth", kind: ErrorAuthenticationDenied, class: governor.OutcomeAuthenticationDenied},
		{name: "403", kind: ErrorHTTP403, class: governor.OutcomeHTTP403},
		{name: "challenge", kind: ErrorLoginChallenge, class: governor.OutcomeLoginChallenge},
		{name: "captcha", kind: ErrorCAPTCHA, class: governor.OutcomeCAPTCHA},
		{name: "suspicious", kind: ErrorSuspiciousActivity, class: governor.OutcomeSuspiciousActivity},
		{name: "warning", kind: ErrorAccountWarning, class: governor.OutcomeAccountWarning},
		{name: "feature", kind: ErrorFeatureRestriction, class: governor.OutcomeFeatureRestriction},
		{name: "reset", kind: ErrorConnectionReset, class: governor.OutcomeConnectionReset},
		{name: "timeout", kind: ErrorTimeout, class: governor.OutcomeTimeout},
		{name: "empty", kind: ErrorEmptyResponse, class: governor.OutcomeEmptyResponse},
		{name: "malformed", kind: ErrorMalformedJSON, class: governor.OutcomeMalformedUpstream},
		{name: "server", kind: ErrorUpstreamServerFailure, class: governor.OutcomeUpstreamServerFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := Classify(provider.Response{}, &Error{
				Kind:            tt.kind,
				RetryAfter:      7 * time.Second,
				ResetAt:         reset,
				UpstreamReached: true,
			})
			if outcome.Class != tt.class || !outcome.UpstreamReached || outcome.RetryAfter != 7*time.Second || !outcome.ResetAt.Equal(reset) {
				t.Fatalf("Classify() = %#v, want class %s with hints", outcome, tt.class)
			}
		})
	}
}

func TestClassifyTreatsCancellationAfterAttemptAsUncertain(t *testing.T) {
	outcome := Classify(provider.Response{Metadata: provider.ResponseMetadata{Endpoint: "/v1/chat/completions"}}, &Error{
		Kind:            ErrorCancelled,
		Cause:           context.Canceled,
		UpstreamReached: true,
	})
	if outcome.Class != governor.OutcomeUncertainReached || !outcome.UpstreamReached {
		t.Fatalf("Classify() = %#v, want uncertain reached", outcome)
	}

	outcome = Classify(provider.Response{}, &Error{Kind: ErrorCancelled, Cause: context.Canceled})
	if outcome.Class != governor.OutcomeCancelledBeforeUpstream || outcome.UpstreamReached {
		t.Fatalf("pre-attempt Classify() = %#v, want cancelled before upstream", outcome)
	}
	if !errors.Is((&Error{Kind: ErrorCancelled, Cause: context.Canceled}), context.Canceled) {
		t.Fatal("typed cancellation did not preserve errors.Is")
	}
}

func TestClassifyCarriesRawDeliveryState(t *testing.T) {
	response := provider.Response{
		Text: "response",
		Metadata: provider.ResponseMetadata{
			DeliveryState: provider.DeliverySentUnconfirmed,
		},
	}
	outcome := Classify(response, nil)
	if outcome.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("delivery state = %v, want sent_unconfirmed", outcome.DeliveryState)
	}
}

func TestClassifyValidTextAndEmptyText(t *testing.T) {
	if got := Classify(provider.Response{Text: " refusal text "}, nil); got.Class != governor.OutcomeSuccess {
		t.Fatalf("text outcome = %#v, want success", got)
	}
	if got := Classify(provider.Response{}, nil); got.Class != governor.OutcomeEmptyResponse {
		t.Fatalf("empty outcome = %#v, want empty response", got)
	}
}
