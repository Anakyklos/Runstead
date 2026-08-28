package compat

// Issue #92: provider-neutral retry classification seam. The governor must
// never import the concrete adapters, and retry decisions must come from
// typed evidence, never from error-message text. This classifier translates
// the adapters' closed ErrorKind vocabularies (identical across the three
// families) into governor OutcomeClass values. Retry ELIGIBILITY remains a
// governor decision (isRecoverableOutcome + delivery evidence + retry budget
// + circuit), so a misclassification here can only make a class MORE
// recoverable than the governor allows, never less safe.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

// NewClassifier returns the composition-layer OutcomeClassifier for the
// compatible adapters. It never renders error text and never carries
// credentials, prompts or response bodies: it maps typed kinds only.
func NewClassifier() governor.OutcomeClassifier {
	return func(response provider.Response, err error) governor.Outcome {
		outcome := governor.Outcome{
			DeliveryState: response.Metadata.DeliveryState,
			RetryAfter:    response.Metadata.RetryAfter,
			ResetAt:       response.Metadata.ResetAt,
		}
		if err == nil {
			if strings.TrimSpace(response.Text) == "" {
				outcome.Class = governor.OutcomeEmptyResponse
				outcome.UpstreamReached = true
				return outcome
			}
			outcome.Class = governor.OutcomeSuccess
			outcome.UpstreamReached = true
			return outcome
		}
		// Caller-side context errors (adapter errors carry finer kinds).
		if errors.Is(err, context.Canceled) {
			outcome.Class = governor.OutcomeCancelledBeforeUpstream
			return outcome
		}
		if errors.Is(err, context.DeadlineExceeded) {
			outcome.Class = governor.OutcomeTimeout
			return outcome
		}
		var openAI *openaicompat.Error
		var anthropic *anthropiccompat.Error
		var google *googlecompat.Error
		switch {
		case errors.As(err, &openAI):
			return classifyError(outcome, string(openAI.Kind), openAI.RetryAfter)
		case errors.As(err, &anthropic):
			return classifyError(outcome, string(anthropic.Kind), anthropic.RetryAfter)
		case errors.As(err, &google):
			return classifyError(outcome, string(google.Kind), google.RetryAfter)
		default:
			// Unknown failure taxonomy: never retryable (uncertain).
			outcome.Class = governor.OutcomeUncertainReached
			outcome.UpstreamReached = true
			return outcome
		}
	}
}

// classifyError maps one closed adapter ErrorKind onto the governor outcome
// taxonomy. Non-retryable families (auth, permission, config, refusal,
// payload/context reconstruction, unsafe redirect, unknown) map to classes
// that isRecoverableOutcome returns FALSE for; delivery-safe families map to
// rate/capacity or upstream-server-failure carrying the authoritative
// Retry-After when observed.
func classifyError(outcome governor.Outcome, kind string, retryAfter time.Duration) governor.Outcome {
	outcome.UpstreamReached = true
	switch kind {
	case "rate_or_capacity":
		outcome.Class = governor.OutcomeRateCapacity
		if retryAfter > outcome.RetryAfter {
			outcome.RetryAfter = retryAfter
		}
	case "upstream_server_failure":
		outcome.Class = governor.OutcomeUpstreamServerFailure
	case "timeout":
		outcome.Class = governor.OutcomeTimeout
	case "empty_response":
		outcome.Class = governor.OutcomeEmptyResponse
	case "malformed_response", "invalid_envelope":
		// A 2xx/3xx body that did not parse is an upstream contract problem;
		// classified as malformed upstream and gated by the governor's
		// recoverable set.
		outcome.Class = governor.OutcomeMalformedUpstream
	case "cancelled":
		outcome.Class = governor.OutcomeCancelledBeforeUpstream
	case "authentication_denied", "auth_unavailable":
		outcome.Class = governor.OutcomeAuthenticationDenied
	case "permission_denied":
		outcome.Class = governor.OutcomeHTTP403
	case "response_too_large", "request_too_large", "unsafe_redirect", "config_refused", "transport":
		// Requires reconstruction, refuses dispatch or is ambiguous: never
		// retryable.
		outcome.Class = governor.OutcomeUncertainReached
	default:
		outcome.Class = governor.OutcomeUncertainReached
	}
	return outcome
}
