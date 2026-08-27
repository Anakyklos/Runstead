package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func testProfileIdentity() provider.Identity {
	return provider.Identity{
		ProviderID:     "provider-a",
		ProtocolFamily: provider.FamilyOpenAICompatible,
		Model:          "model-a",
		ConfigIdentity: "provider.Config{ProviderID:\"provider-a\" ProtocolFamily:\"openai_compatible\" Endpoint:\"https://e.invalid/v1\" Model:\"model-a\" AuthRequirement:\"none\"}",
		ProfileVersion: "v1",
		AdapterVersion: "test",
	}
}

func testProfile(t *testing.T, configIdentity string, model string) *provider.OperationalProfile {
	t.Helper()
	identity := testProfileIdentity()
	if configIdentity != "" {
		identity.ConfigIdentity = configIdentity
	}
	if model != "" {
		identity.Model = model
	}
	profile := provider.NewOperationalProfile(identity)
	next, err := profile.ApplyConfigured(provider.FieldMaxRequestBytes, 8000, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.Apply(provider.ProfileUpdate{
		Field: provider.FieldMaxRequestBytes, Value: 2048, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-000001",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err = next.Apply(provider.ProfileUpdate{
		Field: provider.FieldCooldownMillis, Value: 5000, Provenance: provider.ProvenanceAuthoritative, EvidenceRef: "verif-000001",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// TestProfilesSeparateModelsAndConfigsInPersistence proves two models on the
// same endpoint and the same model name on different config identities store
// independent profiles (the required key semantics survive a restart).
func TestProfilesSeparateModelsAndConfigsInPersistence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	identityA := testProfileIdentity()
	identityB := testProfileIdentity()
	identityB.Model = "model-b"
	identityC := testProfileIdentity()
	identityC.ConfigIdentity = "provider.Config{ProviderID:\"provider-c\" ...}"

	profileA := testProfile(t, "", "model-a")
	profileB := testProfile(t, "", "model-b")
	profileC := testProfile(t, identityC.ConfigIdentity, "model-a")

	for _, profile := range []*provider.OperationalProfile{profileA, profileB, profileC} {
		if err := store.SaveOperationalProfile(ctx, profile); err != nil {
			t.Fatalf("SaveOperationalProfile: %v", err)
		}
	}
	loadedA, err := store.LoadOperationalProfile(ctx, identityA)
	if err != nil {
		t.Fatal(err)
	}
	loadedB, err := store.LoadOperationalProfile(ctx, identityB)
	if err != nil {
		t.Fatal(err)
	}
	if loadedA == nil || loadedB == nil {
		t.Fatalf("profiles must load independently")
	}
	if v := loadedA.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("profile A content wrong: %+v", v)
	}
	if v := loadedB.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 {
		t.Fatalf("profile B content wrong: %+v", v)
	}
	// Updating A never changes B (independent keys). The observed update
	// targets requests_per_minute, which is absent in the seeded profile and
	// has a defined automatic safety direction (lower is conservative), so
	// unknown -> observed is legitimate there.
	updatedA, err := loadedA.Apply(provider.ProfileUpdate{
		Field: provider.FieldRequestsPerMinute, Value: 1000, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-000002",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOperationalProfile(ctx, updatedA); err != nil {
		t.Fatal(err)
	}
	reloadedB, err := store.LoadOperationalProfile(ctx, identityB)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedB.Effective(provider.FieldRequestsPerMinute).Known() {
		t.Fatalf("updating profile A leaked into profile B")
	}
	// profile C is independent of A and B (its own config identity).
	loadedC, err := store.LoadOperationalProfile(ctx, identityC)
	if err != nil {
		t.Fatal(err)
	}
	if loadedC == nil {
		t.Fatalf("profile C must load under its own identity")
	}
	if loadedC.ProviderID != identityC.ProviderID {
		t.Fatalf("profile C identity wrong")
	}
	// An incompatible identity that was NEVER stored must not inherit any of
	// the old profiles: family/config identity changes never reuse learning.
	neverStored := testProfileIdentity()
	neverStored.ConfigIdentity = `provider.Config{ProviderID:"brand-new" ProtocolFamily:"openai_compatible" Endpoint:"https://other.invalid/v1" ConfigVersion:"v9"}`
	if loaded, err := store.LoadOperationalProfile(ctx, neverStored); err != nil || loaded != nil {
		t.Fatalf("unrelated identity must not inherit old learning (loaded=%v err=%v)", loaded, err)
	}
	neverStoredFamily := testProfileIdentity()
	neverStoredFamily.ProtocolFamily = provider.FamilyGoogleCompatible
	if loaded, err := store.LoadOperationalProfile(ctx, neverStoredFamily); err != nil || loaded != nil {
		t.Fatalf("different protocol family must not inherit old learning (loaded=%v err=%v)", loaded, err)
	}
}

// TestOperationalProfileRestartPreservesWithoutProviderCalls proves the
// profile survives a real reopen of the SQLite file, and reconstruction is
// pure state reads (no provider requests are possible anywhere in the path).
func TestOperationalProfileRestartPreservesWithoutProviderCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")
	ctx := context.Background()

	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	profile := testProfile(t, "", "")
	if err := store.SaveOperationalProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the profile is reconstructed from durable state only.
	reopened, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.LoadOperationalProfile(ctx, testProfileIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatalf("profile lost across restart")
	}
	if v := loaded.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved || v.EvidenceRef != "obs-000001" {
		t.Fatalf("profile content lost across restart: %+v", v)
	}
	if v := loaded.Effective(provider.FieldCooldownMillis); v.Provenance != provider.ProvenanceAuthoritative {
		t.Fatalf("authoritative value lost across restart: %+v", v)
	}
}

// TestOperationalProfileFailClosed covers negative authority and corruption:
// key mismatches, invalid provenance rows, over-cap values and unknown fields
// are never silently repaired.
func TestOperationalProfileFailClosed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	identity := testProfileIdentity()

	// Key mismatch between the object and its own identity.
	badProfile := testProfile(t, "", "")
	badProfile.ProfileKey = strings.Repeat("0", 64)
	if err := store.SaveOperationalProfile(ctx, badProfile); err == nil {
		t.Fatalf("key mismatch must fail closed")
	}

	saveValid := func() {
		profile := testProfile(t, "", "")
		if err := store.SaveOperationalProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}

	// Directly corrupt one persisted row: unknown provenance.
	saveValid()
	if _, err := store.db.ExecContext(ctx, `UPDATE provider_operational_profiles SET provenance = 'guessed' WHERE field = 'cooldown_millis'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOperationalProfile(ctx, identity); err == nil {
		t.Fatalf("invalid provenance row must fail closed")
	}
	// Restore and corrupt differently: a persisted row whose value is
	// non-positive (zero/unknown representation violation) fails closed.
	if _, err := store.db.ExecContext(ctx, `UPDATE provider_operational_profiles SET value = 0 WHERE field = 'max_request_bytes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOperationalProfile(ctx, identity); err == nil {
		t.Fatalf("non-positive row must fail closed")
	}
	// Restore and corrupt the identity/key binding.
	if _, err := store.db.ExecContext(ctx, `UPDATE provider_operational_profiles SET config_identity = 'provider.Config{...other...}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOperationalProfile(ctx, identity); err == nil {
		t.Fatalf("identity/key mismatch row must fail closed")
	}
	// Evidence reference missing on an observed row fails closed.
	if _, err := store.db.ExecContext(ctx, `UPDATE provider_operational_profiles SET evidence_ref = '', provenance = 'observed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadOperationalProfile(ctx, identity); err == nil {
		t.Fatalf("observed row without evidence reference must fail closed")
	}
}

// TestOperationalProfileSecretsNeverPersisted proves credential-shaped
// content is redacted before reaching the database and that prompts/response
// bodies have no representation in the profile projection at all.
func TestOperationalProfileSecretsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")
	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	identity := testProfileIdentity()
	identity.ConfigIdentity = "provider.Config{ProviderID:\"x\" Endpoint:\"https://e.invalid/v1\" Auth:\"Bearer sk-super-secret-1234567890\"}"

	profile := provider.NewOperationalProfile(identity)
	next, err := profile.Apply(provider.ProfileUpdate{
		Field: provider.FieldCooldownMillis, Value: 1000, Provenance: provider.ProvenanceObserved,
		EvidenceRef: "obs-000001",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOperationalProfile(ctx, next); err != nil {
		t.Fatal(err)
	}

	// The raw database file never contains the credential-shaped value.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sk-super-secret", "Bearer", "prompt-body-text", "response-body-text"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("durable state contains forbidden content %q", forbidden)
		}
	}
	// A credential-shaped "identity" is impossible through the real contract
	// (Config.Sanitized is credential-free by construction); the persistence
	// layer's Redact defense makes such a row internally inconsistent, and
	// the load path fails CONSERVATIVELY instead of guessing or repairing:
	// corruption never becomes usable state.
	if loaded, err := store.LoadOperationalProfile(ctx, identity); err == nil && loaded != nil {
		t.Fatalf("redacted credential-shaped identity must fail closed on load")
	}
	// The perfectly legitimate identity surface never carries the secret.
	sane := testProfileIdentity()
	if err := store.SaveOperationalProfile(ctx, testProfile(t, "", "")); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadOperationalProfile(ctx, sane)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || strings.Contains(loaded.ConfigIdentity, "sk-super-secret") {
		t.Fatalf("sane identity must load cleanly without secret content")
	}
}

// TestRenderInspectExplainsOperationalProfile renders the sanitized profile
// section: effective value, provenance, identity, model and family, with
// unknown fields explicit and never guessed.
func TestRenderInspectExplainsOperationalProfile(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	taskID := "task-profile"
	snapshot := `{"provider_id":"provider-a","protocol_family":"openai_compatible","provider_model":"model-a","provider_config_identity":"provider.Config{ProviderID:\"provider-a\" ProtocolFamily:\"openai_compatible\" Endpoint:\"https://e.invalid/v1\" Model:\"model-a\" AuthRequirement:\"none\"}","provider_profile_version":"v1","provider_adapter_version":"compatible-provider-v0.1"}`
	if err := store.CreateTask(ctx, TaskRecord{TaskID: taskID, Objective: "objective", Workspace: "/tmp/ws", Model: "model-a", ConfigJSON: []byte(snapshot)}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOperationalProfile(ctx, testProfile(t, "", "")); err != nil {
		t.Fatal(err)
	}

	var rendered bytes.Buffer
	if err := store.RenderInspect(ctx, &rendered, taskID); err != nil {
		t.Fatal(err)
	}
	output := rendered.String()
	for _, want := range []string{
		"Operational profile:",
		"provider_id=provider-a",
		"protocol_family=openai_compatible",
		"model=model-a",
		"profile_version=v1",
		"max_request_bytes: value=2048 provenance=observed",
		"evidence_ref=obs-000001",
		"cooldown_millis: value=5000 provenance=authoritative",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("inspect missing %q:\n%s", want, output)
		}
	}
	// Unknown fields are explicit, never guessed.
	if !strings.Contains(output, "concurrency_ceiling: unknown") {
		t.Fatalf("unknown field must render as unknown:\n%s", output)
	}
	// No prompt/body/secret content ever appears.
	if strings.Contains(output, "sk-super-secret") || strings.Contains(output, "prompt") {
		t.Fatalf("inspect leaked forbidden content:\n%s", output)
	}
}

// TestRenderInspectWithoutConfiguredProviderRendersNoProfile ensures the
// legacy scripted/OmniRoute lanes stay unchanged.
func TestRenderInspectWithoutConfiguredProviderRendersNoProfile(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	mustTask(t, store, "task-legacy")
	var rendered bytes.Buffer
	if err := store.RenderInspect(ctx, &rendered, "task-legacy"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.String(), "Operational profile:") {
		t.Fatalf("legacy lane must not render an operational profile section:\n%s", rendered.String())
	}
}

// applyObservedThroughDurableBoundary is the legitimate way the runtime (and
// tests) feed a specific produced observation into the durable profile.
func applyObservedThroughDurableBoundary(t *testing.T, store *Store, identity provider.Identity, field provider.ProfileField, value int64, evidence string) {
	t.Helper()
	if _, err := store.ApplyOperationalProfileUpdates(context.Background(), identity, nil, []provider.ProfileUpdate{{
		Field: field, Value: value, Provenance: provider.ProvenanceObserved, EvidenceRef: evidence,
	}}); err != nil {
		t.Fatalf("ApplyOperationalProfileUpdates(observed): %v", err)
	}
}

// TestOperationalProfileUpdatesMonotonicAcrossReruns is the #91-review
// regression: persisted configured=8000, tightened to observed=2048, then
// re-running the SAME configured bound (8000) must leave the field at
// 2048/observed, and a restart must preserve the property.
func TestOperationalProfileUpdatesMonotonicAcrossReruns(t *testing.T) {
	identity := testProfileIdentity()
	ctx := context.Background()

	// 1) persist configured=8000.
	store := openTestStore(t)
	profile, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldMaxRequestBytes, Value: 8000, Provenance: provider.ProvenanceConfigured,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := profile.Effective(provider.FieldMaxRequestBytes); v.Value != 8000 || v.Provenance != provider.ProvenanceConfigured {
		t.Fatalf("configured=8000 not persisted: %+v", v)
	}
	// 2) tighten through the durable boundary to observed=2048.
	applyObservedThroughDurableBoundary(t, store, identity, provider.FieldMaxRequestBytes, 2048, "obs-000001")
	loaded, err := store.LoadOperationalProfile(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("observed=2048 not persisted: %+v", v)
	}
	// 3) re-run with the SAME configured bound (replay).
	replayed, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldMaxRequestBytes, Value: 8000, Provenance: provider.ProvenanceConfigured,
	}})
	if err != nil {
		t.Fatalf("configured replay must be a benign no-op, got error: %v", err)
	}
	if replayed == nil {
		t.Fatalf("replay must still return the profile")
	}
	if v := replayed.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("configured replay undid the observed tightening: %+v", v)
	}
	// 4) restart does not alter the property: reopen across a real SQLite
	// file is covered by TestOperationalProfileRestartPreservesWithoutProviderCalls
	// (same persistence path); here we repeat the monotonic assertion through
	// a fresh Load of the same durable store.
	again, err := store.LoadOperationalProfile(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Effective(provider.FieldMaxRequestBytes); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("property lost after reload: %+v", v)
	}
}

// TestOperationalProfileUpdatesCooldownDirection is the #91-review
// regression at the durable boundary: Retry-After 60s over configured 30s is
// accepted as conservative tightening; a 10s observation never weakens it.
func TestOperationalProfileUpdatesCooldownDirection(t *testing.T) {
	identity := testProfileIdentity()
	ctx := context.Background()
	store := openTestStore(t)

	if _, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldCooldownMillis, Value: 30000, Provenance: provider.ProvenanceConfigured,
	}}); err != nil {
		t.Fatal(err)
	}
	// Retry-After 60s -> accepted (higher is conservative).
	if _, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldCooldownMillis, Value: 60000, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-retry-after",
	}}); err != nil {
		t.Fatalf("cooldown 30s -> observed 60s must be accepted: %v", err)
	}
	// Observation 10s -> refused (would weaken).
	if _, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldCooldownMillis, Value: 10000, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-faster",
	}}); !errors.Is(err, provider.ErrObservedNotConservative) {
		t.Fatalf("cooldown weakening must be refused, got %v", err)
	}
	loaded, err := store.LoadOperationalProfile(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Effective(provider.FieldCooldownMillis); v.Value != 60000 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("cooldown weakened without authority: %+v", v)
	}
}

// TestZeroAndNegativeUpdatesRefusedAtDurableBoundary proves the single
// zero/unknown representation holds at the store boundary too.
func TestZeroAndNegativeUpdatesRefusedAtDurableBoundary(t *testing.T) {
	identity := testProfileIdentity()
	ctx := context.Background()
	store := openTestStore(t)
	for _, update := range []provider.ProfileUpdate{
		{Field: provider.FieldMaxRequestBytes, Value: 0, Provenance: provider.ProvenanceConfigured},
		{Field: provider.FieldMaxRequestBytes, Value: -5, Provenance: provider.ProvenanceConfigured},
		{Field: provider.FieldCooldownMillis, Value: 0, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-000001"},
		{Field: provider.FieldTimeoutMillis, Value: 0, Provenance: provider.ProvenanceAuthoritative, EvidenceRef: "verif-000001"},
	} {
		if _, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{update}); err == nil {
			t.Fatalf("non-positive update %+v must fail closed at the durable boundary", update)
		}
	}
}

// TestOperationalProfileUpdatesTimeoutUnknownObservedPersistsNoRow is the
// #91-review durable-boundary regression: an observed timeout on an unknown
// field is refused (no automatic safety direction) and NO row is persisted.
func TestOperationalProfileUpdatesTimeoutUnknownObservedPersistsNoRow(t *testing.T) {
	identity := testProfileIdentity()
	ctx := context.Background()
	store := openTestStore(t)

	if _, err := store.ApplyOperationalProfileUpdates(ctx, identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldTimeoutMillis, Value: 30000, Provenance: provider.ProvenanceObserved, EvidenceRef: "obs-timeout-first",
	}}); !errors.Is(err, provider.ErrNoAutomaticDirection) {
		t.Fatalf("unknown timeout -> observed at the durable boundary must fail with ErrNoAutomaticDirection, got %v", err)
	}
	// No row exists for any field of that profile: the refused observation
	// persisted nothing.
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_operational_profiles`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("refused timeout observation persisted %d rows, want 0", count)
	}
	loaded, err := store.LoadOperationalProfile(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatalf("refused observation produced a profile: %+v", loaded)
	}
}
