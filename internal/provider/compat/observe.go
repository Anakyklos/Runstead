package compat

import (
	"errors"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/adaptive"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

// Observation converts the outcome of ONE governed physical attempt into
// adaptive evidence (issue #93). It is the only place where adapter-typed
// errors become learnable evidence, and it is deliberately family-neutral:
// the adapter sanitized kinds shared by all three protocol families map to
// the closed adaptive kinds, and nothing else is readable from the error.
//
// The evidence reference is intentionally left EMPTY here: assigning the
// audit reference of the attempt is the caller's (CLI observer) job, and
// Updates() refuses to learn anything without a valid structured reference.
// Free text, prompts, responses and raw headers are never copied into the
// evidence; numeric limits only flow when the response metadata proves them
// (no adapter synthesizes a number today, so they may arrive as zero and
// the mapping then learns nothing rather than inventing a value).
func Observation(response provider.Response, err error, now time.Time) adaptive.Evidence {
	if err == nil {
		return adaptive.Evidence{Kind: adaptive.KindSuccess}
	}

	kind, retryAfter := sanitizedKind(err)
	evidence := adaptive.Evidence{Kind: adaptive.KindAmbiguous}
	switch kind {
	case "rate_or_capacity":
		evidence.Kind = adaptive.KindRateLimited
		evidence.RetryAfter = provenWait(retryAfter, response, now)
	case "request_too_large":
		evidence.Kind = adaptive.KindRequestTooLarge
	case "response_too_large":
		evidence.Kind = adaptive.KindOutputTooLarge
	case "unsupported_response_format":
		evidence.Kind = adaptive.KindUnsupportedOption
		evidence.UnsupportedOption = adaptive.OptionResponseFormat
	}
	return evidence
}

// sanitizedKind extracts the adapter's sanitized classification and its
// proven Retry-After wait from ANY of the supported adapter error types.
// Unknown errors are ambiguous and never become evidence.
func sanitizedKind(err error) (kind string, retryAfter time.Duration) {
	var openAIErr *openaicompat.Error
	if errors.As(err, &openAIErr) {
		return string(openAIErr.Kind), openAIErr.RetryAfter
	}
	var anthropicErr *anthropiccompat.Error
	if errors.As(err, &anthropicErr) {
		return string(anthropicErr.Kind), anthropicErr.RetryAfter
	}
	var googleErr *googlecompat.Error
	if errors.As(err, &googleErr) {
		return string(googleErr.Kind), googleErr.RetryAfter
	}
	return "", 0
}

// provenWait resolves the wait to learn a cooldown from: the typed error's
// sanitized Retry-After first, then the response metadata's sanitized
// Retry-After, then a future ResetAt converted to a remaining duration.
// It never invents a wait when none is proven.
func provenWait(retryAfter time.Duration, response provider.Response, now time.Time) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	if response.Metadata.RetryAfter > 0 {
		return response.Metadata.RetryAfter
	}
	if response.Metadata.ResetAt.After(now) {
		return response.Metadata.ResetAt.Sub(now)
	}
	return 0
}
