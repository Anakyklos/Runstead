package provider

// Operational profile (#91): durable, versioned operational metadata for one
// combination of SANITIZED provider/config identity + exact model + protocol
// family, with explicit evidence provenance. A profile is OPERATIONAL
// METADATA only: it is not task truth, not policy, not approval, not retry
// authority, not verifier state and never model-controlled state.
//
// Guarantees enforced by the update rules (OperationalProfile.Apply):
//   - evidence can make the runtime automatically MORE conservative
//     (observed evidence may tighten an effective value, never raise it);
//   - ordinary successful requests NEVER raise a hard ceiling, concurrency
//     ceiling, rate envelope, context/output limit or any bound;
//   - raising a value above the currently known effective value for the same
//     unchanged identity requires operator configuration or an explicitly
//     typed authoritative update, and is always bounded by Runstead hard caps;
//   - unknown stays unknown and is never fabricated into information.
//
// The profile cannot execute retries, cannot grant admission, cannot change
// provider/model/fallback and never contains credentials, prompts or private
// response bodies.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProfileVersion identifies the operational profile schema/semantics version.
const ProfileVersion = "v1"

// ErrProfileInvalid marks an operational profile/update that cannot be
// applied or persisted. All rule failures wrap it so callers can classify
// without parsing text.
var ErrProfileInvalid = errors.New("invalid operational profile")

// ErrObservedCannotRaise is returned when observed evidence tries to raise
// an effective value: ordinary observations can only tighten or fill
// unknown, never increase a ceiling (#91).
var ErrObservedCannotRaise = errors.New("observed evidence cannot raise an effective value")

// ErrProfileReplayUndo is returned when a configured-replay update (the same
// unchanged identity re-supplying its declared bounds) would undo an
// effective observed tightening or authoritative acceptance. It is a
// benign no-op signal for the composition root: the profile is left intact.
var ErrProfileReplayUndo = errors.New("configured replay would undo observed/authoritative effective value")

// Provenance is the typed origin of one persisted profile value. The four
// semantics are distinct and fail-closed: the zero value is invalid.
type Provenance string

const (
	// ProvenanceUnknown means no information exists; the effective value is
	// absent and nothing is guessed.
	ProvenanceUnknown Provenance = "unknown"
	// ProvenanceConfigured is operator-declared configuration evidence (for
	// example capability-profile bounds).
	ProvenanceConfigured Provenance = "configured"
	// ProvenanceObserved is concrete runtime evidence actually produced
	// (restrictive limits, retry-after envelopes, timeout observations),
	// referenced by its sanitized evidence id.
	ProvenanceObserved Provenance = "observed"
	// ProvenanceAuthoritative is evidence accepted through an explicitly
	// typed, contract-reviewed path (for example an operator-accepted
	// evidence record); it is the only provenance that can raise a value for
	// the same unchanged identity, still bounded by the Runstead hard caps.
	ProvenanceAuthoritative Provenance = "authoritative"
)

// Valid reports whether p is one of the known provenance values. The zero
// value is invalid and fails closed everywhere it is checked.
func (p Provenance) Valid() bool {
	switch p {
	case ProvenanceConfigured, ProvenanceObserved, ProvenanceAuthoritative:
		return true
	default:
		return false
	}
}

// ParseProvenance parses an operator/evidence-supplied provenance name,
// failing closed on empty or unknown values.
func ParseProvenance(value string) (Provenance, error) {
	parsed := Provenance(strings.TrimSpace(value))
	if !parsed.Valid() {
		return "", fmt.Errorf("unknown provenance %q (supported: configured, observed, authoritative)", strings.TrimSpace(value))
	}
	return parsed, nil
}

// ProfileField names one variable operational bound/envelope carried by the
// profile. The set is deliberately small and closed.
type ProfileField string

const (
	// FieldMaxRequestBytes is the input/context constraint (request payload
	// bound).
	FieldMaxRequestBytes ProfileField = "max_request_bytes"
	// FieldMaxResponseBytes is the output constraint (response payload
	// bound).
	FieldMaxResponseBytes ProfileField = "max_response_bytes"
	// FieldRequestsPerMinute is the conservative rate envelope.
	FieldRequestsPerMinute ProfileField = "requests_per_minute"
	// FieldCooldownMillis is the conservative cooldown envelope observed
	// after rate limiting (e.g. parsed Retry-After).
	FieldCooldownMillis ProfileField = "cooldown_millis"
	// FieldConcurrencyCeiling is the effective concurrency ceiling. It can
	// only be tightened by evidence; success never raises it.
	FieldConcurrencyCeiling ProfileField = "concurrency_ceiling"
	// FieldTimeoutMillis is the conservative timeout/latency observation
	// bound when adequate evidence exists.
	FieldTimeoutMillis ProfileField = "timeout_millis"
)

// AllProfileFields is the deterministic closed vocabulary.
var AllProfileFields = []ProfileField{
	FieldMaxRequestBytes,
	FieldMaxResponseBytes,
	FieldRequestsPerMinute,
	FieldCooldownMillis,
	FieldConcurrencyCeiling,
	FieldTimeoutMillis,
}

// IsProfileField reports whether field belongs to the closed vocabulary.
func IsProfileField(field ProfileField) bool {
	for _, known := range AllProfileFields {
		if field == known {
			return true
		}
	}
	return false
}

// Hard caps: Runstead's OWN absolute ceilings. No effective value may ever
// exceed the cap of its field; a raising update is refused even with
// configured/authoritative provenance. Values are deliberately conservative.
const (
	HardCapMaxRequestBytes    int64 = 32 << 20 // 32 MiB
	HardCapMaxResponseBytes   int64 = 64 << 20 // 64 MiB
	HardCapRequestsPerMinute  int64 = 1000
	HardCapCooldownMillis     int64 = 3600_000 // 1h
	HardCapConcurrencyCeiling int64 = 16
	HardCapTimeoutMillis      int64 = 600_000 // 10m
)

// HardCapFor returns Runstead's hard cap for one profile field. Unknown
// fields have no cap and are refused earlier by validation.
func HardCapFor(field ProfileField) int64 {
	switch field {
	case FieldMaxRequestBytes:
		return HardCapMaxRequestBytes
	case FieldMaxResponseBytes:
		return HardCapMaxResponseBytes
	case FieldRequestsPerMinute:
		return HardCapRequestsPerMinute
	case FieldCooldownMillis:
		return HardCapCooldownMillis
	case FieldConcurrencyCeiling:
		return HardCapConcurrencyCeiling
	case FieldTimeoutMillis:
		return HardCapTimeoutMillis
	default:
		return 0
	}
}

// ProfileValue is one persisted variable value with its provenance. Missing
// or zero fields are UNKNOWN: no default is fabricated.
type ProfileValue struct {
	Value      int64
	Provenance Provenance
	// EvidenceRef is the SANITIZED evidence reference (evidence id, task id,
	// verification attempt id) when the origin is observed/authoritative. It
	// is never a copy of private evidence content.
	EvidenceRef string
	// UpdatedAt is the RFC3339 UTC time of the last update to this field.
	UpdatedAt string
}

// Known reports whether the field carries an effective value.
func (v ProfileValue) Known() bool {
	return v.Provenance.Valid() && v.Value > 0
}

// OperationalProfileKey derives the deterministic profile identity from the
// sanitized config identity, the exact model and the protocol family. The
// key separates:
//   - two models on the same endpoint (different keys);
//   - the same model name on different providers/configurations (different
//     keys);
//   - incompatible family/config identity changes (different keys, so old
//     learning is never inherited silently).
func OperationalProfileKey(configIdentity, model string, family ProtocolFamily) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(configIdentity) + "\x00" + strings.TrimSpace(model) + "\x00" + string(family)))
	return hex.EncodeToString(hash[:])
}

// OperationalProfile is the in-memory representation of one profile. It is
// stateful metadata, NOT an executor: it contains no Client, no context and
// no way to trigger provider activity.
type OperationalProfile struct {
	ProfileKey     string
	ProviderID     string
	ConfigIdentity string
	Model          string
	ProtocolFamily ProtocolFamily
	ProfileVersion string
	Values         map[ProfileField]ProfileValue
	CreatedAt      string
	UpdatedAt      string
}

// NewOperationalProfile creates a FRESH profile for an identity: every field
// is unknown/absent. It never guesses values.
func NewOperationalProfile(identity Identity) *OperationalProfile {
	now := time.Now().UTC().Format(time.RFC3339)
	return &OperationalProfile{
		ProfileKey:     OperationalProfileKey(identity.ConfigIdentity, identity.Model, identity.ProtocolFamily),
		ProviderID:     identity.ProviderID,
		ConfigIdentity: identity.ConfigIdentity,
		Model:          identity.Model,
		ProtocolFamily: identity.ProtocolFamily,
		ProfileVersion: ProfileVersion,
		Values:         make(map[ProfileField]ProfileValue),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// ProfileUpdate is one typed, deterministic update request to a profile. It
// deliberately carries NO provider/model/fallback/retry authority: the
// identity of the profile is fixed at construction; only a field value with
// provenance and evidence may be proposed.
type ProfileUpdate struct {
	Field       ProfileField
	Value       int64
	Provenance  Provenance
	EvidenceRef string
}

// Validate checks one update for internal consistency BEFORE it reaches the
// rule engine. Unknown provenance, unknown field, negative values, values
// above the Runstead hard cap and missing evidence references for
// observed/authoritative provenance all fail closed.
func (u ProfileUpdate) Validate() error {
	if !IsProfileField(u.Field) {
		return fmt.Errorf("%w: unknown profile field %q", ErrProfileInvalid, string(u.Field))
	}
	if !u.Provenance.Valid() {
		return fmt.Errorf("%w: provenance %q is not a valid update origin (unknown values are never written)", ErrProfileInvalid, string(u.Provenance))
	}
	if u.Value < 0 {
		return fmt.Errorf("%w: %s value must not be negative", ErrProfileInvalid, u.Field)
	}
	if cap := HardCapFor(u.Field); u.Value > cap {
		return fmt.Errorf("%w: %s value %d exceeds the Runstead hard cap %d", ErrProfileInvalid, u.Field, u.Value, cap)
	}
	if u.Provenance == ProvenanceObserved || u.Provenance == ProvenanceAuthoritative {
		if strings.TrimSpace(u.EvidenceRef) == "" {
			return fmt.Errorf("%w: %s with provenance %q requires a sanitized evidence reference", ErrProfileInvalid, u.Field, u.Provenance)
		}
	}
	return nil
}

// Apply executes the deterministic update rules and returns a NEW profile
// state. The receiver is never mutated: profiles are pure state so no side
// effect (network, retry, admission) is even possible.
//
// Rules:
//
//	unknown -> configured            (operator declaration)
//	unknown -> observed              (a specific, actually produced observation,
//	                                 with evidence reference)
//	unknown -> authoritative         (only through the explicitly typed path)
//	observed value < effective       (restrictive evidence tightens)
//	observed value >= effective      REFUSED (success/common evidence never
//	                                 raises a ceiling; ErrObservedCannotRaise)
//	configured < effective           (operator tightens further)
//	configured >= effective          REFUSED when the current provenance is
//	                                 observed or authoritative (a replay of the
//	                                 same unchanged identity must not undo
//	                                 tightening/acceptance)
//	authoritative                    ACCEPTED (explicitly typed path), bounded
//	                                 by the hard caps
func (p *OperationalProfile) Apply(update ProfileUpdate, clock func() time.Time) (*OperationalProfile, error) {
	if err := update.Validate(); err != nil {
		return nil, err
	}
	if p.ProfileVersion != ProfileVersion {
		return nil, fmt.Errorf("%w: profile version %q is not supported (supported: %s)", ErrProfileInvalid, p.ProfileVersion, ProfileVersion)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if clock != nil {
		now = clock().UTC().Format(time.RFC3339)
	}
	current, exists := p.Values[update.Field]

	if update.Provenance == ProvenanceObserved {
		if exists && current.Known() && update.Value >= current.Value {
			return nil, fmt.Errorf("%w: %s observed value %d is not restrictive against the effective value %d", ErrObservedCannotRaise, update.Field, update.Value, current.Value)
		}
	} else if update.Provenance == ProvenanceConfigured {
		if exists && current.Known() && update.Value >= current.Value &&
			(current.Provenance == ProvenanceObserved || current.Provenance == ProvenanceAuthoritative) {
			return nil, fmt.Errorf("%w: configured bound %s=%d would undo the %s effective value %d for the same unchanged identity", ErrProfileReplayUndo, update.Field, update.Value, current.Provenance, current.Value)
		}
	}

	next := p.clone()
	value := ProfileValue{Value: update.Value, Provenance: update.Provenance, EvidenceRef: strings.TrimSpace(update.EvidenceRef), UpdatedAt: now}
	next.Values[update.Field] = value
	if now > next.UpdatedAt {
		next.UpdatedAt = now
	}
	return next, nil
}

// ApplyConfigured is the typed convenience for operator-declared values
// (used by the composition root when a run starts under a resolved
// capability profile).
func (p *OperationalProfile) ApplyConfigured(field ProfileField, value int64, clock func() time.Time) (*OperationalProfile, error) {
	return p.Apply(ProfileUpdate{Field: field, Value: value, Provenance: ProvenanceConfigured}, clock)
}

// Effective returns the effective value and provenance of one field. Zero
// value with unknown provenance means there is no information.
func (p *OperationalProfile) Effective(field ProfileField) ProfileValue {
	if p == nil {
		return ProfileValue{}
	}
	return p.Values[field]
}

// clone returns a defensive copy: profile state is never shared mutably.
func (p *OperationalProfile) clone() *OperationalProfile {
	copied := &OperationalProfile{
		ProfileKey:     p.ProfileKey,
		ProviderID:     p.ProviderID,
		ConfigIdentity: p.ConfigIdentity,
		Model:          p.Model,
		ProtocolFamily: p.ProtocolFamily,
		ProfileVersion: p.ProfileVersion,
		Values:         make(map[ProfileField]ProfileValue, len(p.Values)),
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
	}
	for field, value := range p.Values {
		copied.Values[field] = value
	}
	return copied
}

// Sanitized renders the profile identity WITHOUT any secret-bearing content.
// Evidence references are shown only in their sanitized form (the profile
// never carries private evidence payloads).
func (p *OperationalProfile) Sanitized() string {
	if p == nil {
		return "operational profile: <none>"
	}
	fields := make([]string, 0, len(p.Values))
	for field, value := range p.Values {
		fields = append(fields, fmt.Sprintf("%s=%d/%s", field, value.Value, value.Provenance))
	}
	sort.Strings(fields)
	return fmt.Sprintf("OperationalProfile{key:%s provider:%s family:%s model:%q config:%s version:%s values:%v}",
		shortKey(p.ProfileKey), p.ProviderID, p.ProtocolFamily, p.Model, sanitizedIdentityRef(p.ConfigIdentity), p.ProfileVersion, fields)
}

// shortKey keeps inspection output bounded while staying traceable via the
// full key persisted alongside.
func shortKey(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:16] + "…"
}

// sanitizedIdentityRef is defense in depth: rendering reuses the
// Config.Sanitized value, which is already credential-free by construction.
func sanitizedIdentityRef(identity string) string {
	return identity
}
