package provider

// Operational profile (#91): durable, versioned operational metadata for one
// combination of SANITIZED provider/config identity + exact model + protocol
// family, with explicit evidence provenance. A profile is OPERATIONAL
// METADATA only: it is not task truth, not policy, not approval, not retry
// authority, not verifier state and never model-controlled state.
//
// Guarantees enforced by the update rules (ApplyFieldValue):
//   - evidence can make the runtime automatically MORE conservative, in the
//     explicit per-field safety direction (FieldSafetyDirection); ordinary
//     successful observations NEVER raise a ceiling or weaken a bound;
//   - raising/weakening a value for the same unchanged identity requires
//     operator configuration or an explicitly typed authoritative update;
//   - unknown stays unknown (zero value / absent row) and is never
//     fabricated into information; a value of zero is refused for every
//     writable provenance.
//
// The profile cannot execute retries, cannot grant admission, cannot change
// provider/model/fallback and never contains credentials, prompts or private
// response bodies. The profile defines NO admission policy of its own: it
// represents values with provenance and lets the governor/adapters enforce
// the runtime's existing contracts.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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

// ErrObservedNotConservative is returned when observed evidence would move a
// field AWAY from its conservative direction (raising a lower-is-conservative
// bound, or lowering a higher-is-conservative bound) or when a field has no
// defined automatic safety direction: ordinary observations can only tighten
// or fill unknown (#91/#92).
var ErrObservedNotConservative = errors.New("observed evidence is not conservative for this field")

// ErrProfileReplayUndo is returned when a configured-replay update (the same
// unchanged identity re-supplying its declared bounds) would undo an
// effective observed tightening or authoritative acceptance. It is a
// benign no-op signal for the composition root: the profile is left intact.
var ErrProfileReplayUndo = errors.New("configured replay would undo observed/authoritative effective value")

// ErrNoAutomaticDirection is returned when observed evidence targets a field
// whose automatic safety direction is not defined (timeout_millis): the
// profile can represent configured/authoritative values for such fields, but
// never auto-adjusts them from observations (#91 review).
var ErrNoAutomaticDirection = errors.New("field has no defined automatic safety direction for observations")

// ErrInvalidEvidenceRef marks an evidence reference that is not a structured,
// sanitized identifier: evidence references are NEVER free text and cannot
// carry prompts, response bodies, headers or credentials (#91 review).
var ErrInvalidEvidenceRef = errors.New("invalid sanitized evidence reference")

// EvidenceKind names the structured kind of a sanitized evidence reference.
type EvidenceKind string

const (
	// EvidenceKindEvidence references a persisted observation evidence id
	// (obs-NNNNNN).
	EvidenceKindEvidence EvidenceKind = "evidence"
	// EvidenceKindExecution references a Runstead execution id (exec-NNNNNN).
	EvidenceKindExecution EvidenceKind = "execution"
	// EvidenceKindVerification references a verification attempt id.
	EvidenceKindVerification EvidenceKind = "verification"
	// EvidenceKindTask references a durable task id.
	EvidenceKindTask EvidenceKind = "task"
)

// Valid reports whether k is a known evidence kind.
func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceKindEvidence, EvidenceKindExecution, EvidenceKindVerification, EvidenceKindTask:
		return true
	default:
		return false
	}
}

// closedEvidenceIDPatterns restrict each EvidenceKind to the CLOSED set of
// canonical identifier formats the Runstead runtime actually produces
// (#91 review). The counter generator uses fmt.Sprintf("%s-%06d",
// prefix, next): %06d is a MINIMUM width, NOT a maximum, so counters
// 0..999999 render as exactly six zero-padded digits and counters
// >= 1000000 render with more digits and no leading zero. Any shorter
// form or extraneous leading zeros is not a Runstead-produced id.
const counterIDPattern = `(?:[0-9]{6}|[1-9][0-9]{5,})`

//   - evidence/execution/verification: "<prefix>-" + counterIDPattern
//     (obs-000001, exec-000001, verif-1000000, ...);
//   - task ids are the CLI-generated "cli-"+unix-nanos (digits only,
//     no leading zero).
//
// A token that merely has no spaces (for example "privatePromptBody" or
// "sensitive-response-content") is NOT a Runstead-produced identifier and is
// refused: private content has no representation it can smuggle through.
var closedEvidenceIDPatterns = map[EvidenceKind]*regexp.Regexp{
	EvidenceKindEvidence:     regexp.MustCompile("^obs-" + counterIDPattern + "$"),
	EvidenceKindExecution:    regexp.MustCompile("^exec-" + counterIDPattern + "$"),
	EvidenceKindVerification: regexp.MustCompile("^verif-" + counterIDPattern + "$"),
	EvidenceKindTask:         regexp.MustCompile(`^cli-[1-9][0-9]{9,18}$`),
}

// EvidenceRef is a STRUCTURED, sanitized reference to evidence that produced
// a value. It is deliberately not free text: only a known kind plus a
// conservative identifier may be persisted, so private content (prompts,
// response bodies, raw headers, credentials) has no representation it can
// smuggle through (#91 review).
type EvidenceRef struct {
	Kind EvidenceKind
	ID   string
}

// Valid reports whether the reference is a complete reference whose ID
// matches the CLOSED identifier format of its kind. The zero value (absent
// reference) is invalid.
func (r EvidenceRef) Valid() bool {
	pattern, ok := closedEvidenceIDPatterns[r.Kind]
	if !ok {
		return false
	}
	id := r.ID
	if id == "" || id != strings.TrimSpace(id) {
		return false
	}
	return pattern.MatchString(id)
}

// String renders the structured reference as "kind:id" for persistence and
// inspection. The zero value renders empty (absent reference).
func (r EvidenceRef) String() string {
	if r.Kind == "" {
		return ""
	}
	return string(r.Kind) + ":" + r.ID
}

// ParseEvidenceRef parses a persisted "kind:id" reference. The empty string
// is the ABSENT reference (no evidence). Anything that is not a known kind
// producing a Runstead identifier fails closed. Error messages are strictly
// generic and NEVER reproduce the candidate value: an invalid candidate may
// be private content, so nothing about it is echoed back (#91 review).
func ParseEvidenceRef(value string) (EvidenceRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return EvidenceRef{}, nil
	}
	colon := strings.Index(value, ":")
	if colon <= 0 || colon == len(value)-1 {
		return EvidenceRef{}, fmt.Errorf("%w: reference is not a structured kind:id value", ErrInvalidEvidenceRef)
	}
	kind := EvidenceKind(value[:colon])
	id := value[colon+1:]
	ref := EvidenceRef{Kind: kind, ID: id}
	if !ref.Valid() {
		return EvidenceRef{}, fmt.Errorf("%w: id does not satisfy the closed per-kind identifier format", ErrInvalidEvidenceRef)
	}
	return ref, nil
}

// EvidenceEvidenceRef / ExecutionEvidenceRef / VerificationEvidenceRef /
// TaskEvidenceRef are the typed constructors so sanitized references are
// born from a Runstead-produced id space. The reference is still validated
// (Valid) before it can be applied or persisted.
func EvidenceEvidenceRef(id string) EvidenceRef {
	return EvidenceRef{Kind: EvidenceKindEvidence, ID: id}
}

func ExecutionEvidenceRef(id string) EvidenceRef {
	return EvidenceRef{Kind: EvidenceKindExecution, ID: id}
}

func VerificationEvidenceRef(id string) EvidenceRef {
	return EvidenceRef{Kind: EvidenceKindVerification, ID: id}
}

func TaskEvidenceRef(id string) EvidenceRef {
	return EvidenceRef{Kind: EvidenceKindTask, ID: id}
}

// Provenance is the typed origin of one persisted profile value. The four
// semantics are distinct and fail-closed: the zero value is invalid.
type Provenance string

const (
	// ProvenanceUnknown means no information exists; the effective value is
	// absent (zero/row-absent) and nothing is guessed. It is a valid STATE
	// but never a writable origin.
	ProvenanceUnknown Provenance = "unknown"
	// ProvenanceConfigured is operator-declared configuration evidence (for
	// example capability-profile bounds).
	ProvenanceConfigured Provenance = "configured"
	// ProvenanceObserved is concrete runtime evidence actually produced
	// (restrictive limits, retry-after envelopes), referenced by its
	// sanitized evidence id.
	ProvenanceObserved Provenance = "observed"
	// ProvenanceAuthoritative is evidence accepted through an explicitly
	// typed, contract-reviewed path (for example an operator-accepted
	// evidence record); it is not bounded by invented profile-only caps and
	// remains the highest authority for a field's value.
	ProvenanceAuthoritative Provenance = "authoritative"
)

// Valid reports whether p is one of the known WRITABLE provenance values.
// unknown/zero/absent are valid states but never update origins.
func (p Provenance) Valid() bool {
	switch p {
	case ProvenanceConfigured, ProvenanceObserved, ProvenanceAuthoritative:
		return true
	default:
		return false
	}
}

// ParseProvenance parses an operator/evidence-supplied provenance name,
// failing closed on empty, unknown or absent-state values.
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
	// FieldTimeoutMillis is a timeout/latency representation WITHOUT an
	// automatic safety direction: the profile can carry configured or
	// authoritative values for it, but observations never auto-adjust it
	// (no assumption that a smaller timeout is always safer).
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

// FieldSafetyDirection declares, per field, which direction is MORE
// conservative. Automatic observed tightening is only defined where this
// direction is explicit (#91 review).
type FieldSafetyDirection int

const (
	// DirectionLowerIsConservative: smaller effective values are safer
	// (request/output bytes, requests per minute, concurrency ceiling).
	DirectionLowerIsConservative FieldSafetyDirection = iota
	// DirectionHigherIsConservative: larger effective values are safer
	// (cooldown wait after rate limiting).
	DirectionHigherIsConservative
	// DirectionNoAutomatic means the field's safety semantics are NOT
	// assumed; observations are never auto-applied to it.
	DirectionNoAutomatic
)

// SafetyDirection returns the explicit conservative direction of one field.
func SafetyDirection(field ProfileField) FieldSafetyDirection {
	switch field {
	case FieldMaxRequestBytes, FieldMaxResponseBytes, FieldRequestsPerMinute, FieldConcurrencyCeiling:
		return DirectionLowerIsConservative
	case FieldCooldownMillis:
		return DirectionHigherIsConservative
	case FieldTimeoutMillis:
		return DirectionNoAutomatic
	default:
		return DirectionNoAutomatic
	}
}

// ProfileValue is one persisted variable value with its provenance. The
// representation is single: a KNOWN value is always positive; zero value
// (or absent row) means UNKNOWN and is never fabricable.
type ProfileValue struct {
	Value      int64
	Provenance Provenance
	// EvidenceRef is the STRUCTURED, sanitized evidence reference (kind:id)
	// used only when the provenance is observed/authoritative. It is never
	// free text and never a copy of private evidence content (#91 review).
	EvidenceRef EvidenceRef
	// UpdatedAt is the RFC3339 UTC time of the last update to this field.
	UpdatedAt string
}

// Known reports whether the field carries an effective (positive) value.
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
// identity is fixed at construction; only a field value with provenance and
// evidence may be proposed.
type ProfileUpdate struct {
	Field       ProfileField
	Value       int64
	Provenance  Provenance
	EvidenceRef EvidenceRef
}

// Validate checks one update for internal consistency BEFORE it reaches the
// rule engine: unknown provenance, unknown field, non-positive values (zero
// means unknown/absent and is never persisted) and missing evidence
// references for observed/authoritative provenance all fail closed.
func (u ProfileUpdate) Validate() error {
	if !IsProfileField(u.Field) {
		return fmt.Errorf("%w: unknown profile field %q", ErrProfileInvalid, string(u.Field))
	}
	if !u.Provenance.Valid() {
		return fmt.Errorf("%w: provenance %q is not a valid update origin (unknown/zero values are never written)", ErrProfileInvalid, string(u.Provenance))
	}
	if u.Value <= 0 {
		return fmt.Errorf("%w: %s value must be positive (a zero value means unknown/absent and is never persisted)", ErrProfileInvalid, u.Field)
	}
	if u.Provenance == ProvenanceObserved || u.Provenance == ProvenanceAuthoritative {
		if !u.EvidenceRef.Valid() {
			return fmt.Errorf("%w: %s with provenance %q requires a structured sanitized evidence reference (kind:id without free text)", ErrProfileInvalid, u.Field, u.Provenance)
		}
	} else if u.Provenance == ProvenanceConfigured && u.EvidenceRef.Kind != "" {
		// configured is operator declaration, never evidence-derived: an
		// evidence reference on configured provenance is a contradiction
		// and must fail before the rules (it would otherwise be persisted
		// and only discovered at load) (#91 review).
		return fmt.Errorf("%w: %s with provenance %q must not carry an evidence reference", ErrProfileInvalid, u.Field, u.Provenance)
	}
	return nil
}

// moreConservative reports whether candidate is strictly more conservative
// than current for the field's explicit safety direction.
func moreConservative(field ProfileField, current, candidate int64) bool {
	switch SafetyDirection(field) {
	case DirectionHigherIsConservative:
		return candidate > current
	default:
		// lower-is-conservative is the default direction for bounded fields.
		return candidate < current
	}
}

// ApplyFieldValue is the SINGLE deterministic rule engine shared by the
// in-memory profile AND the durable-state boundary (issues #91/#92). It
// decides whether an update is acceptable against one field's current state
// and returns the new field value. current is the absent/unknown state when
// exists is false.
//
// Rules:
//
//	unknown -> configured            (operator declaration)
//	unknown -> observed              (a specific, actually produced observation,
//	                                 with evidence reference)
//	unknown -> authoritative         (explicitly typed path)
//	observed, direction defined     ACCEPTED only when strictly more
//	                                 conservative for this field; otherwise
//	                                 ErrObservedNotConservative
//	observed, no automatic direction REFUSED (ErrNoAutomaticDirection)
//	configured over configured       supersedes
//	configured over observed/...     ACCEPTED only when strictly more
//	                                 conservative; otherwise ErrProfileReplayUndo
//	                                 (a replay of the same unchanged identity
//	                                 must not undo tightening/acceptance)
//	authoritative                    ACCEPTED (explicitly typed path)
func ApplyFieldValue(field ProfileField, current ProfileValue, exists bool, update ProfileUpdate, now string) (ProfileValue, error) {
	if err := update.Validate(); err != nil {
		return ProfileValue{}, err
	}
	// The automatic-safety-direction check precedes the unknown fast-path: a
	// field with NO defined conservative direction (timeout_millis) never
	// accepts observations, not even as its first value. An absent field is
	// not a license to apply an observation whose safety semantics are
	// undefined (#91 review).
	if update.Provenance == ProvenanceObserved && SafetyDirection(field) == DirectionNoAutomatic {
		return ProfileValue{}, fmt.Errorf("%w: %s (no automatic safety direction)", ErrNoAutomaticDirection, field)
	}
	proposed := ProfileValue{Value: update.Value, Provenance: update.Provenance, EvidenceRef: update.EvidenceRef, UpdatedAt: now}

	if !exists || !current.Known() {
		// unknown/absent can receive any writable provenance through its
		// typed path (subject to the checks above).
		return proposed, nil
	}
	if update.Provenance == ProvenanceObserved {
		if !moreConservative(field, current.Value, update.Value) {
			return ProfileValue{}, fmt.Errorf("%w: %s observed value %d is not conservative against the effective value %d", ErrObservedNotConservative, field, update.Value, current.Value)
		}
		return proposed, nil
	}
	if update.Provenance == ProvenanceConfigured {
		if current.Provenance == ProvenanceObserved || current.Provenance == ProvenanceAuthoritative {
			if !moreConservative(field, current.Value, update.Value) {
				return ProfileValue{}, fmt.Errorf("%w: configured bound %s=%d would undo the %s effective value %d for the same unchanged identity", ErrProfileReplayUndo, field, update.Value, current.Provenance, current.Value)
			}
		}
		return proposed, nil
	}
	// authoritative: the explicitly typed, contract-reviewed path.
	return proposed, nil
}

// Apply executes the deterministic update rules and returns a NEW profile
// state. The receiver is never mutated: profiles are pure state so no side
// effect (network, retry, admission) is even possible.
func (p *OperationalProfile) Apply(update ProfileUpdate, clock func() time.Time) (*OperationalProfile, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil profile", ErrProfileInvalid)
	}
	if p.ProfileVersion != ProfileVersion {
		return nil, fmt.Errorf("%w: profile version %q is not supported (supported: %s)", ErrProfileInvalid, p.ProfileVersion, ProfileVersion)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if clock != nil {
		now = clock().UTC().Format(time.RFC3339)
	}
	current, exists := p.Values[update.Field]
	nextValue, err := ApplyFieldValue(update.Field, current, exists, update, now)
	if err != nil {
		return nil, err
	}
	next := p.clone()
	next.Values[update.Field] = nextValue
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
		shortKey(p.ProfileKey), p.ProviderID, p.ProtocolFamily, p.Model, p.ConfigIdentity, p.ProfileVersion, fields)
}

// shortKey keeps inspection output bounded while staying traceable via the
// full key persisted alongside.
func shortKey(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:16] + "…"
}
