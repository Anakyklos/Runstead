package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// ErrProfileState marks invalid/corrupted persisted operational profile
// state. It fails closed: unknown provenance, unknown field, negative or
// over-cap values and identity/key mismatches are never silently repaired.
var ErrProfileState = errors.New("invalid persisted operational profile")

// profile field separator used by the persisted projection.
const profileSchemaVersion = "v1"

// SaveOperationalProfile persists one profile (all its known fields) with
// per-field upsert. The stored projection is sanitized (Redact is applied as
// defense in depth) and every field is validated fail-closed BEFORE the
// write. A profile whose key does not match its identity columns is refused.
func (s *Store) SaveOperationalProfile(ctx context.Context, profile *provider.OperationalProfile) error {
	if profile == nil {
		return fmt.Errorf("%w: nil profile", ErrProfileState)
	}
	if profile.ProfileVersion != provider.ProfileVersion {
		return fmt.Errorf("%w: unsupported profile version %q", ErrProfileState, profile.ProfileVersion)
	}
	expectedKey := provider.OperationalProfileKey(profile.ConfigIdentity, profile.Model, profile.ProtocolFamily)
	if profile.ProfileKey != expectedKey {
		return fmt.Errorf("%w: profile key %q does not match the identity", ErrProfileState, profile.ProfileKey)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operational profile save: %w", err)
	}
	defer tx.Rollback()
	for _, field := range provider.AllProfileFields {
		value, exists := profile.Values[field]
		if !exists {
			continue
		}
		if err := validateProfileRow(profile.ProfileKey, profile.ConfigIdentity, profile.Model, profile.ProtocolFamily, profile.ProfileVersion, field, value); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO provider_operational_profiles
			 (profile_key, provider_id, protocol_family, config_identity, model, profile_version, field, value, provenance, evidence_ref, updated_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (profile_key, field) DO UPDATE SET
			   value = excluded.value,
			   provenance = excluded.provenance,
			   evidence_ref = excluded.evidence_ref,
			   updated_at = excluded.updated_at`,
			profile.ProfileKey, Redact(profile.ProviderID), Redact(string(profile.ProtocolFamily)),
			Redact(profile.ConfigIdentity), Redact(profile.Model), profile.ProfileVersion,
			string(field), value.Value, string(value.Provenance), Redact(value.EvidenceRef),
			value.UpdatedAt, now); err != nil {
			return fmt.Errorf("upsert operational profile field %s: %w", field, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operational profile save: %w", err)
	}
	return nil
}

// LoadOperationalProfile reconstructs the profile for one identity from
// durable state. It performs NO provider call (recovery never rediscovers
// information by repeating requests). Rows whose key/identity/provenance
// are inconsistent or whose values violate the contract fail closed.
func (s *Store) LoadOperationalProfile(ctx context.Context, identity provider.Identity) (*provider.OperationalProfile, error) {
	key := provider.OperationalProfileKey(identity.ConfigIdentity, identity.Model, identity.ProtocolFamily)
	return s.loadOperationalProfileByKey(ctx, key, identity)
}

func (s *Store) loadOperationalProfileByKey(ctx context.Context, key string, identity provider.Identity) (*provider.OperationalProfile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_id, protocol_family, config_identity, model, profile_version, field, value, provenance, evidence_ref, updated_at, created_at
		 FROM provider_operational_profiles WHERE profile_key = ?`, key)
	if err != nil {
		return nil, fmt.Errorf("load operational profile: %w", err)
	}
	defer rows.Close()
	profile := &provider.OperationalProfile{
		ProfileKey:     key,
		ProviderID:     identity.ProviderID,
		ConfigIdentity: identity.ConfigIdentity,
		Model:          identity.Model,
		ProtocolFamily: identity.ProtocolFamily,
		ProfileVersion: provider.ProfileVersion,
		Values:         make(map[provider.ProfileField]provider.ProfileValue),
		CreatedAt:      "",
		UpdatedAt:      "",
	}
	found := false
	for rows.Next() {
		found = true
		var providerID, family, configIdentity, model, version, field, provenance, evidenceRef, updatedAt, createdAt string
		var value int64
		if err := rows.Scan(&providerID, &family, &configIdentity, &model, &version, &field, &value, &provenance, &evidenceRef, &updatedAt, &createdAt); err != nil {
			return nil, fmt.Errorf("scan operational profile: %w", err)
		}
		if err := validatePersistedProfileRow(key, providerID, family, configIdentity, model, version, field, value, provenance, evidenceRef); err != nil {
			return nil, err
		}
		profile.ProviderID = providerID
		profile.ProtocolFamily = provider.ProtocolFamily(family)
		profile.ConfigIdentity = configIdentity
		profile.Model = model
		profile.ProfileVersion = version
		profile.Values[provider.ProfileField(field)] = provider.ProfileValue{
			Value:       value,
			Provenance:  provider.Provenance(provenance),
			EvidenceRef: evidenceRef,
			UpdatedAt:   updatedAt,
		}
		if profile.CreatedAt == "" || createdAt < profile.CreatedAt {
			profile.CreatedAt = createdAt
		}
		if updatedAt > profile.UpdatedAt {
			profile.UpdatedAt = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operational profile: %w", err)
	}
	if !found {
		// The identity is valid but has never been written: this is the
		// honest UNKNOWN state, not corruption and not a fabricated value.
		return nil, nil
	}
	return profile, nil
}

// validateProfileRow rejects invalid in-memory rows BEFORE the write.
func validateProfileRow(key, configIdentity, model string, family provider.ProtocolFamily, version string, field provider.ProfileField, value provider.ProfileValue) error {
	if !provider.IsProfileField(field) {
		return fmt.Errorf("%w: unknown profile field %q", ErrProfileState, string(field))
	}
	if !value.Provenance.Valid() {
		return fmt.Errorf("%w: field %s has invalid provenance %q", ErrProfileState, field, string(value.Provenance))
	}
	if value.Value < 0 {
		return fmt.Errorf("%w: field %s has negative value %d", ErrProfileState, field, value.Value)
	}
	if cap := provider.HardCapFor(field); value.Value > cap {
		return fmt.Errorf("%w: field %s value %d exceeds hard cap %d", ErrProfileState, field, value.Value, cap)
	}
	if !family.Valid() {
		return fmt.Errorf("%w: protocol family %q is invalid", ErrProfileState, string(family))
	}
	if version != provider.ProfileVersion {
		return fmt.Errorf("%w: profile version %q is unsupported", ErrProfileState, version)
	}
	return nil
}

// validatePersistedProfileRow re-validates one persisted row and re-derives
// the key from the stored identity columns: stored state whose key does not
// match its own identity is corruption and fails closed.
func validatePersistedProfileRow(key, providerID, family, configIdentity, model, version, field string, value int64, provenance, evidenceRef string) error {
	if !provider.IsProfileField(provider.ProfileField(field)) {
		return fmt.Errorf("%w: row for key %s has unknown field %q", ErrProfileState, shortKeyRef(key), field)
	}
	parsedFamily, err := provider.ParseProtocolFamily(family)
	if err != nil || parsedFamily.String() != strings.TrimSpace(family) {
		return fmt.Errorf("%w: row for key %s has invalid protocol family", ErrProfileState, shortKeyRef(key))
	}
	parsed, err := provider.ParseProvenance(provenance)
	if err != nil || string(parsed) != strings.TrimSpace(provenance) {
		return fmt.Errorf("%w: row for key %s has invalid provenance %q", ErrProfileState, shortKeyRef(key), provenance)
	}
	if value < 0 {
		return fmt.Errorf("%w: row for key %s has negative value", ErrProfileState, shortKeyRef(key))
	}
	if cap := provider.HardCapFor(provider.ProfileField(field)); value > cap {
		return fmt.Errorf("%w: row for key %s exceeds the hard cap", ErrProfileState, shortKeyRef(key))
	}
	if strings.TrimSpace(version) != provider.ProfileVersion {
		return fmt.Errorf("%w: row for key %s has unsupported profile version %q", ErrProfileState, shortKeyRef(key), version)
	}
	expected := provider.OperationalProfileKey(configIdentity, model, parsedFamily)
	if expected != key {
		return fmt.Errorf("%w: row key %s does not match its stored identity", ErrProfileState, shortKeyRef(key))
	}
	// Only observed/authoritative rows may carry an evidence reference; a
	// reference on configured state is a provenance violation.
	if (parsed == provider.ProvenanceObserved || parsed == provider.ProvenanceAuthoritative) && strings.TrimSpace(evidenceRef) == "" {
		return fmt.Errorf("%w: row for key %s has provenance %q without evidence reference", ErrProfileState, shortKeyRef(key), parsed)
	}
	return nil
}

func shortKeyRef(key string) string {
	if len(key) > 16 {
		return key[:16] + "…"
	}
	return key
}
