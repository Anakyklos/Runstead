package provider

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrUnknownProvider is returned when an operator-selected provider ID has no
// configured definition. It must fail before any dispatch.
var ErrUnknownProvider = errors.New("unknown provider id")

// ErrInvalidProviderConfig marks provider configuration that cannot be used
// for protected execution. All resolution/validation errors below wrap it so
// callers can classify pre-dispatch failures without parsing error text.
var ErrInvalidProviderConfig = errors.New("invalid provider configuration")

// SecretRef is a NON-SECRET reference to externally held authentication
// material (for example an env var name, a secret-store key or a credential
// file path). The reference may be persisted; the material it names never
// enters Runstead state, metadata, traces, contract hashes, fixtures or model
// context (#79).
//
// Whether authentication is needed is declared EXPLICITLY through
// Config.AuthRequirement and is never inferred from the protocol family: #86
// includes gateways, local services and third-party endpoints whose family
// matches but which accept unauthenticated traffic.
type SecretRef string

func (r SecretRef) String() string { return string(r) }

// Normalize trims surrounding whitespace. A whitespace-only reference is
// treated as unset so it can never pose as configured authentication material.
func (r SecretRef) Normalize() SecretRef {
	return SecretRef(strings.TrimSpace(string(r)))
}

// AuthRequirement declares explicitly whether one configured provider endpoint
// requires authentication. It is operator-declared evidence about the
// endpoint, never derived from the protocol family name.
type AuthRequirement string

const (
	// AuthReferenceRequired means the endpoint needs authentication material;
	// a non-empty SecretRef must be configured or resolution fails closed.
	AuthReferenceRequired AuthRequirement = "reference_required"
	// AuthNone means the endpoint accepts unauthenticated traffic. Declaring
	// AuthNone together with a SecretRef is refused so declarations stay honest.
	AuthNone AuthRequirement = "none"
)

// Valid reports whether r is one of the known auth requirements. The zero
// value is deliberately invalid: the requirement must be declared, not guessed.
func (r AuthRequirement) Valid() bool {
	switch r {
	case AuthReferenceRequired, AuthNone:
		return true
	default:
		return false
	}
}

// Config is one operator-declared provider endpoint. It separates:
//
//   - ProviderID: the stable operator-facing identity of this configured
//     endpoint. Two different IDs may declare the same ProtocolFamily and are
//     served by the same adapter path with no agent-loop branching;
//   - ProtocolFamily: which compatibility protocol family the endpoint speaks
//     (openai_compatible, anthropic_compatible, google_compatible). Family and
//     identity are distinct concepts; neither implies the other;
//   - BaseURL: the exact endpoint root;
//   - Model: the exact model identifier required for every request;
//   - Auth: non-secret authentication reference (SecretRef); required if and
//     only if AuthRequirement demands it;
//   - AuthRequirement: explicit declaration of whether this endpoint needs
//     authentication. Never inferred from the protocol family;
//   - Options: strictly necessary NON-SECRET protocol options for this
//     endpoint. Values must never carry credential material;
//   - Profile: the explicit versioned capability profile proven for this
//     endpoint, including its RouteSafety declaration;
//   - ConfigVersion: operator-maintained configuration identity, bumped when
//     the meaning of this configuration changes.
//
// Config contains no secret values by construction; Auth is only a reference.
type Config struct {
	ProviderID      string
	ProtocolFamily  ProtocolFamily
	BaseURL         string
	Model           string
	Auth            SecretRef
	AuthRequirement AuthRequirement
	Options         map[string]string
	Profile         CapabilityProfile
	ConfigVersion   string
}

// Sanitized renders the configuration identity without any secret-bearing
// content. Options keys are listed but their values are not rendered: option
// values are untrusted operator input and could accidentally contain
// credential-shaped strings. Safe to persist, trace or embed in diagnostics.
func (c Config) Sanitized() string {
	optionKeys := make([]string, 0, len(c.Options))
	for key := range c.Options {
		optionKeys = append(optionKeys, key)
	}
	// Deterministic ordering keeps sanitized identities stable across runs.
	for i := 1; i < len(optionKeys); i++ {
		for j := i; j > 0 && optionKeys[j] < optionKeys[j-1]; j-- {
			optionKeys[j], optionKeys[j-1] = optionKeys[j-1], optionKeys[j]
		}
	}
	return fmt.Sprintf("provider.Config{ProviderID:%q ProtocolFamily:%q BaseURL:%q Model:%q AuthRequirement:%q AuthRef:%t Options:%v ProfileVersion:%q RouteSafety:%#v ConfigVersion:%q}",
		c.ProviderID, c.ProtocolFamily, c.BaseURL, c.Model, string(c.AuthRequirement), c.Auth != "", optionKeys,
		c.Profile.ProfileVersion, c.Profile.RouteSafety, c.ConfigVersion)
}

func (c Config) String() string   { return c.Sanitized() }
func (c Config) GoString() string { return c.Sanitized() }

// Resolved is the validated result of resolving a Config against the registry
// before any dispatch. Every field is normalized and proven; a Resolved value
// existing means the provider passed all pre-flight contract checks.
type Resolved struct {
	ProviderID      string
	ProtocolFamily  ProtocolFamily
	BaseURL         string
	Model           string
	Auth            SecretRef
	AuthRequirement AuthRequirement
	Profile         CapabilityProfile
	ConfigIdentity  string
}

// Validate checks one provider configuration for internal consistency. It
// fails closed on unknown family, missing identity/model/endpoint, unsafe
// route declarations and invalid capability profiles. Authentication
// requirements are enforced at Resolve time, when the required-capability set
// is known.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ProviderID) == "" {
		return fmt.Errorf("%w: provider id must not be empty", ErrInvalidProviderConfig)
	}
	if !c.ProtocolFamily.Valid() {
		return fmt.Errorf("%w: unknown protocol family %q for provider %q", ErrInvalidProviderConfig, string(c.ProtocolFamily), c.ProviderID)
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("%w: base URL must not be empty for provider %q", ErrInvalidProviderConfig, c.ProviderID)
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("%w: base URL for provider %q does not parse", ErrInvalidProviderConfig, c.ProviderID)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: base URL for provider %q must be http(s)", ErrInvalidProviderConfig, c.ProviderID)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: base URL for provider %q must include a host", ErrInvalidProviderConfig, c.ProviderID)
	}
	// The base URL is an endpoint ROOT. Userinfo, query and fragment components
	// are classic credential carriers; refusing them here keeps everything that
	// can reach Sanitized()/ConfigIdentity provably non-secret (#79).
	if parsed.User != nil {
		return fmt.Errorf("%w: base URL for provider %q must not carry userinfo; use the non-secret authentication reference instead", ErrInvalidProviderConfig, c.ProviderID)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("%w: base URL for provider %q must not carry a query string", ErrInvalidProviderConfig, c.ProviderID)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%w: base URL for provider %q must not carry a fragment", ErrInvalidProviderConfig, c.ProviderID)
	}
	// Authentication is validated from the explicit declaration, never from
	// the protocol family name.
	if !c.AuthRequirement.Valid() {
		return fmt.Errorf("%w: provider %q must declare auth_requirement explicitly (%q or %q); it is never inferred from the protocol family", ErrInvalidProviderConfig, c.ProviderID, AuthNone, AuthReferenceRequired)
	}
	auth := c.Auth.Normalize()
	if c.Auth != "" && auth == "" {
		return fmt.Errorf("%w: provider %q has a blank authentication reference", ErrInvalidProviderConfig, c.ProviderID)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("%w: exact model must be configured for provider %q", ErrInvalidProviderConfig, c.ProviderID)
	}
	if err := c.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: provider %q: %v", ErrInvalidProviderConfig, c.ProviderID, err)
	}
	if c.AuthRequirement == AuthNone && auth != "" {
		return fmt.Errorf("%w: provider %q declares auth_requirement none but also configures an authentication reference", ErrInvalidProviderConfig, c.ProviderID)
	}
	for key, value := range c.Options {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: provider %q has an empty option key", ErrInvalidProviderConfig, c.ProviderID)
		}
		// Defense in depth: options are declared non-secret. A value that
		// carries credential shape is refused rather than silently stored.
		if looksCredentialShaped(value) {
			return fmt.Errorf("%w: provider %q option %q must not carry credential-shaped values", ErrInvalidProviderConfig, c.ProviderID, key)
		}
	}
	return nil
}

// looksCredentialShaped reports whether value matches obvious credential
// shapes. It mirrors the persistence redaction patterns conservatively and is
// deliberately over-inclusive.
func looksCredentialShaped(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	for _, marker := range []string{"bearer ", "sk-", "authorization:", "set-cookie:", "api_key=", "apikey=", "password="} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// Registry maps operator-chosen provider IDs to their configurations. Lookup
// of an unknown ID fails closed with ErrUnknownProvider before any dispatch.
type Registry struct {
	configs map[string]Config
}

// NewRegistry builds a registry from operator configurations. Duplicate
// provider IDs are a configuration error, never a silent overwrite.
func NewRegistry(configs ...Config) (*Registry, error) {
	registry := &Registry{configs: make(map[string]Config, len(configs))}
	for _, config := range configs {
		id := strings.TrimSpace(config.ProviderID)
		if _, exists := registry.configs[id]; exists {
			return nil, fmt.Errorf("%w: duplicate provider id %q", ErrInvalidProviderConfig, id)
		}
		registry.configs[id] = config
	}
	return registry, nil
}

// Config returns the configured definition for providerID.
func (r *Registry) Config(providerID string) (Config, error) {
	config, ok := r.configs[strings.TrimSpace(providerID)]
	if !ok {
		return Config{}, fmt.Errorf("%w %q", ErrUnknownProvider, providerID)
	}
	return config, nil
}

// Resolve validates and normalizes the configuration for providerID against
// the required capability set BEFORE any provider code can run. Required cases
// that must fail here: unknown provider ID, unknown protocol family,
// incomplete configuration, missing mandatory model, invalid endpoint, missing
// required capability, incompatible RouteSafety and required-but-unconfigured
// authentication. Errors are stable, sanitized and name the failing dimension.
func (r *Registry) Resolve(providerID string, required []Capability, safety RouteSafety) (*Resolved, error) {
	config, err := r.Config(providerID)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	profile := config.Profile
	if err := profile.Supported().SupportsRequired(required); err != nil {
		return nil, fmt.Errorf("%w: provider %q (%s): %v", ErrInvalidProviderConfig, config.ProviderID, config.ProtocolFamily, err)
	}
	// RouteSafety remains the single executable source of truth for attempt
	// safety. The resolved configuration must be able to honor exactly the
	// route safety the governor will admit; anything else refuses here.
	if err := safety.Validate(); err != nil {
		return nil, fmt.Errorf("%w: provider %q: %v", ErrInvalidProviderConfig, config.ProviderID, err)
	}
	if !routeSafetyCompatible(profile.RouteSafety, safety) {
		return nil, fmt.Errorf("%w: provider %q declares route safety %#v incompatible with the required route safety %#v",
			ErrInvalidProviderConfig, config.ProviderID, profile.RouteSafety, safety)
	}
	// Authentication: the explicit declaration decides. A required-but-missing
	// reference fails closed before dispatch; a declared-authless endpoint
	// resolves with an empty normalized reference. The reference itself stays
	// non-secret.
	auth := config.Auth.Normalize()
	if config.AuthRequirement == AuthReferenceRequired && auth == "" {
		return nil, fmt.Errorf("%w: provider %q requires an authentication reference but none is configured", ErrInvalidProviderConfig, config.ProviderID)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	model := strings.TrimSpace(config.Model)
	return &Resolved{
		ProviderID:      strings.TrimSpace(config.ProviderID),
		ProtocolFamily:  config.ProtocolFamily,
		BaseURL:         baseURL,
		Model:           model,
		Auth:            auth,
		AuthRequirement: config.AuthRequirement,
		Profile:         profile,
		ConfigIdentity:  config.Sanitized(),
	}, nil
}

// routeSafetyCompatible reports whether the endpoint's declared route safety
// satisfies the required route safety. Both sides must validate first; the
// required side is compared exactly, mirroring governor admission semantics
// where a mismatched declaration is refused rather than downgraded.
func routeSafetyCompatible(declared, required RouteSafety) bool {
	if err := declared.Validate(); err != nil {
		return false
	}
	return declared.Equal(required)
}
