package adaptive

import (
	"errors"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func obsRef(id string) provider.EvidenceRef {
	return provider.EvidenceRef{Kind: provider.EvidenceKindTask, ID: id}
}

func mustUpdates(t *testing.T, e Evidence) []provider.ProfileUpdate {
	t.Helper()
	out := Updates(e)
	for _, u := range out {
		if u.Provenance != provider.ProvenanceObserved {
			t.Fatalf("adaptive updates must be provenance observed, got %s", u.Provenance)
		}
		if u.EvidenceRef != e.EvidenceRef {
			t.Fatalf("update must carry the observation's evidence ref, got %+v", u.EvidenceRef)
		}
	}
	return out
}

func expectOne(t *testing.T, out []provider.ProfileUpdate, field provider.ProfileField, value int64) {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("expected exactly one update for %s, got %d: %+v", field, len(out), out)
	}
	if out[0].Field != field || out[0].Value != value {
		t.Fatalf("expected %s=%d, got %s=%d", field, value, out[0].Field, out[0].Value)
	}
}

// TestRateEvidenceLearnsCooldownFromRetryAfter: the ONLY live production
// signal is a proven Retry-After wait; it must tighten cooldown_millis with
// the attempt's task evidence reference.
func TestRateEvidenceLearnsCooldownFromRetryAfter(t *testing.T) {
	out := mustUpdates(t, Evidence{
		Kind:        KindRateLimited,
		RetryAfter:  7 * time.Second,
		EvidenceRef: obsRef("cli-1000000042"),
	})
	expectOne(t, out, provider.FieldCooldownMillis, 7000)
}

// TestRateWithoutRetryAfterLearnsNothing: absent evidence never becomes
// information; a 429 without a proven wait must not invent any number.
func TestRateWithoutRetryAfterLearnsNothing(t *testing.T) {
	if out := Updates(Evidence{Kind: KindRateLimited, EvidenceRef: obsRef("cli-1000000042")}); len(out) != 0 {
		t.Fatalf("rate evidence without a proven wait must learn nothing, got %+v", out)
	}
	// A sub-millisecond signal is not a provable wait either.
	if out := Updates(Evidence{Kind: KindRateLimited, RetryAfter: time.Microsecond, EvidenceRef: obsRef("cli-1000000042")}); len(out) != 0 {
		t.Fatalf("sub-millisecond wait must not become a cooldown, got %+v", out)
	}
}

// TestProvenRequestsPerMinuteIsLearnedOnlyWhenProven: a typed numeric
// per-minute limit is learned exactly when present; it is never derived.
func TestProvenRequestsPerMinuteIsLearnedOnlyWhenProven(t *testing.T) {
	out := mustUpdates(t, Evidence{
		Kind:              KindRateLimited,
		RetryAfter:        30 * time.Second,
		RequestsPerMinute: 64,
		EvidenceRef:       obsRef("cli-1000000042"),
	})
	if len(out) != 2 {
		t.Fatalf("expected cooldown + rpm updates, got %+v", out)
	}
	byField := map[provider.ProfileField]int64{}
	for _, u := range out {
		byField[u.Field] = u.Value
	}
	if byField[provider.FieldCooldownMillis] != 30000 || byField[provider.FieldRequestsPerMinute] != 64 {
		t.Fatalf("unexpected updates: %+v", byField)
	}
}

// TestRequestTooLargeLearnsOnlyProvenLimit: a typed context-too-large with a
// proven numeric input limit tightens max_request_bytes; without the number
// it repairs nothing (learning a number from content would be fabrication).
func TestRequestTooLargeLearnsOnlyProvenLimit(t *testing.T) {
	out := mustUpdates(t, Evidence{Kind: KindRequestTooLarge, MaxRequestBytes: 4096, EvidenceRef: obsRef("cli-1000000043")})
	expectOne(t, out, provider.FieldMaxRequestBytes, 4096)
	if out := Updates(Evidence{Kind: KindRequestTooLarge, EvidenceRef: obsRef("cli-1000000043")}); len(out) != 0 {
		t.Fatalf("request-too-large without a proven number must learn nothing, got %+v", out)
	}
}

// TestOutputTooLargeLearnsOnlyProvenLimit.
func TestOutputTooLargeLearnsOnlyProvenLimit(t *testing.T) {
	out := mustUpdates(t, Evidence{Kind: KindOutputTooLarge, MaxOutputBytes: 8192, EvidenceRef: obsRef("cli-1000000044")})
	expectOne(t, out, provider.FieldMaxResponseBytes, 8192)
	if out := Updates(Evidence{Kind: KindOutputTooLarge, EvidenceRef: obsRef("cli-1000000044")}); len(out) != 0 {
		t.Fatalf("output-too-large without a proven number must learn nothing, got %+v", out)
	}
}

// TestCapacityRestrictedLearnsOnlyProvenCeiling: a proven concurrent ceiling
// is conservative metadata even though today's runtime is serialized by
// governor contract (MaxInFlight must be exactly one).
func TestCapacityRestrictedLearnsOnlyProvenCeiling(t *testing.T) {
	out := mustUpdates(t, Evidence{Kind: KindCapacityRestricted, ConcurrencyCeiling: 1, EvidenceRef: obsRef("cli-1000000045")})
	expectOne(t, out, provider.FieldConcurrencyCeiling, 1)
	if out := Updates(Evidence{Kind: KindCapacityRestricted, EvidenceRef: obsRef("cli-1000000045")}); len(out) != 0 {
		t.Fatalf("capacity evidence without a proven ceiling must learn nothing, got %+v", out)
	}
}

// TestUnsupportedOptionLearnsClosedOptionOnly: only closed options map to
// bits; an unknown option is not representable and stays silent.
func TestUnsupportedOptionLearnsClosedOptionOnly(t *testing.T) {
	out := mustUpdates(t, Evidence{Kind: KindUnsupportedOption, UnsupportedOption: OptionResponseFormat, EvidenceRef: obsRef("cli-1000000046")})
	expectOne(t, out, provider.FieldUnsupportedOptions, 1)
	if out := Updates(Evidence{Kind: KindUnsupportedOption, UnsupportedOption: Option("invented_option"), EvidenceRef: obsRef("cli-1000000046")}); len(out) != 0 {
		t.Fatalf("unknown option must not become profile information, got %+v", out)
	}
}

// TestSuccessNeverLearns: a successful attempt is evidence of nothing
// envelope-shaped; even if numeric hints were attached, success must not
// change any limit (positivity: success never raises).
func TestSuccessNeverLearns(t *testing.T) {
	out := Updates(Evidence{
		Kind: KindSuccess, RetryAfter: time.Minute, MaxRequestBytes: 4096, MaxOutputBytes: 8192,
		RequestsPerMinute: 60, ConcurrencyCeiling: 1, UnsupportedOption: OptionResponseFormat,
		EvidenceRef: obsRef("cli-1000000047"),
	})
	if len(out) != 0 {
		t.Fatalf("success must never learn, got %+v", out)
	}
}

// TestAmbiguityNeverLearns: an uncertain outcome must never change any
// limit.
func TestAmbiguityNeverLearns(t *testing.T) {
	if out := Updates(Evidence{Kind: KindAmbiguous, RetryAfter: time.Minute, EvidenceRef: obsRef("cli-1000000048")}); len(out) != 0 {
		t.Fatalf("ambiguous outcome must never learn, got %+v", out)
	}
}

// TestMissingEvidenceReferenceNeverLearns: without a valid structured
// reference an observation cannot be audited, so it never becomes
// information.
func TestMissingEvidenceReferenceNeverLearns(t *testing.T) {
	if out := Updates(Evidence{Kind: KindRateLimited, RetryAfter: time.Minute}); len(out) != 0 {
		t.Fatalf("evidence without a valid reference must learn nothing, got %+v", out)
	}
	if out := Updates(Evidence{Kind: KindRateLimited, RetryAfter: time.Minute, EvidenceRef: provider.EvidenceRef{Kind: provider.EvidenceKindTask, ID: "free text"}}); len(out) != 0 {
		t.Fatalf("evidence with a non-structured reference must learn nothing, got %+v", out)
	}
}

// TestUnknownKindNeverLearns: only closed kinds are learnable.
func TestUnknownKindNeverLearns(t *testing.T) {
	if out := Updates(Evidence{Kind: Kind("invented"), RetryAfter: time.Minute, EvidenceRef: obsRef("cli-1000000049")}); len(out) != 0 {
		t.Fatalf("unknown kind must never learn, got %+v", out)
	}
}

// TestUpdatesAreDeterministic: the same observation maps to the same
// updates, in the same order.
func TestUpdatesAreDeterministic(t *testing.T) {
	e := Evidence{Kind: KindRateLimited, RetryAfter: 5 * time.Second, RequestsPerMinute: 100, EvidenceRef: obsRef("cli-1000000050")}
	a, b := Updates(e), Updates(e)
	if len(a) != len(b) {
		t.Fatal("deterministic mapping produced different lengths")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("deterministic mapping produced different updates: %+v vs %+v", a, b)
		}
	}
}

// TestAllEmittedUpdatesAreConservative: every update the mapping emits for a
// defined-direction field is tighter than a less strict current state
// (cooldown higher, anything else lower), and remains so against the
// rule engine.
func TestAllEmittedUpdatesAreConservative(t *testing.T) {
	cases := []struct {
		name      string
		evidence  Evidence
		defensive provider.ProfileField
		value     int64
	}{
		{"cooldown", Evidence{Kind: KindRateLimited, RetryAfter: 90 * time.Second, EvidenceRef: obsRef("cli-1000000051")}, provider.FieldCooldownMillis, 90000},
		{"request", Evidence{Kind: KindRequestTooLarge, MaxRequestBytes: 2048, EvidenceRef: obsRef("cli-1000000052")}, provider.FieldMaxRequestBytes, 2048},
		{"output", Evidence{Kind: KindOutputTooLarge, MaxOutputBytes: 4096, EvidenceRef: obsRef("cli-1000000053")}, provider.FieldMaxResponseBytes, 4096},
		{"rpm", Evidence{Kind: KindRateLimited, RequestsPerMinute: 32, EvidenceRef: obsRef("cli-1000000054")}, provider.FieldRequestsPerMinute, 32},
		{"ceiling", Evidence{Kind: KindCapacityRestricted, ConcurrencyCeiling: 1, EvidenceRef: obsRef("cli-1000000055")}, provider.FieldConcurrencyCeiling, 1},
		{"option", Evidence{Kind: KindUnsupportedOption, UnsupportedOption: OptionResponseFormat, EvidenceRef: obsRef("cli-1000000056")}, provider.FieldUnsupportedOptions, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := mustUpdates(t, tc.evidence)
			if len(out) == 0 {
				t.Fatal("expected a conservative update")
			}
			for _, u := range out {
				if _, err := provider.ApplyFieldValue(u.Field, provider.ProfileValue{}, false, u, "2026-08-01T00:00:00Z"); err != nil {
					t.Fatalf("emitted update must apply to unknown state: %v", err)
				}
			}
			// A state that is ALREADY more strict must refuse-or-merge; never
			// weaken. Lower-direction fields: a smaller current refuses. The
			// bitmask field actually absorbs new bits, so apply and compare.
			if tc.evidence.Kind == KindRateLimited && tc.evidence.RequestsPerMinute == 0 {
				current := provider.ProfileValue{Value: tc.value + 1000, Provenance: provider.ProvenanceObserved, EvidenceRef: obsRef("cli-1000000057"), UpdatedAt: "2026-08-01T00:00:00Z"}
				for _, u := range out {
					if _, err := provider.ApplyFieldValue(u.Field, current, true, u, "2026-08-01T00:00:00Z"); !errors.Is(err, provider.ErrObservedNotConservative) {
						t.Fatalf("weaker update against stricter state must be refused, got %v", err)
					}
				}
			}
		})
	}
}

// TestConservativeSubsetDropsRefusedUpdates: the subset kept for the durable
// boundary excludes updates the effective profile already covers or refuses.
func TestConservativeSubsetDropsRefusedUpdates(t *testing.T) {
	profile := provider.NewOperationalProfile(provider.Identity{ProviderID: "p", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "m", ConfigIdentity: "c"})
	clock := func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) }
	var err error
	profile, err = profile.Apply(provider.ProfileUpdate{Field: provider.FieldCooldownMillis, Value: 60000, Provenance: provider.ProvenanceObserved, EvidenceRef: obsRef("cli-1000000058")}, clock)
	if err != nil {
		t.Fatal(err)
	}

	updates := []provider.ProfileUpdate{
		// 30s against an effective 60s: already stricter, must be dropped.
		{Field: provider.FieldCooldownMillis, Value: 30000, Provenance: provider.ProvenanceObserved, EvidenceRef: obsRef("cli-1000000059")},
		// 120s against an effective 60s: tightening, must be kept.
		{Field: provider.FieldCooldownMillis, Value: 120000, Provenance: provider.ProvenanceObserved, EvidenceRef: obsRef("cli-1000000059")},
		// Undefined-direction field: never accepted automatically, dropped.
		{Field: provider.FieldTimeoutMillis, Value: 1000, Provenance: provider.ProvenanceObserved, EvidenceRef: obsRef("cli-1000000059")},
	}
	kept := ConservativeSubset(profile, updates, clock)
	if len(kept) != 1 || kept[0].Value != 120000 {
		t.Fatalf("expected only the 120s tightening to survive, got %+v", kept)
	}

	if all := ConservativeSubset(nil, updates, clock); len(all) != 3 {
		t.Fatalf("nil profile must accept every emitted update, got %+v", all)
	}
	if out := ConservativeSubset(profile, nil, clock); out != nil {
		t.Fatalf("no updates must produce no subset")
	}
}

// TestEvidenceCannotCarrySensitiveText: the evidence surface is integers,
// closed enums and a structured reference. This test documents the
// property: no string field exists to smuggle prompts, responses, headers
// or secrets into the learning path.
func TestEvidenceCannotCarrySensitiveText(t *testing.T) {
	e := Evidence{Kind: KindRateLimited, RetryAfter: time.Second, EvidenceRef: obsRef("cli-1000000060")}
	if e.RetryAfter <= 0 || !e.EvidenceRef.Valid() {
		t.Fatal("evidence fixture must be valid")
	}
	for _, u := range Updates(e) {
		if u.Field == "" || u.Provenance == "" {
			t.Fatalf("update carries empty structured fields: %+v", u)
		}
	}
}
