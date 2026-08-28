package provider

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testIdentity(configIdentity string, model string, family ProtocolFamily) Identity {
	return Identity{
		ProviderID:     "provider-a",
		ProtocolFamily: family,
		Model:          model,
		ConfigIdentity: configIdentity,
		ProfileVersion: "v1",
		AdapterVersion: "test",
	}
}

func fixedClock() func() time.Time {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func TestProfileKeysSeparateModelsConfigsAndFamilies(t *testing.T) {
	baseConfig := "provider.Config{ProviderID:\"p\" Endpoint:\"https://e.invalid/v1\" ProtocolFamily:\"openai_compatible\"}"
	otherConfig := "provider.Config{ProviderID:\"p\" Endpoint:\"https://e.invalid/v1\" ProtocolFamily:\"openai_compatible\" ConfigVersion:\"v2\"}"

	keyModelA := OperationalProfileKey(baseConfig, "model-a", FamilyOpenAICompatible)
	keyModelB := OperationalProfileKey(baseConfig, "model-b", FamilyOpenAICompatible)
	if keyModelA == keyModelB {
		t.Fatalf("different models must not share a profile key")
	}
	keyOtherConfig := OperationalProfileKey(otherConfig, "model-a", FamilyOpenAICompatible)
	if keyModelA == keyOtherConfig {
		t.Fatalf("different config identities must not share a profile key")
	}
	keyGoogle := OperationalProfileKey(baseConfig, "model-a", FamilyGoogleCompatible)
	if keyModelA == keyGoogle {
		t.Fatalf("different protocol families must not share a profile key")
	}
}

func TestNewProfileStartsUnknownAndConservative(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	for _, field := range AllProfileFields {
		if value := profile.Effective(field); value.Known() {
			t.Fatalf("fresh profile field %s must be unknown, got %+v", field, value)
		}
	}
	if len(profile.Values) != 0 {
		t.Fatalf("fresh profile must carry no values")
	}
}

// TestSafetyDirectionsExplicit covers the #91-review requirement: the
// conservative direction is defined PER FIELD and drives automatic
// observations.
func TestSafetyDirectionsExplicit(t *testing.T) {
	lowerFields := []ProfileField{FieldMaxRequestBytes, FieldMaxResponseBytes, FieldRequestsPerMinute, FieldConcurrencyCeiling}
	for _, field := range lowerFields {
		if SafetyDirection(field) != DirectionLowerIsConservative {
			t.Fatalf("field %s must be lower-is-conservative", field)
		}
	}
	if SafetyDirection(FieldCooldownMillis) != DirectionHigherIsConservative {
		t.Fatalf("cooldown_millis must be higher-is-conservative")
	}
	if SafetyDirection(FieldTimeoutMillis) != DirectionNoAutomatic {
		t.Fatalf("timeout_millis must have no automatic direction")
	}
}

// TestObservedEvidenceTightensButNeverWeakens proves the shared rule engine:
// observed evidence may only move a field toward its conservative
// direction; anything else is refused with ErrObservedNotConservative.
func TestObservedEvidenceTightensButNeverWeakens(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	profile, err = profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 8000 || v.Provenance != ProvenanceConfigured {
		t.Fatalf("configured not applied: %+v", v)
	}

	// Restrictive observed evidence tightens (lower-is-conservative).
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000001"},
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != ProvenanceObserved || v.EvidenceRef != (EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000001"}) {
		t.Fatalf("tightening not applied: %+v", v)
	}

	// Raising or equal observations are REFUSED.
	for _, value := range []int64{4096, 2048} {
		if _, err = profile.Apply(ProfileUpdate{
			Field: FieldMaxRequestBytes, Value: value, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000002"},
		}, fixedClock()); !errors.Is(err, ErrObservedNotConservative) {
			t.Fatalf("observed value %d must be refused as not conservative, got %v", value, err)
		}
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 2048 {
		t.Fatalf("effective value changed after refused observation: %+v", v)
	}
}

// TestCooldownHigherIsConservative covers the cooldown direction: a longer
// Retry-After observation is accepted as tightening; a shorter one never
// weakens the profile automatically.
func TestCooldownHigherIsConservative(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	profile, err = profile.ApplyConfigured(FieldCooldownMillis, 30000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	// Retry-After 60s is MORE conservative than the effective 30s: accepted.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldCooldownMillis, Value: 60000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000042"},
	}, fixedClock())
	if err != nil {
		t.Fatalf("cooldown 30s -> observed 60s must be accepted as tightening: %v", err)
	}
	if v := profile.Effective(FieldCooldownMillis); v.Value != 60000 || v.Provenance != ProvenanceObserved {
		t.Fatalf("cooldown tightening not applied: %+v", v)
	}
	// A SHORTER observation must not automatically weaken the profile.
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldCooldownMillis, Value: 10000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000043"},
	}, fixedClock()); !errors.Is(err, ErrObservedNotConservative) {
		t.Fatalf("cooldown 60s -> observed 10s must be refused, got %v", err)
	}
	if v := profile.Effective(FieldCooldownMillis); v.Value != 60000 {
		t.Fatalf("cooldown weakened without authority: %+v", v)
	}
}

// TestTimeoutHasNoAutomaticDirection: observations are never auto-applied to
// timeout_millis — including as the FIRST value on an unknown field —
// while configured/authoritative values remain representable.
func TestTimeoutHasNoAutomaticDirection(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error

	// unknown timeout -> observed must fail: the undefined direction check
	// precedes the unknown fast-path (#91 review).
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldTimeoutMillis, Value: 30000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000046"},
	}, fixedClock()); !errors.Is(err, ErrNoAutomaticDirection) {
		t.Fatalf("unknown timeout -> observed must fail with ErrNoAutomaticDirection, got %v", err)
	}
	if profile.Effective(FieldTimeoutMillis).Known() {
		t.Fatalf("refused observation must not produce an effective value")
	}

	profile, err = profile.ApplyConfigured(FieldTimeoutMillis, 60000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldTimeoutMillis, Value: 30000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000045"},
	}, fixedClock()); !errors.Is(err, ErrNoAutomaticDirection) {
		t.Fatalf("timeout observation must be refused without an automatic direction, got %v", err)
	}
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldTimeoutMillis, Value: 120000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000045"},
	}, fixedClock()); !errors.Is(err, ErrNoAutomaticDirection) {
		t.Fatalf("timeout observation (any direction) must be refused, got %v", err)
	}
	// Authoritative remains representable through the typed path.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldTimeoutMillis, Value: 90000, Provenance: ProvenanceAuthoritative, EvidenceRef: EvidenceRef{Kind: EvidenceKindVerification, ID: "verif-000001"},
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldTimeoutMillis); v.Value != 90000 || v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative timeout not applied: %+v", v)
	}
}

// TestSuccessNeverRaises proposes non-conservative observations on every
// automated field: none of them may change the effective value.
func TestSuccessNeverRaises(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	seeded := []struct {
		field ProfileField
		value int64
	}{
		{FieldMaxRequestBytes, 8000},
		{FieldMaxResponseBytes, 16000},
		{FieldRequestsPerMinute, 20},
		{FieldConcurrencyCeiling, 2},
		{FieldCooldownMillis, 30000},
	}
	for _, entry := range seeded {
		profile, err = profile.ApplyConfigured(entry.field, entry.value, fixedClock())
		if err != nil {
			t.Fatal(err)
		}
	}
	proposals := []struct {
		field ProfileField
		value int64
	}{
		{FieldMaxRequestBytes, 8001},   // raise request bound
		{FieldMaxResponseBytes, 16001}, // raise output bound
		{FieldRequestsPerMinute, 21},   // raise rate envelope
		{FieldConcurrencyCeiling, 3},   // raise concurrency
		{FieldCooldownMillis, 29999},   // shorten cooldown (weaken)
	}
	for _, proposal := range proposals {
		if _, err := profile.Apply(ProfileUpdate{
			Field: proposal.field, Value: proposal.value, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000044"},
		}, fixedClock()); !errors.Is(err, ErrObservedNotConservative) {
			t.Fatalf("field %s: non-conservative observation must be refused, got %v", proposal.field, err)
		}
	}
	for _, entry := range seeded {
		if v := profile.Effective(entry.field); v.Value != entry.value {
			t.Fatalf("field %s effective value changed by refused observation: %+v", entry.field, v)
		}
	}
}

func TestUnknownCanReceiveAllThreeProvenancesThroughTheirPaths(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error

	profile, err = profile.Apply(ProfileUpdate{Field: FieldRequestsPerMinute, Value: 10, Provenance: ProvenanceConfigured}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldRequestsPerMinute); v.Provenance != ProvenanceConfigured {
		t.Fatalf("configured provenance lost")
	}

	profile, err = profile.Apply(ProfileUpdate{Field: FieldCooldownMillis, Value: 5000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000010"}}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldCooldownMillis); v.Provenance != ProvenanceObserved || v.EvidenceRef != (EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000010"}) {
		t.Fatalf("observed provenance lost")
	}

	profile, err = profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: 30000, Provenance: ProvenanceAuthoritative, EvidenceRef: EvidenceRef{Kind: EvidenceKindVerification, ID: "verif-000002"}}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldTimeoutMillis); v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative provenance lost")
	}

	if ProvenanceUnknown == ProvenanceConfigured || ProvenanceConfigured == ProvenanceObserved || ProvenanceObserved == ProvenanceAuthoritative {
		t.Fatalf("provenance semantics are not distinct")
	}
	for _, input := range []string{"configured", "observed", "authoritative"} {
		parsed, err := ParseProvenance(input)
		if err != nil || string(parsed) != input {
			t.Fatalf("provenance %q not parseable/distinguishable", input)
		}
	}
	if _, err := ParseProvenance(""); err == nil {
		t.Fatalf("empty provenance must fail closed")
	}
	if _, err := ParseProvenance("unknown"); err == nil {
		t.Fatalf("unknown provenance must never be writable")
	}
	fresh := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	if fresh.Effective(FieldTimeoutMillis).Known() {
		t.Fatalf("unknown state must not be fabricated into known")
	}
}

func TestObservedAndAuthoritativeRequireEvidenceReferences(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	for _, provenance := range []Provenance{ProvenanceObserved, ProvenanceAuthoritative} {
		if _, err := profile.Apply(ProfileUpdate{Field: FieldCooldownMillis, Value: 1000, Provenance: provenance}, fixedClock()); err == nil {
			t.Fatalf("%s without evidence reference must fail closed", provenance)
		}
	}
}

func TestConfiguredReplayCannotUndoObservedTightening(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	profile, err = profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000001"},
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	// Replaying the same unchanged configured bound must not undo the
	// observed tightening; it is signalled as a benign replay.
	replayed, err := profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock())
	if !errors.Is(err, ErrProfileReplayUndo) {
		t.Fatalf("configured replay must be refused with ErrProfileReplayUndo, got %v", err)
	}
	if replayed != nil {
		t.Fatalf("refused replay must return nil state")
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != ProvenanceObserved {
		t.Fatalf("configured replay undid the observed tightening: %+v", v)
	}
	// Authoritative evidence CAN move the value (explicitly typed path).
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 16000, Provenance: ProvenanceAuthoritative, EvidenceRef: EvidenceRef{Kind: EvidenceKindVerification, ID: "verif-000003"},
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 16000 || v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative update not applied: %+v", v)
	}
}

// TestNoInventedProfileCapDoesNotRejectValidProviderConfig is the #91-review
// regression: persisting operational metadata must never reject a
// configuration that the #79 provider contract considers valid only because
// of a newly invented profile cap. The profile defines no admission policy.
func TestNoInventedProfileCapDoesNotRejectValidProviderConfig(t *testing.T) {
	// The provider contract accepts large capability bounds (non-negative);
	// the operational profile must represent and persist them without a
	// profile-only ceiling vetoing the run's own configuration.
	config := Config{
		ProviderID:      "big-provider",
		ProtocolFamily:  FamilyOpenAICompatible,
		BaseURL:         "http://127.0.0.1:1/v1",
		Model:           "model-big",
		AuthRequirement: AuthNone,
		Profile: CapabilityProfile{
			ProfileVersion:   "v1",
			Capabilities:     testProfileCapabilities(),
			RouteSafety:      SafeRouteSafety(),
			MaxRequestBytes:  1 << 30, // 1 GiB: valid for the provider contract
			MaxResponseBytes: 2 << 30,
		},
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("provider contract must accept this configuration: %v", err)
	}
	resolved, err := registry.Resolve("big-provider", RequiredCapabilities(), SafeRouteSafety())
	if err != nil {
		t.Fatalf("provider contract must resolve this configuration: %v", err)
	}

	// The operational profile mirrors the same configured values without
	// inventing a ceiling of its own.
	profile := NewOperationalProfile(IdentityFromResolved(*resolved, "test"))
	profile, err = profile.ApplyConfigured(FieldMaxRequestBytes, int64(resolved.Profile.MaxRequestBytes), fixedClock())
	if err != nil {
		t.Fatalf("profile must accept the provider-valid bound without an invented cap: %v", err)
	}
	profile, err = profile.ApplyConfigured(FieldMaxResponseBytes, int64(resolved.Profile.MaxResponseBytes), fixedClock())
	if err != nil {
		t.Fatalf("profile must accept the provider-valid bound without an invented cap: %v", err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 1<<30 || v.Provenance != ProvenanceConfigured {
		t.Fatalf("provider-valid bound not represented: %+v", v)
	}
}

func testProfileCapabilities() Capabilities {
	return Capabilities{
		CapabilityTextTurn:         true,
		CapabilityRunsteadProtocol: true,
	}
}

// TestZeroValueNeverPersisted covers the single zero/unknown representation:
// value <= 0 is refused for configured, observed and authoritative.
func TestZeroValueNeverPersisted(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	cases := []ProfileUpdate{
		{Field: FieldMaxRequestBytes, Value: 0, Provenance: ProvenanceConfigured},
		{Field: FieldMaxRequestBytes, Value: -1, Provenance: ProvenanceConfigured},
		{Field: FieldCooldownMillis, Value: 0, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000001"}},
		{Field: FieldTimeoutMillis, Value: 0, Provenance: ProvenanceAuthoritative, EvidenceRef: EvidenceRef{Kind: EvidenceKindVerification, ID: "verif-000001"}},
	}
	for _, update := range cases {
		if _, err := profile.Apply(update, fixedClock()); err == nil {
			t.Fatalf("update %+v with non-positive value must fail closed", update)
		}
	}
	if _, err := profile.Apply(ProfileUpdate{Field: "bogus_field", Value: 1, Provenance: ProvenanceConfigured}, fixedClock()); err == nil {
		t.Fatalf("unknown field must fail closed")
	}
	if _, err := profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: 1, Provenance: "unknown"}, fixedClock()); err == nil {
		t.Fatalf("unknown provenance must fail closed")
	}
}

func TestApplyIsPureAndProfileCarriesNoExecutionAuthority(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	before := profile.Sanitized()
	next, err := profile.Apply(ProfileUpdate{
		Field: FieldCooldownMillis, Value: 1000, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000020"},
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Sanitized() != before {
		t.Fatalf("Apply mutated the receiver")
	}
	_ = next

	// The update/profile surfaces have no provider/model/fallback/retry
	// authority fields.
	updateType := reflect.TypeOf(ProfileUpdate{})
	for _, field := range []string{"ProviderID", "ProtocolFamily", "Model", "Fallback", "Retry", "MaxRetries", "Client", "Endpoint"} {
		if _, exists := updateType.FieldByName(field); exists {
			t.Fatalf("ProfileUpdate must not carry %q authority", field)
		}
	}
	profileType := reflect.TypeOf(OperationalProfile{})
	for _, field := range []string{"Client", "Retry", "Context", "Fallback"} {
		if _, exists := profileType.FieldByName(field); exists {
			t.Fatalf("OperationalProfile must not carry %q authority", field)
		}
	}
}

func TestUnsupportedProfileVersionFailsClosed(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	profile.ProfileVersion = "v99"
	if _, err := profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: 1, Provenance: ProvenanceConfigured}, fixedClock()); err == nil {
		t.Fatalf("unsupported profile version must fail closed")
	}
}

func TestInvalidUpdatesFailClosed(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	if _, err := profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: -1, Provenance: ProvenanceConfigured}, fixedClock()); err == nil {
		t.Fatalf("negative value must fail closed")
	}
	if _, err := profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: 0, Provenance: ProvenanceConfigured}, fixedClock()); err == nil {
		t.Fatalf("zero value must fail closed")
	}
}

// TestEvidenceRefIsStructuredNotFreeText is the #91-review regression: an
// evidence reference is a structured kind:id identifier whose ID must match
// the CLOSED format Runstead actually produces — not merely "no spaces".
// Private content, even as a single token such as "privatePromptBody" or
// "sensitive-response-content", has no representation it can smuggle
// through, and error messages never reproduce the invalid candidate.
func TestEvidenceRefIsStructuredNotFreeText(t *testing.T) {
	ref, err := ParseEvidenceRef("evidence:obs-000001")
	if err != nil {
		t.Fatalf("valid reference must parse: %v", err)
	}
	if ref.Kind != EvidenceKindEvidence || ref.ID != "obs-000001" || ref.String() != "evidence:obs-000001" {
		t.Fatalf("parsed reference wrong: %+v", ref)
	}
	if parsed, err := ParseEvidenceRef(""); err != nil || parsed.Kind != "" {
		t.Fatalf("empty reference is the ABSENT state, got %+v err %v", parsed, err)
	}
	for _, kind := range []EvidenceKind{EvidenceKindEvidence, EvidenceKindExecution, EvidenceKindVerification, EvidenceKindTask} {
		pattern := closedEvidenceIDPatterns[kind]
		if pattern == nil {
			t.Fatalf("kind %s has no closed pattern", kind)
		}
	}

	// Free text with spaces AND single tokens without spaces are refused:
	// the closed per-kind formats only accept the ids Runstead produces.
	for _, candidate := range []string{
		"the quick brown fox jumps over the lazy dog",
		"response body content that must never be a reference",
		"prompt: fix the calculator and explain",
		"Bearer sk-secret-token-value",
		"authorization header value",
		"evidence:with spaces inside the id",
		"privatePromptBody",
		"sensitive-response-content",
		"thequickbrownfoxjumpsoverthelazydogandmoreprivatetext",
		"credentials-in-a-reference",
		"obs-retry-after",
		"obs-00001",
		"ober-000001",
		"obs-0000010",
		"obs-NNNNNN",
	} {
		if _, err := ParseEvidenceRef(candidate); err == nil {
			t.Fatalf("candidate %q must fail ParseEvidenceRef", candidate)
		}
		if _, err := ParseEvidenceRef("evidence:" + candidate); err == nil {
			t.Fatalf("candidate id %q must fail ParseEvidenceRef", candidate)
		}
		// Errors never reproduce the invalid candidate.
		if _, err := ParseEvidenceRef(candidate); err != nil && strings.Contains(err.Error(), candidate) {
			t.Fatalf("error must not echo the invalid candidate: %v", err)
		}
	}

	// Updates with a non-structured reference are invalid before the rules.
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved,
		EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "free text id"},
	}, fixedClock()); err == nil {
		t.Fatalf("update with free-text evidence id must fail closed")
	}
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved,
		EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "privatePromptBody"},
	}, fixedClock()); err == nil {
		t.Fatalf("update with private single-token id must fail closed")
	}
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved,
		EvidenceRef: EvidenceRef{Kind: "bogus", ID: "obs-000001"},
	}, fixedClock()); err == nil {
		t.Fatalf("update with unknown evidence kind must fail closed")
	}
	// Typed constructors + closed formats are the only valid path.
	if !EvidenceEvidenceRef("obs-000001").Valid() || !ExecutionEvidenceRef("exec-000001").Valid() ||
		!VerificationEvidenceRef("verif-000001").Valid() || !TaskEvidenceRef("cli-1787864000000000000").Valid() {
		t.Fatalf("typed constructors with real Runstead ids must be valid")
	}
	if EvidenceEvidenceRef("privatePromptBody").Valid() {
		t.Fatalf("typed constructor must still refuse private non-format ids")
	}
}

// TestEvidenceRefAcceptsCanonicalCounterIds is the #91-review regression:
// %06d is a MINIMUM width, so counters above 999999 legitimately produce
// ids with MORE than six digits (obs-1000000) and those must be accepted,
// while non-canonical forms (too short, or extraneous leading zeros) must
// be refused.
func TestEvidenceRefAcceptsCanonicalCounterIds(t *testing.T) {
	validIDs := []EvidenceRef{
		EvidenceEvidenceRef("obs-000001"),
		EvidenceEvidenceRef("obs-1000000"),
		EvidenceEvidenceRef("obs-1234567890"),
		ExecutionEvidenceRef("exec-000001"),
		ExecutionEvidenceRef("exec-1000000"),
		VerificationEvidenceRef("verif-000001"),
		VerificationEvidenceRef("verif-1000000"),
		TaskEvidenceRef("cli-1787864000000000000"),
	}
	for _, ref := range validIDs {
		if !ref.Valid() {
			t.Fatalf("canonical counter id %s must be valid", ref.String())
		}
		if parsed, err := ParseEvidenceRef(ref.String()); err != nil || !parsed.Valid() {
			t.Fatalf("canonical counter id %s must parse: %v", ref.String(), err)
		}
	}
	nonCanonical := []EvidenceRef{
		EvidenceEvidenceRef("obs-00001"),     // too short for the padding minimum
		EvidenceEvidenceRef("obs-0000010"),   // extraneous leading zero (7 digits)
		EvidenceEvidenceRef("obs-000000000"), // multiple extraneous zeros
		ExecutionEvidenceRef("exec-99999"),
		VerificationEvidenceRef("verif-"),
		TaskEvidenceRef("cli-0000000000"), // leading zeros are not nanos
	}
	for _, ref := range nonCanonical {
		if ref.Valid() {
			t.Fatalf("non-canonical id %s must be invalid", ref.String())
		}
	}
}

// TestConfiguredUpdateMustNotCarryEvidenceRef is the #91-review regression:
// configured provenance is operator declaration, never evidence-derived; an
// evidence reference on a configured update must fail before the rules (it
// would otherwise be persisted and only discovered at load).
func TestConfiguredUpdateMustNotCarryEvidenceRef(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	update := ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 8000, Provenance: ProvenanceConfigured,
		EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000001"},
	}
	if _, err := profile.Apply(update, fixedClock()); err == nil {
		t.Fatalf("configured update with an evidence reference must fail closed")
	}
	if profile.Effective(FieldMaxRequestBytes).Known() {
		t.Fatalf("refused configured update must not produce an effective value")
	}
	// The typed configured path stays clean.
	if _, err := profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock()); err != nil {
		t.Fatalf("clean configured update must apply: %v", err)
	}
}

// TestUnsupportedOptionsBitmaskAccumulatesMonotonically: unsupported_options
// is an accumulated bitmask. Observations may only ADD proven bits
// (merge), repeat observations are refused, configured updates may only
// superset an observed/authoritative mask, and only authoritative input can
// relax it (#93).
func TestUnsupportedOptionsBitmaskAccumulatesMonotonically(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error

	// unknown -> observed single bit accepted.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldUnsupportedOptions, Value: 0x1, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000051"},
	}, fixedClock())
	if err != nil {
		t.Fatalf("unknown unsupported_options -> observed bit 0x1 must apply: %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x1 || v.Provenance != ProvenanceObserved {
		t.Fatalf("bit not applied: %+v", v)
	}

	// A later observation adds only its NEW bit: value accumulates (OR).
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldUnsupportedOptions, Value: 0x4, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000052"},
	}, fixedClock())
	if err != nil {
		t.Fatalf("observed new bit must merge onto the mask: %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x5 {
		t.Fatalf("mask must accumulate to 0x5, got %+v", v)
	}

	// Repeating an already-proven bit is not new information: refused.
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldUnsupportedOptions, Value: 0x1, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000053"},
	}, fixedClock()); !errors.Is(err, ErrObservedNotConservative) {
		t.Fatalf("repeated observed bit must be refused, got %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x5 {
		t.Fatalf("refused repeat must not change the mask: %+v", v)
	}

	// An observation that would clear a proven bit NEVER clears it: the
	// merged mask keeps all previously proven bits.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldUnsupportedOptions, Value: 0x2, Provenance: ProvenanceObserved, EvidenceRef: EvidenceRef{Kind: EvidenceKindEvidence, ID: "obs-000054"},
	}, fixedClock())
	if err != nil {
		t.Fatalf("observed new bit must merge: %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x7 {
		t.Fatalf("mask must not lose proven bits: %+v", v)
	}

	// Configured input that would UNDO proven bits is a replay-undo refusal.
	if _, err = profile.ApplyConfigured(FieldUnsupportedOptions, 0x1, fixedClock()); !errors.Is(err, ErrProfileReplayUndo) {
		t.Fatalf("configured mask clearing proven bits must be refused, got %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x7 {
		t.Fatalf("refused configured update must not weaken the mask: %+v", v)
	}

	// Configured input that SUPERSETS the mask replaces it wholesale.
	profile, err = profile.ApplyConfigured(FieldUnsupportedOptions, 0xF, fixedClock())
	if err != nil {
		t.Fatalf("configured superset must replace the mask: %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0xF || v.Provenance != ProvenanceConfigured {
		t.Fatalf("configured superset not applied: %+v", v)
	}

	// Only authoritative input can relax previously proven bits.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldUnsupportedOptions, Value: 0x3, Provenance: ProvenanceAuthoritative, EvidenceRef: EvidenceRef{Kind: EvidenceKindVerification, ID: "verif-000002"},
	}, fixedClock())
	if err != nil {
		t.Fatalf("authoritative relaxation must apply: %v", err)
	}
	if v := profile.Effective(FieldUnsupportedOptions); v.Value != 0x3 || v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative mask not applied: %+v", v)
	}
}
