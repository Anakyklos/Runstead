package compat

// Issue #92: the provider-neutral classifier maps the three identical closed
// adapter ErrorKind vocabularies onto governor outcome classes, and unknown
// taxonomies / free text never become retryable classes.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

func TestClassifierMapsAdapterKindsProviderNeutrally(t *testing.T) {
	classifier := NewClassifier()
	cases := []struct {
		name string
		err  error
		want governor.OutcomeClass
	}{
		{"openai 429 rate", &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity}, governor.OutcomeRateCapacity},
		{"anthropic 429 rate", &anthropiccompat.Error{Kind: anthropiccompat.ErrorRateCapacity}, governor.OutcomeRateCapacity},
		{"google 429 rate", &googlecompat.Error{Kind: googlecompat.ErrorRateCapacity}, governor.OutcomeRateCapacity},
		{"openai 5xx", &openaicompat.Error{Kind: openaicompat.ErrorUpstreamServerFailure}, governor.OutcomeUpstreamServerFailure},
		{"anthropic 5xx", &anthropiccompat.Error{Kind: anthropiccompat.ErrorUpstreamServerFailure}, governor.OutcomeUpstreamServerFailure},
		{"google 5xx", &googlecompat.Error{Kind: googlecompat.ErrorUpstreamServerFailure}, governor.OutcomeUpstreamServerFailure},
		{"openai auth denied", &openaicompat.Error{Kind: openaicompat.ErrorAuthenticationDenied}, governor.OutcomeAuthenticationDenied},
		{"anthropic permission denied", &anthropiccompat.Error{Kind: anthropiccompat.ErrorPermissionDenied}, governor.OutcomeHTTP403},
		{"google timeout", &googlecompat.Error{Kind: googlecompat.ErrorTimeout}, governor.OutcomeTimeout},
		{"openai empty response", &openaicompat.Error{Kind: openaicompat.ErrorEmptyResponse}, governor.OutcomeEmptyResponse},
		{"anthropic malformed response", &anthropiccompat.Error{Kind: anthropiccompat.ErrorMalformedResponse}, governor.OutcomeMalformedUpstream},
		// Never retryable:
		{"openai response too large", &openaicompat.Error{Kind: openaicompat.ErrorResponseTooLarge}, governor.OutcomeUncertainReached},
		{"anthropic request too large", &anthropiccompat.Error{Kind: anthropiccompat.ErrorRequestTooLarge}, governor.OutcomeUncertainReached},
		{"google config refused", &googlecompat.Error{Kind: googlecompat.ErrorConfigRefused}, governor.OutcomeUncertainReached},
		{"openai unsafe redirect", &openaicompat.Error{Kind: openaicompat.ErrorUnsafeRedirect}, governor.OutcomeUncertainReached},
		{"google transport", &googlecompat.Error{Kind: googlecompat.ErrorTransport}, governor.OutcomeUncertainReached},
		{"unknown taxonomy", errors.New("plain unknown error"), governor.OutcomeUncertainReached},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			outcome := classifier(provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, testCase.err)
			if outcome.Class != testCase.want {
				t.Fatalf("class = %q, want %q", outcome.Class, testCase.want)
			}
		})
	}
}

func TestClassifierRetainsOnlyKnownTypedFailureKinds(t *testing.T) {
	classifier := NewClassifier()
	typed := []struct {
		name string
		err  error
		want string
	}{
		{"openai timeout wrapping context deadline", &openaicompat.Error{Kind: openaicompat.ErrorTimeout, Cause: errors.Join(errors.New("SECRET_DO_NOT_PERSIST"), context.DeadlineExceeded)}, "timeout"},
		{"anthropic transport", &anthropiccompat.Error{Kind: anthropiccompat.ErrorTransport}, "transport"},
		{"google auth unavailable", &googlecompat.Error{Kind: googlecompat.ErrorAuthUnavailable}, "auth_unavailable"},
		{"openai authentication denied", &openaicompat.Error{Kind: openaicompat.ErrorAuthenticationDenied}, "authentication_denied"},
		{"anthropic permission denied", &anthropiccompat.Error{Kind: anthropiccompat.ErrorPermissionDenied}, "permission_denied"},
		{"google rate capacity", &googlecompat.Error{Kind: googlecompat.ErrorRateCapacity}, "rate_or_capacity"},
		{"openai upstream server", &openaicompat.Error{Kind: openaicompat.ErrorUpstreamServerFailure}, "upstream_server_failure"},
		{"anthropic malformed", &anthropiccompat.Error{Kind: anthropiccompat.ErrorMalformedResponse}, "malformed_response"},
		{"google invalid envelope", &googlecompat.Error{Kind: googlecompat.ErrorInvalidEnvelope}, "invalid_envelope"},
		{"openai response too large", &openaicompat.Error{Kind: openaicompat.ErrorResponseTooLarge}, "response_too_large"},
		{"anthropic request too large", &anthropiccompat.Error{Kind: anthropiccompat.ErrorRequestTooLarge}, "request_too_large"},
		{"google unsafe redirect", &googlecompat.Error{Kind: googlecompat.ErrorUnsafeRedirect}, "unsafe_redirect"},
		{"openai config refused", &openaicompat.Error{Kind: openaicompat.ErrorConfigRefused}, "config_refused"},
		{"unknown typed value", &googlecompat.Error{Kind: googlecompat.ErrorKind("SECRET_DO_NOT_PERSIST")}, ""},
		{"untyped error", errors.New("SECRET_DO_NOT_PERSIST"), ""},
	}
	for _, testCase := range typed {
		t.Run(testCase.name, func(t *testing.T) {
			outcome := classifier(provider.Response{}, testCase.err)
			if got := string(outcome.ProviderFailureClass); got != testCase.want {
				t.Fatalf("provider failure class = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestClassifierCarriesAuthoritativeRetryAfter(t *testing.T) {
	classifier := NewClassifier()
	err := &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity, RetryAfter: 90 * time.Second}
	outcome := classifier(provider.Response{Metadata: provider.ResponseMetadata{RetryAfter: 90 * time.Second}}, err)
	if outcome.Class != governor.OutcomeRateCapacity || outcome.RetryAfter != 90*time.Second {
		t.Fatalf("rate outcome must carry Retry-After: %+v", outcome)
	}
}

func TestClassifierSuccessAndEmptyResponse(t *testing.T) {
	classifier := NewClassifier()
	if outcome := classifier(provider.Response{Text: "ok"}, nil); outcome.Class != governor.OutcomeSuccess {
		t.Fatalf("success = %q", outcome.Class)
	}
	if outcome := classifier(provider.Response{}, nil); outcome.Class != governor.OutcomeEmptyResponse {
		t.Fatalf("empty = %q", outcome.Class)
	}
}
