package omniroute

import (
	"context"
	"errors"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Classify maps only sanitized adapter errors and response metadata into the
// governor's existing account-protection outcome vocabulary.
func Classify(response provider.Response, err error) governor.Outcome {
	reached := response.Metadata.StatusCode != 0 || response.Metadata.Endpoint != ""
	if providerErr := new(Error); errors.As(err, &providerErr) {
		reached = reached || providerErr.UpstreamReached
		return classifiedError(providerErr, reached)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			if reached {
				return governor.Outcome{Class: governor.OutcomeUncertainReached, UpstreamReached: true}
			}
			return governor.Outcome{Class: governor.OutcomeCancelledBeforeUpstream}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return governor.Outcome{Class: governor.OutcomeTimeout, UpstreamReached: reached}
		}
		return governor.Outcome{Class: governor.OutcomeUncertainReached, UpstreamReached: reached}
	}
	if strings.TrimSpace(response.Text) == "" {
		return governor.Outcome{Class: governor.OutcomeEmptyResponse, UpstreamReached: reached}
	}
	return governor.Outcome{Class: governor.OutcomeSuccess, UpstreamReached: reached}
}

func classifiedError(providerErr *Error, reached bool) governor.Outcome {
	if providerErr == nil {
		return governor.Outcome{Class: governor.OutcomeUncertainReached, UpstreamReached: reached}
	}
	base := governor.Outcome{RetryAfter: providerErr.RetryAfter, ResetAt: providerErr.ResetAt, UpstreamReached: reached}
	switch providerErr.Kind {
	case ErrorRateCapacity:
		base.Class = governor.OutcomeRateCapacity
	case ErrorAuthenticationExpired:
		base.Class = governor.OutcomeAuthenticationExpired
	case ErrorAuthenticationDenied:
		base.Class = governor.OutcomeAuthenticationDenied
	case ErrorHTTP403:
		base.Class = governor.OutcomeHTTP403
	case ErrorLoginChallenge:
		base.Class = governor.OutcomeLoginChallenge
	case ErrorCAPTCHA:
		base.Class = governor.OutcomeCAPTCHA
	case ErrorSuspiciousActivity:
		base.Class = governor.OutcomeSuspiciousActivity
	case ErrorAccountWarning:
		base.Class = governor.OutcomeAccountWarning
	case ErrorFeatureRestriction:
		base.Class = governor.OutcomeFeatureRestriction
	case ErrorConnectionReset:
		base.Class = governor.OutcomeConnectionReset
	case ErrorTimeout:
		base.Class = governor.OutcomeTimeout
	case ErrorEmptyResponse:
		base.Class = governor.OutcomeEmptyResponse
	case ErrorMalformedJSON, ErrorInvalidEnvelope:
		base.Class = governor.OutcomeMalformedUpstream
	case ErrorUpstreamServerFailure:
		base.Class = governor.OutcomeUpstreamServerFailure
	case ErrorCancelled:
		if reached {
			base.Class = governor.OutcomeUncertainReached
			base.UpstreamReached = true
		} else {
			base.Class = governor.OutcomeCancelledBeforeUpstream
			base.UpstreamReached = false
		}
	default:
		base.Class = governor.OutcomeUncertainReached
	}
	return base
}
