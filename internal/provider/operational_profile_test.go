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

	// 1. Two models on the same endpoint have independent profiles.
	keyModelA := OperationalProfileKey(baseConfig, "model-a", FamilyOpenAICompatible)
	keyModelB := OperationalProfileKey(baseConfig, "model-b", FamilyOpenAICompatible)
	if keyModelA == keyModelB {
		t.Fatalf("different models must not share a profile key")
	}
	// 2. The same model name on different provider/config identities does not
	// collide.
	keyOtherConfig := OperationalProfileKey(otherConfig, "model-a", FamilyOpenAICompatible)
	if keyModelA == keyOtherConfig {
		t.Fatalf("different config identities must not share a profile key")
	}
	// 3. A protocol-family change does not inherit old learning.
	keyGoogle := OperationalProfileKey(baseConfig, "model-a", FamilyGoogleCompatible)
	if keyModelA == keyGoogle {
		t.Fatalf("different protocol families must not share a profile key")
	}
	if !strings.Contains(string(FamilyOpenAICompatible), "openai") {
		t.Fatalf("family name must stay explicit")
	}
}

func TestNewProfileStartsUnknownAndConservative(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	for _, field := range AllProfileFields {
		value := profile.Effective(field)
		if value.Known() {
			t.Fatalf("fresh profile field %s must be unknown, got %+v", field, value)
		}
	}
	if len(profile.Values) != 0 {
		t.Fatalf("fresh profile must carry no values")
	}
	if profile.ProfileVersion != ProfileVersion || profile.ProfileKey == "" {
		t.Fatalf("fresh profile metadata wrong: %+v", profile)
	}
}

func TestObservedEvidenceTightensButNeverRaises(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	// configured bound first.
	profile, err = profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 8000 || v.Provenance != ProvenanceConfigured {
		t.Fatalf("configured not applied: %+v", v)
	}

	// Restrictive observed evidence tightens.
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved, EvidenceRef: "obs-000001",
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != ProvenanceObserved || v.EvidenceRef != "obs-000001" {
		t.Fatalf("tightening not applied: %+v", v)
	}

	// Ordinary evidence trying to raise is REFUSED.
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 4096, Provenance: ProvenanceObserved, EvidenceRef: "obs-000002",
	}, fixedClock()); !errors.Is(err, ErrObservedCannotRaise) {
		t.Fatalf("observed raise must be refused with ErrObservedCannotRaise, got %v", err)
	}
	// ... and equal values are equally refused (no silent no-op raise).
	if _, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved, EvidenceRef: "obs-000003",
	}, fixedClock()); !errors.Is(err, ErrObservedCannotRaise) {
		t.Fatalf("observed equal value must be refused, got %v", err)
	}
	// The effective value never changed.
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 2048 {
		t.Fatalf("effective value changed after refused raise: %+v", v)
	}
}

func TestSuccessNeverRaisesHardCeilingOrConcurrency(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error
	profile, err = profile.ApplyConfigured(FieldMaxRequestBytes, 8000, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	profile, err = profile.ApplyConfigured(FieldConcurrencyCeiling, 2, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	// Give every tested field a known effective value first, so the negative
	// assertions below prove "success never raises" rather than "unknown
	// accepts its first observation".
	for _, field := range []struct {
		field ProfileField
		value int64
	}{
		{FieldMaxRequestBytes, 8000},
		{FieldMaxResponseBytes, 16000},
		{FieldRequestsPerMinute, 20},
		{FieldTimeoutMillis, 60000},
	} {
		profile, err = profile.ApplyConfigured(field.field, field.value, fixedClock())
		if err != nil {
			t.Fatal(err)
		}
	}
	// A successful-looking observation with a higher value never raises the
	// input/output/context bound.
	for _, field := range []ProfileField{FieldMaxRequestBytes, FieldConcurrencyCeiling, FieldMaxResponseBytes, FieldRequestsPerMinute, FieldTimeoutMillis} {
		before := profile.Effective(field)
		if _, err := profile.Apply(ProfileUpdate{
			Field: field, Value: before.Value + 1000, Provenance: ProvenanceObserved, EvidenceRef: "obs-success",
		}, fixedClock()); err == nil {
			t.Fatalf("field %s: success evidence raised a value without error", field)
		}
	}
	// Values above the Runstead hard caps are refused for EVERY provenance.
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: HardCapMaxRequestBytes + 1, Provenance: ProvenanceConfigured,
	}, fixedClock()); err == nil {
		t.Fatalf("configured above hard cap must fail")
	}
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldConcurrencyCeiling, Value: HardCapConcurrencyCeiling + 1, Provenance: ProvenanceAuthoritative, EvidenceRef: "verif-000001",
	}, fixedClock()); err == nil {
		t.Fatalf("authoritative above hard cap must fail")
	}
	if _, err := profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: HardCapMaxRequestBytes + 1, Provenance: ProvenanceObserved, EvidenceRef: "obs-000001",
	}, fixedClock()); err == nil {
		t.Fatalf("observed above hard cap must fail")
	}
}

func TestUnknownCanReceiveAllThreeProvenancesThroughTheirPaths(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	var err error

	// unknown -> configured
	profile, err = profile.Apply(ProfileUpdate{Field: FieldRequestsPerMinute, Value: 10, Provenance: ProvenanceConfigured}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldRequestsPerMinute); v.Provenance != ProvenanceConfigured {
		t.Fatalf("configured provenance lost")
	}

	// unknown -> observed (a specific, actually produced observation)
	profile, err = profile.Apply(ProfileUpdate{Field: FieldCooldownMillis, Value: 5000, Provenance: ProvenanceObserved, EvidenceRef: "obs-000010"}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldCooldownMillis); v.Provenance != ProvenanceObserved || v.EvidenceRef != "obs-000010" {
		t.Fatalf("observed provenance lost")
	}

	// unknown -> authoritative ONLY through the explicitly typed path.
	profile, err = profile.Apply(ProfileUpdate{Field: FieldTimeoutMillis, Value: 30000, Provenance: ProvenanceAuthoritative, EvidenceRef: "verif-000002"}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldTimeoutMillis); v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative provenance lost")
	}

	// The four semantics remain distinguishable. unknown is the honest
	// ABSENT state (never a writable origin); configured/observed/
	// authoritative are writable origins and parse strictly.
	if ProvenanceUnknown == ProvenanceConfigured || ProvenanceUnknown == ProvenanceObserved || ProvenanceUnknown == ProvenanceAuthoritative ||
		ProvenanceConfigured == ProvenanceObserved || ProvenanceObserved == ProvenanceAuthoritative || ProvenanceConfigured == ProvenanceAuthoritative {
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
	if _, err := ParseProvenance("guessed"); err == nil {
		t.Fatalf("unknown provenance must fail closed")
	}
	// A fresh profile field is the unknown state: not "configured" and not
	// "observed", with no guessed value.
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
		Field: FieldMaxRequestBytes, Value: 2048, Provenance: ProvenanceObserved, EvidenceRef: "obs-000001",
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
	// Authoritative evidence CAN raise (explicitly typed path, within caps).
	profile, err = profile.Apply(ProfileUpdate{
		Field: FieldMaxRequestBytes, Value: 16000, Provenance: ProvenanceAuthoritative, EvidenceRef: "verif-000003",
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(FieldMaxRequestBytes); v.Value != 16000 || v.Provenance != ProvenanceAuthoritative {
		t.Fatalf("authoritative raise not applied: %+v", v)
	}
}

func TestApplyIsPureAndProfileCarriesNoExecutionAuthority(t *testing.T) {
	profile := NewOperationalProfile(testIdentity("cfg", "model-a", FamilyOpenAICompatible))
	before := profile.Sanitized()
	next, err := profile.Apply(ProfileUpdate{
		Field: FieldTimeoutMillis, Value: 1000, Provenance: ProvenanceObserved, EvidenceRef: "obs-000020",
	}, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	// The receiver was never mutated: applying is a pure state transition.
	if profile.Sanitized() != before {
		t.Fatalf("Apply mutated the receiver")
	}
	_ = next

	// Negative authority assertions (#91 tests 14/15): the update/profiles
	// surface has no fields that could select a provider, change a model,
	// trigger a retry or route traffic anywhere.
	updateType := reflect.TypeOf(ProfileUpdate{})
	for _, field := range []string{
		"ProviderID", "ProtocolFamily", "Model", "Fallback", "Retry", "MaxRetries", "Client", "Endpoint",
	} {
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
	cases := []ProfileUpdate{
		{Field: "bogus_field", Value: 1, Provenance: ProvenanceConfigured},
		{Field: FieldTimeoutMillis, Value: -1, Provenance: ProvenanceConfigured},
		{Field: FieldTimeoutMillis, Value: 1, Provenance: "unknown"},
		{Field: FieldTimeoutMillis, Value: HardCapTimeoutMillis + 1, Provenance: ProvenanceConfigured},
	}
	for _, update := range cases {
		if _, err := profile.Apply(update, fixedClock()); err == nil {
			t.Fatalf("update %+v must fail closed", update)
		}
	}
}
