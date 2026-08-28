package compat

// Issue #92: the provider-neutral classifier maps the three identical closed
// adapter ErrorKind vocabularies onto governor outcome classes, and unknown
// taxonomies / free text never become retryable classes.

import (
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
