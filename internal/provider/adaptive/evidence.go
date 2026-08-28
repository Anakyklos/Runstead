// Package adaptive is the PURE provider-neutral contract for conservative
// operational envelope learning (issue #93). It receives ONE typed,
// sanitized attempt observation and produces ONLY deterministic,
// conservative OperationalProfile updates.
//
// Hard rules:
//
//   - Success and ambiguity never produce updates.
//   - Absent evidence never becomes information: every numeric limit it
//     emits must have been PROVEN by a machine-readable typed signal; the
//     package never invents, estimates or extrapolates numbers.
//   - Updates are always tightening in the profile's safety direction;
//     relaxation requires configured/authoritative provenance elsewhere.
//   - Every update carries the structured evidence reference of the attempt
//     that produced it; without a valid reference nothing is emitted.
//   - No free text, prompts, responses, headers or secrets can be
//     represented by Evidence: the package carries integers, closed enums
//     and a structured reference only.
//
// This package imports no adapters, governor, agent, store or CLI code, so
// the mapping is unit-testable without any network or execution machinery.
package adaptive

import (
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Kind is the closed set of attempt outcomes that may justify learning.
// Anything outside this set is ambiguous for envelope purposes and must be
// represented as KindAmbiguous (which never learns).
type Kind string

const (
	// KindRateLimited is deterministic rate/capacity evidence (for example
	// a typed 429 family error carrying a Retry-After signal).
	KindRateLimited Kind = "rate_limited"
	// KindRequestTooLarge is typed evidence that a request exceeded the
	// provider's proven input limit.
	KindRequestTooLarge Kind = "request_too_large"
	// KindOutputTooLarge is typed evidence that the provider only produced
	// output beyond its proven limit.
	KindOutputTooLarge Kind = "output_too_large"
	// KindCapacityRestricted is typed evidence of a PROVEN numeric
	// concurrent-request ceiling. Concurrency is not runtime enforcement
	// material today (the governor is serialized by contract), but a proven
	// ceiling is conservative profile metadata and must still generalize to
	// ids that become enforced.
	KindCapacityRestricted Kind = "capacity_restricted"
	// KindUnsupportedOption is typed evidence that a closed request option
	// was refused as unsupported by this identity.
	KindUnsupportedOption Kind = "unsupported_option"
	// KindSuccess is an outcome without evidence: it never learns.
	KindSuccess Kind = "success"
	// KindAmbiguous is an uncertain outcome: it never learns.
	KindAmbiguous Kind = "ambiguous"
)

// Valid reports whether k is a closed adaptive evidence kind.
func (k Kind) Valid() bool {
	switch k {
	case KindRateLimited, KindRequestTooLarge, KindOutputTooLarge, KindCapacityRestricted, KindUnsupportedOption, KindSuccess, KindAmbiguous:
		return true
	}
	return false
}

// Option is the CLOSED set of request options the contract can represent as
// deterministically proven unsupported. Each option maps to exactly one bit
// in the provider profile's unsupported_options mask. Unknown options are
// not representable: they stay silent rather than being guessed.
type Option string

const (
	// OptionResponseFormat is the response-format option (for example the
	// Messages-style unsupported_response_format signal).
	OptionResponseFormat Option = "response_format"
)

// Valid reports whether o is a closed adaptive option.
func (o Option) Valid() bool { return o == OptionResponseFormat }

// OptionBit returns the single profile bit for one closed option, and zero
// for anything not representable.
func OptionBit(o Option) int64 {
	switch o {
	case OptionResponseFormat:
		return 1 << 0
	}
	return 0
}

// Evidence is ONE typed, sanitized observation of one governed physical
// attempt. Every numeric field is a PROVEN machine-readable limit; zero
// means unknown/absent and is never treated as information. The type
// deliberately carries no strings beyond the closed enums and the structured
// evidence reference: prompts, responses, raw headers and secrets cannot be
// represented.
type Evidence struct {
	// Kind is the closed outcome kind.
	Kind Kind
	// RetryAfter is the authoritative wait proven by the provider (from a
	// sanitized Retry-After/reset signal). Zero means unknown.
	RetryAfter time.Duration
	// MaxRequestBytes is a proven numeric input size limit. Zero means not
	// proven; it is never estimated from content.
	MaxRequestBytes int64
	// MaxOutputBytes is a proven numeric output size limit. Zero means not
	// proven.
	MaxOutputBytes int64
	// RequestsPerMinute is a proven numeric per-minute request ceiling.
	// Zero means not proven; it is NEVER fabricated or derived.
	RequestsPerMinute int64
	// ConcurrencyCeiling is a proven numeric concurrent-request ceiling.
	// Zero means not proven.
	ConcurrencyCeiling int64
	// UnsupportedOption is the closed option proven unsupported.
	UnsupportedOption Option
	// EvidenceRef identifies the attempt that produced the observation; it
	// must be a valid structured reference or nothing is learned (absence
	// of an auditable reference never becomes information).
	EvidenceRef provider.EvidenceRef
}

// Updates converts one observation into the deterministic conservative
// profile updates it can prove. The function is TOTAL: it never returns an
// error, it simply returns nothing for outcomes that do not justify
// learning. Success and ambiguity never produce updates, and an observation
// without a valid structured evidence reference produces nothing.
func Updates(in Evidence) []provider.ProfileUpdate {
	if !in.EvidenceRef.Valid() {
		return nil
	}
	if !in.Kind.Valid() {
		return nil
	}
	switch in.Kind {
	case KindSuccess, KindAmbiguous:
		return nil
	}

	var updates []provider.ProfileUpdate
	emit := func(field provider.ProfileField, value int64) {
		updates = append(updates, provider.ProfileUpdate{
			Field:       field,
			Value:       value,
			Provenance:  provider.ProvenanceObserved,
			EvidenceRef: in.EvidenceRef,
		})
	}

	switch in.Kind {
	case KindRateLimited:
		// The cooldown wait proven by the provider's own signal is the only
		// generally available proven pace evidence. A missing signal is
		// NEVER turned into an invented value.
		if millis := in.RetryAfter.Milliseconds(); millis > 0 {
			emit(provider.FieldCooldownMillis, millis)
		}
		// A proven per-minute ceiling is only persisted when it was actually
		// proven by a typed numeric signal; no adapter populates this today
		// (documented limitation), so production never guesses an RPM.
		if in.RequestsPerMinute > 0 {
			emit(provider.FieldRequestsPerMinute, in.RequestsPerMinute)
		}
	case KindRequestTooLarge:
		if in.MaxRequestBytes > 0 {
			emit(provider.FieldMaxRequestBytes, in.MaxRequestBytes)
		}
	case KindOutputTooLarge:
		if in.MaxOutputBytes > 0 {
			emit(provider.FieldMaxResponseBytes, in.MaxOutputBytes)
		}
	case KindCapacityRestricted:
		if in.ConcurrencyCeiling > 0 {
			emit(provider.FieldConcurrencyCeiling, in.ConcurrencyCeiling)
		}
	case KindUnsupportedOption:
		if bit := OptionBit(in.UnsupportedOption); bit != 0 {
			emit(provider.FieldUnsupportedOptions, bit)
		}
	}
	return updates
}

// ConservativeSubset filters updates down to exactly those the current
// profile would accept, so the durable boundary (which fails closed on a
// non-conservative proposal) only ever receives justifiable tightening.
// Updates that the profile already covers more conservatively, or that
// would not apply, are dropped: the goal state (conservative profile) is
// already reached, so dropping them is both safe and mandatory. A nil
// profile means unknown state, where any emitted update is accepted. The
// clock is only used for the rule engine's bookkeeping fields (discarded
// here); nil defaults to the real clock, keeping call sites simple.
func ConservativeSubset(current *provider.OperationalProfile, updates []provider.ProfileUpdate, clock func() time.Time) []provider.ProfileUpdate {
	if len(updates) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if clock != nil {
		now = clock().UTC().Format(time.RFC3339)
	}
	accepted := make([]provider.ProfileUpdate, 0, len(updates))
	for _, update := range updates {
		if current == nil {
			accepted = append(accepted, update)
			continue
		}
		fieldValue, exists := current.Values[update.Field]
		if _, err := provider.ApplyFieldValue(update.Field, fieldValue, exists, update, now); err != nil {
			// Not conservatively applicable against the effective state:
			// the profile is already as conservative or more, or the update
			// cannot be justified (for example an undefined direction).
			continue
		}
		accepted = append(accepted, update)
	}
	return accepted
}
