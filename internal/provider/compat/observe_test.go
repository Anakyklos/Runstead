package compat

import (
	"errors"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/adaptive"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

var observeNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// TestObservationSuccessNeverLearnsEvidence: a successful attempt maps to
// success evidence, which the adaptive mapping refuses to learn from.
func TestObservationSuccessNeverLearnsEvidence(t *testing.T) {
	ev := Observation(provider.Response{}, nil, observeNow)
	if ev.Kind != adaptive.KindSuccess {
		t.Fatalf("nil error must map to success evidence, got %s", ev.Kind)
	}
	if out := adaptive.Updates(ev); len(out) != 0 {
		t.Fatalf("success evidence must never learn, got %+v", out)
	}
}

// TestObservationRateEvidenceAcrossAllFamilies: 429-family typed errors from
// every protocol family map to the same adaptive kind and carry the same
// proven wait (required equivalent behavior across families).
func TestObservationRateEvidenceAcrossAllFamilies(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"openai", &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity, RetryAfter: 42 * time.Second}},
		{"anthropic", &anthropiccompat.Error{Kind: anthropiccompat.ErrorRateCapacity, RetryAfter: 42 * time.Second}},
		{"google", &googlecompat.Error{Kind: googlecompat.ErrorRateCapacity, RetryAfter: 42 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Observation(provider.Response{}, tc.err, observeNow)
			if ev.Kind != adaptive.KindRateLimited || ev.RetryAfter != 42*time.Second {
				t.Fatalf("rate evidence not normalized: %+v", ev)
			}
			out := adaptive.Updates(evidenceWithRef(ev))
			if len(out) != 1 || out[0].Field != provider.FieldCooldownMillis || out[0].Value != 42000 {
				t.Fatalf("rate evidence must learn cooldown 42000ms, got %+v", out)
			}
		})
	}
}

// TestObservationRateWithoutWaitLearnsNothing: a 429 without a proven wait
// is rate evidence that must not invent a cooldown.
func TestObservationRateWithoutWaitLearnsNothing(t *testing.T) {
	ev := Observation(provider.Response{}, &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity}, observeNow)
	if ev.Kind != adaptive.KindRateLimited {
		t.Fatalf("expected rate evidence, got %s", ev.Kind)
	}
	if ev.RetryAfter != 0 {
		t.Fatalf("no wait must stay unknown, got %v", ev.RetryAfter)
	}
	if out := adaptive.Updates(evidenceWithRef(ev)); len(out) != 0 {
		t.Fatalf("rate evidence without a wait must learn nothing, got %+v", out)
	}
}

// TestObservationFallsBackToMetadataWait: when the typed error carries no
// wait, a sanitized metadata Retry-After or a future ResetAt still proves one.
func TestObservationFallsBackToMetadataWait(t *testing.T) {
	err := &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity}
	withMeta := provider.Response{Metadata: provider.ResponseMetadata{RetryAfter: 10 * time.Second}}
	if ev := Observation(withMeta, err, observeNow); ev.RetryAfter != 10*time.Second {
		t.Fatalf("metadata Retry-After must fall back, got %v", ev.RetryAfter)
	}
	withReset := provider.Response{Metadata: provider.ResponseMetadata{ResetAt: observeNow.Add(25 * time.Second)}}
	if ev := Observation(withReset, err, observeNow); ev.RetryAfter != 25*time.Second {
		t.Fatalf("future ResetAt must convert to a wait, got %v", ev.RetryAfter)
	}
	pastReset := provider.Response{Metadata: provider.ResponseMetadata{ResetAt: observeNow.Add(-time.Minute)}}
	if ev := Observation(pastReset, err, observeNow); ev.RetryAfter != 0 {
		t.Fatalf("past ResetAt must stay unknown, got %v", ev.RetryAfter)
	}
}

// TestObservationRequestOutputTooLargeNeverFabricatesNumbers: typed
// too-large evidence keeps its kind, but no adapter proves a numeric limit
// today, so the numeric fields stay zero and nothing is learned (the
// profile contract supports the numeric channel for future proven signals).
func TestObservationRequestOutputTooLargeNeverFabricatesNumbers(t *testing.T) {
	req := Observation(provider.Response{}, &openaicompat.Error{Kind: openaicompat.ErrorRequestTooLarge}, observeNow)
	if req.Kind != adaptive.KindRequestTooLarge || req.MaxRequestBytes != 0 {
		t.Fatalf("request-too-large evidence not normalized: %+v", req)
	}
	if out := adaptive.Updates(evidenceWithRef(req)); len(out) != 0 {
		t.Fatalf("too-large without a proven number must learn nothing, got %+v", out)
	}
	out := Observation(provider.Response{}, &anthropiccompat.Error{Kind: anthropiccompat.ErrorResponseTooLarge}, observeNow)
	if out.Kind != adaptive.KindOutputTooLarge || out.MaxOutputBytes != 0 {
		t.Fatalf("response-too-large evidence not normalized: %+v", out)
	}
}

// TestObservationUnsupportedOptionIsClosed: the typed unsupported-response
// format signal maps to the single closed option bit.
func TestObservationUnsupportedOptionIsClosed(t *testing.T) {
	for _, err := range []error{
		&anthropiccompat.Error{Kind: anthropiccompat.ErrorUnsupportedResponseFormat},
		&googlecompat.Error{Kind: googlecompat.ErrorUnsupportedResponseFormat},
	} {
		ev := Observation(provider.Response{}, err, observeNow)
		if ev.Kind != adaptive.KindUnsupportedOption || ev.UnsupportedOption != adaptive.OptionResponseFormat {
			t.Fatalf("unsupported-response-format evidence not normalized: %+v", ev)
		}
		out := adaptive.Updates(evidenceWithRef(ev))
		if len(out) != 1 || out[0].Field != provider.FieldUnsupportedOptions || out[0].Value != 1 {
			t.Fatalf("must learn the closed option bit, got %+v", out)
		}
	}
}

// TestObservationUnknownErrorsStayAmbiguous: outcomes that carry no
// learnable typed kind (unknown/unwrapped errors) or whose adapter kind has
// no learnable adaptive mapping (cancelled, timeout, auth, upstream
// failures) stay ambiguous and never become evidence.
func TestObservationUnknownErrorsStayAmbiguous(t *testing.T) {
	for _, err := range []error{
		errors.New("generic control-plane failure"),
		fmtWrap("wrapped", &openaicompat.Error{Kind: openaicompat.ErrorTimeout}),
		&openaicompat.Error{Kind: openaicompat.ErrorAuthUnavailable},
		&anthropiccompat.Error{Kind: anthropiccompat.ErrorCancelled},
		&googlecompat.Error{Kind: googlecompat.ErrorUpstreamServerFailure},
	} {
		ev := Observation(provider.Response{}, err, observeNow)
		if ev.Kind != adaptive.KindAmbiguous {
			t.Fatalf("%v must stay ambiguous, got %s", err, ev.Kind)
		}
		if out := adaptive.Updates(evidenceWithRef(ev)); len(out) != 0 {
			t.Fatalf("ambiguous evidence must learn nothing, got %+v", out)
		}
	}
}

// TestObservationCarriesNoSensitiveText: the translation copies no request
// ids, endpoints or free text into the evidence; only the typed kind and
// the sanitized wait survive.
func TestObservationCarriesNoSensitiveText(t *testing.T) {
	err := &openaicompat.Error{Kind: openaicompat.ErrorRateCapacity, RequestID: "req_private", RetryAfter: time.Minute}
	resp := provider.Response{Metadata: provider.ResponseMetadata{RequestID: "req_private_meta", Endpoint: "https://secret.example/v1", RetryAfter: 0}}
	ev := Observation(resp, err, observeNow)
	if ev.RetryAfter != time.Minute {
		t.Fatalf("wait must survive, got %v", ev.RetryAfter)
	}
	str := []string{string(ev.Kind), string(ev.UnsupportedOption), string(ev.EvidenceRef.Kind), ev.EvidenceRef.ID}
	for _, s := range str {
		if s == "req_private" || s == "https://secret.example/v1" || s == "req_private_meta" {
			t.Fatalf("sensitive text leaked into evidence: %q", s)
		}
	}
}

func evidenceWithRef(ev adaptive.Evidence) adaptive.Evidence {
	ev.EvidenceRef = provider.EvidenceRef{Kind: provider.EvidenceKindTask, ID: "cli-1000000042"}
	return ev
}

func fmtWrap(msg string, err error) error {
	return &wrapError{msg: msg, err: err}
}

type wrapError struct {
	msg string
	err error
}

func (w *wrapError) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapError) Unwrap() error { return w.err }
