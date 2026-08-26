package provider

import (
	"fmt"
	"strings"
)

// ProtocolFamily identifies the wire-protocol compatibility family a provider
// endpoint implements. It is a property of the protocol adapter, never of the
// operator-chosen provider identity and never of the task execution loop.
//
// A family names a protocol contract (for example "OpenAI-compatible chat
// completion APIs"), not a concrete vendor: official OpenAI, Anthropic and
// Google endpoints are only example implementations of these families. The
// agent loop must never branch on a vendor name or on this value; family
// dispatch belongs exclusively to the provider layer (#79/#86).
type ProtocolFamily string

const (
	// FamilyOpenAICompatible covers OpenAI-compatible chat-completion style
	// APIs. Any endpoint implementing the family sufficiently may be declared
	// with it; it does not imply the official OpenAI service.
	FamilyOpenAICompatible ProtocolFamily = "openai_compatible"
	// FamilyAnthropicCompatible covers Anthropic Messages-style APIs. It does
	// not imply the official Anthropic service.
	FamilyAnthropicCompatible ProtocolFamily = "anthropic_compatible"
	// FamilyGoogleCompatible covers Google Gemini generateContent-style APIs.
	// It does not imply the official Google service.
	FamilyGoogleCompatible ProtocolFamily = "google_compatible"
)

// ProtocolFamilies lists every supported protocol family. Adapters are added
// per family by #87/#88/#89; adding a family is a provider-layer change that
// must not require agent-loop changes.
var ProtocolFamilies = []ProtocolFamily{
	FamilyOpenAICompatible,
	FamilyAnthropicCompatible,
	FamilyGoogleCompatible,
}

func (f ProtocolFamily) String() string { return string(f) }

// Valid reports whether f is one of the supported compatibility families. The
// zero value is invalid and fails closed everywhere it is checked.
func (f ProtocolFamily) Valid() bool {
	switch f {
	case FamilyOpenAICompatible, FamilyAnthropicCompatible, FamilyGoogleCompatible:
		return true
	default:
		return false
	}
}

// ParseProtocolFamily parses an operator-supplied protocol family name. It
// returns an error for unknown or empty values so bad configuration fails
// before any dispatch instead of silently selecting a default.
func ParseProtocolFamily(value string) (ProtocolFamily, error) {
	trimmed := strings.TrimSpace(value)
	family := ProtocolFamily(trimmed)
	if !family.Valid() {
		return "", fmt.Errorf("unknown protocol family %q (supported: openai_compatible, anthropic_compatible, google_compatible)", trimmed)
	}
	return family, nil
}
