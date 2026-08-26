package provider

import "fmt"

// Capability is one provider capability Runstead may require before protected
// execution. The set is deliberately narrow: only capabilities the runtime
// actually gates on are represented (#79). Do not grow it into a feature
// catalog.
type Capability string

const (
	// CapabilityTextTurn is the plain text/model turn every family provides.
	// A resolved configuration always proves it, but the explicit constant
	// keeps required-capability declarations readable.
	CapabilityTextTurn Capability = "text_turn"
	// CapabilityRunsteadProtocol means the endpoint has been validated to
	// carry the runstead.protocol.v1 text action contract reliably. Declared
	// protocol families do NOT prove this by name; it must be asserted by the
	// operator/evidence through the capability profile.
	CapabilityRunsteadProtocol Capability = "runstead_protocol"
	// CapabilityNativeTools means native provider tool calls are proven
	// supported for this endpoint. It is opt-in evidence, never inferred from
	// the protocol family or vendor name.
	CapabilityNativeTools Capability = "native_tools"
	// CapabilityStreaming means response streaming is supported.
	CapabilityStreaming Capability = "streaming"
	// CapabilityCancellation means in-flight request cancellation is supported
	// by the endpoint/adapter combination.
	CapabilityCancellation Capability = "cancellation"
)

// Capabilities is a set of declared capabilities. Its zero value is empty:
// nothing is supported until explicitly declared, which fails closed when a
// capability is later required (#79).
//
// The set is CLOSED: only capabilities in KnownCapabilities may appear as
// keys. Anything else is refused by Validate, so an unknown capability can
// never silently satisfy a gate. The type does not model retry/amplification
// safety or delivery-uncertainty semantics: those remain owned exclusively by
// RouteSafety and DeliveryState, the existing executable sources of truth.
type Capabilities map[Capability]bool

// knownCapabilities is the closed vocabulary of capabilities this contract
// version understands. It must contain exactly the Capability constants above.
var knownCapabilities = map[Capability]struct{}{
	CapabilityTextTurn:         {},
	CapabilityRunsteadProtocol: {},
	CapabilityNativeTools:      {},
	CapabilityStreaming:        {},
	CapabilityCancellation:     {},
}

// IsKnown reports whether cap is part of the closed capability vocabulary.
func IsKnown(cap Capability) bool {
	_, ok := knownCapabilities[cap]
	return ok
}

// Validate refuses any capability outside the closed vocabulary. An unknown
// key would otherwise be able to pose as a satisfiable requirement, so the
// declaration fails closed before dispatch (#79).
func (c Capabilities) Validate() error {
	for cap := range c {
		if !IsKnown(cap) {
			return fmt.Errorf("unknown capability %q is not part of this contract's closed capability set", string(cap))
		}
	}
	return nil
}

func (c Capabilities) Has(cap Capability) bool { return c[cap] }

// SupportsRequired reports whether every capability in required is explicitly
// declared. Unknown capabilities fail closed on BOTH sides: a requirement
// outside the closed vocabulary is refused even before consulting the set, and
// an absent capability is reported by name, so an incompatible endpoint is
// refused before any dispatch with provider/family/capability identified.
func (c Capabilities) SupportsRequired(required []Capability) error {
	for _, cap := range required {
		if !IsKnown(cap) {
			return fmt.Errorf("required capability %q is unknown to this contract version; refusing to interpret it", string(cap))
		}
		if !c.Has(cap) {
			return fmt.Errorf("required capability %q is not declared for this provider", cap)
		}
	}
	return nil
}

// CapabilityProfile is the explicit, versioned compatibility profile of one
// configured provider endpoint. It represents only what Runstead needs to know
// before execution; it is never inferred from the vendor name, the base URL or
// the declared protocol family.
//
// ProfileVersion identifies the profile schema/content version so operators
// can re-prove endpoints when the meaning of a profile changes.
type CapabilityProfile struct {
	ProfileVersion string
	Capabilities   Capabilities
	// RouteSafety carries the attempt/amplification behavior declared for this
	// endpoint. It must validate and be admitted against the governor's route
	// safety; see Config.Validate and governor admission.
	RouteSafety RouteSafety
	// MaxRequestBytes/MaxResponseBytes are known size constraints in bytes.
	// Zero means "no configured bound", which validation may reject where a
	// bound is mandatory.
	MaxRequestBytes  int
	MaxResponseBytes int
}

// profileContractVersion identifies the capability contract version. The
// closed vocabulary lives in knownCapabilities; growing it bumps this version.
const profileContractVersion = 1

// Validate checks the profile's internal consistency. Unknown values fail
// closed: an undeclared profile version, a capability outside the closed
// vocabulary, an invalid RouteSafety declaration or negative bounds refuse the
// configuration before dispatch.
func (p CapabilityProfile) Validate() error {
	if p.ProfileVersion == "" {
		return fmt.Errorf("capability profile version must not be empty")
	}
	if p.ProfileVersion != fmt.Sprintf("v%d", profileContractVersion) {
		return fmt.Errorf("unsupported capability profile version %q", p.ProfileVersion)
	}
	if err := p.Capabilities.Validate(); err != nil {
		return fmt.Errorf("capability profile declares an unusable capability set: %w", err)
	}
	if err := p.RouteSafety.Validate(); err != nil {
		return fmt.Errorf("capability profile route safety is unsafe: %w", err)
	}
	if p.MaxRequestBytes < 0 || p.MaxResponseBytes < 0 {
		return fmt.Errorf("capability profile size bounds must not be negative")
	}
	return nil
}

// Supported returns the declared capabilities. Callers gate execution on
// RequiredCapabilities instead of trusting the full set.
func (p CapabilityProfile) Supported() Capabilities {
	if p.Capabilities == nil {
		return Capabilities{}
	}
	return p.Capabilities
}

// RequiredCapabilities returns the minimal capability set demanded for a
// governed Complete on any supported family. Native tools are deliberately
// excluded: Runstead executes its own text action protocol and never requires
// provider-native tool calling (#79).
func RequiredCapabilities() []Capability {
	return []Capability{CapabilityTextTurn, CapabilityRunsteadProtocol}
}
