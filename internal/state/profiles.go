package state

import (
	"context"
	"database/sql"
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
		// The stored provider_id must match the identity the profile is
		// being reconstructed for: a tampered provider_id is corruption and
		// fails closed instead of rendering as valid state (#91 review).
		ref, parseErr := provider.ParseEvidenceRef(evidenceRef)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: row for key %s: %v", ErrProfileState, shortKeyRef(key), parseErr)
		}
		if err := validatePersistedProfileRow(key, identity.ProviderID, providerID, family, configIdentity, model, version, field, value, provenance, ref); err != nil {
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
			EvidenceRef: ref,
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

// validatePersistedProfileRow re-validates one persisted row and re-derives
// the key from the stored identity columns: stored state whose key does not
// match its own identity, whose provider_id does not match the identity it is
// being loaded for, whose evidence reference is not a structured sanitized
// kind:id, or whose provenance/value violates the contract is corruption and
// fails closed.
func validatePersistedProfileRow(key, expectedProviderID, providerID, family, configIdentity, model, version, field string, value int64, provenance string, evidenceRef provider.EvidenceRef) error {
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
	if value <= 0 {
		return fmt.Errorf("%w: row for key %s has non-positive value %d (zero means unknown/absent)", ErrProfileState, shortKeyRef(key), value)
	}
	if strings.TrimSpace(version) != provider.ProfileVersion {
		return fmt.Errorf("%w: row for key %s has unsupported profile version %q", ErrProfileState, shortKeyRef(key), version)
	}
	if strings.TrimSpace(providerID) != strings.TrimSpace(expectedProviderID) {
		return fmt.Errorf("%w: row for key %s has provider id %q that does not match the identity %q", ErrProfileState, shortKeyRef(key), providerID, expectedProviderID)
	}
	expected := provider.OperationalProfileKey(configIdentity, model, parsedFamily)
	if expected != key {
		return fmt.Errorf("%w: row key %s does not match its stored identity", ErrProfileState, shortKeyRef(key))
	}
	// Evidence references are structured kind:id identifiers; free text and
	// configured rows carrying references are provenance violations.
	if parsed == provider.ProvenanceObserved || parsed == provider.ProvenanceAuthoritative {
		if !evidenceRef.Valid() {
			return fmt.Errorf("%w: row for key %s has provenance %q without a valid structured evidence reference", ErrProfileState, shortKeyRef(key), parsed)
		}
	} else if evidenceRef.Kind != "" {
		return fmt.Errorf("%w: row for key %s carries an evidence reference on configured provenance", ErrProfileState, shortKeyRef(key))
	}
	return nil
}

func shortKeyRef(key string) string {
	if len(key) > 16 {
		return key[:16] + "…"
	}
	return key
}

// ApplyOperationalProfileUpdates applies typed profile updates
// MONOTONICALLY at the durable boundary: the current row state is read and
// validated inside the SAME SQLite transaction that writes the result
// (check-and-set), so concurrent tasks cannot interleave a stale write that
// undoes an observed tightening or authoritative acceptance. The rule engine
// is provider.ApplyFieldValue (single source of truth shared with the
// in-memory profile). A configured replay that would undo an
// observed/authoritative value signals provider.ErrProfileReplayUndo and
// leaves that field untouched while the remaining updates still apply
// (atomic commit of the successful subset).
func (s *Store) ApplyOperationalProfileUpdates(ctx context.Context, identity provider.Identity, clock func() time.Time, updates []provider.ProfileUpdate) (*provider.OperationalProfile, error) {
	if identity.ConfigIdentity == "" || identity.Model == "" || !identity.ProtocolFamily.Valid() {
		return nil, fmt.Errorf("%w: incomplete profile identity", ErrProfileState)
	}
	if len(updates) == 0 {
		return nil, nil
	}
	key := provider.OperationalProfileKey(identity.ConfigIdentity, identity.Model, identity.ProtocolFamily)
	now := time.Now().UTC().Format(time.RFC3339)
	if clock != nil {
		now = clock().UTC().Format(time.RFC3339)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin operational profile updates: %w", err)
	}
	defer tx.Rollback()

	var createdAt string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(created_at), '') FROM provider_operational_profiles WHERE profile_key = ?`, key).Scan(&createdAt); err != nil {
		return nil, fmt.Errorf("read operational profile age: %w", err)
	}
	if createdAt == "" {
		createdAt = now
	}

	applied := make(map[provider.ProfileField]provider.ProfileValue)
	for _, update := range updates {
		var storedProviderID, storedFamily, storedConfigIdentity, storedModel, storedVersion string
		var currentValue int64
		var currentProvenance, currentEvidence, currentUpdated string
		var exists bool
		err := tx.QueryRowContext(ctx,
			`SELECT provider_id, protocol_family, config_identity, model, profile_version, value, provenance, evidence_ref, updated_at
			 FROM provider_operational_profiles WHERE profile_key = ? AND field = ?`,
			key, string(update.Field)).Scan(&storedProviderID, &storedFamily, &storedConfigIdentity, &storedModel, &storedVersion,
			&currentValue, &currentProvenance, &currentEvidence, &currentUpdated)
		currentRef, parseErr := provider.ParseEvidenceRef(currentEvidence)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: row for key %s: %v", ErrProfileState, shortKeyRef(key), parseErr)
		}
		if err == nil {
			// Validate the STORED row in full (identity columns AND value
			// columns) against the identity this update is being applied
			// under, BEFORE any write can touch it: uncertain state must
			// never be updated/overwritten and then discovered only after
			// commit (fail-closed before any durable effect).
			if err := validatePersistedProfileRow(key, identity.ProviderID, storedProviderID, storedFamily, storedConfigIdentity, storedModel,
				storedVersion, string(update.Field), currentValue, currentProvenance, currentRef); err != nil {
				return nil, err
			}
			exists = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read operational profile field %s: %w", update.Field, err)
		}

		current := provider.ProfileValue{Value: currentValue, Provenance: provider.Provenance(currentProvenance), EvidenceRef: currentRef, UpdatedAt: currentUpdated}
		next, applyErr := provider.ApplyFieldValue(update.Field, current, exists, update, now)
		if errors.Is(applyErr, provider.ErrProfileReplayUndo) {
			// Benign no-op for the same unchanged identity: the field keeps
			// its more conservative value.
			if exists {
				applied[update.Field] = current
			}
			continue
		}
		if applyErr != nil {
			return nil, applyErr
		}
		if !exists {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_operational_profiles
				 (profile_key, provider_id, protocol_family, config_identity, model, profile_version, field, value, provenance, evidence_ref, updated_at, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				key, Redact(identity.ProviderID), Redact(string(identity.ProtocolFamily)), Redact(identity.ConfigIdentity),
				Redact(identity.Model), provider.ProfileVersion, string(update.Field), next.Value, string(next.Provenance),
				Redact(next.EvidenceRef.String()), next.UpdatedAt, createdAt); err != nil {
				return nil, fmt.Errorf("insert operational profile field %s: %w", update.Field, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE provider_operational_profiles SET value = ?, provenance = ?, evidence_ref = ?, updated_at = ? WHERE profile_key = ? AND field = ?`,
				next.Value, string(next.Provenance), Redact(next.EvidenceRef.String()), next.UpdatedAt, key, string(update.Field)); err != nil {
				return nil, fmt.Errorf("update operational profile field %s: %w", update.Field, err)
			}
		}
		applied[update.Field] = next
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit operational profile updates: %w", err)
	}
	if len(applied) == 0 {
		return nil, nil
	}
	return s.loadOperationalProfileByKey(ctx, key, identity)
}
