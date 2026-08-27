package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// EnvProviders names the provider-declaration file for the provider-neutral
// live surface (#14): a JSON document listing operator-declared endpoints
// following the provider.Config contract of #79.
const EnvProviders = "RUNSTEAD_PROVIDERS"

// EnvProviderID selects exactly one configured provider for the current
// execution (run or resume).
const EnvProviderID = "RUNSTEAD_PROVIDER_ID"

// providersDocument is the operator-facing JSON representation of the #79
// provider contract. It is NOT a second configuration model: every field maps
// one-to-one onto provider.Config/CapabilityProfile/RouteSafety, which remain
// the single validated model. String enums are used on the wire so
// misconfiguration is readable and fails closed instead of silently
// selecting fabricated values.
type providersDocument struct {
	Version   int                  `json:"version"`
	Providers []providersFileEntry `json:"providers"`
}

type providersFileEntry struct {
	ProviderID      string            `json:"provider_id"`
	ProtocolFamily  string            `json:"protocol_family"`
	BaseURL         string            `json:"base_url"`
	Model           string            `json:"model"`
	AuthRef         string            `json:"auth_ref"`
	AuthRequirement string            `json:"auth_requirement"`
	Options         map[string]string `json:"options"`
	ConfigVersion   string            `json:"config_version"`
	Profile         providersProfile  `json:"profile"`
}

type providersProfile struct {
	ProfileVersion   string           `json:"profile_version"`
	Capabilities     []string         `json:"capabilities"`
	RouteSafety      *providersSafety `json:"route_safety"`
	MaxRequestBytes  int              `json:"max_request_bytes"`
	MaxResponseBytes int              `json:"max_response_bytes"`
}

type providersSafety struct {
	AttemptAccounting string `json:"attempt_accounting"`
	SingleAttempt     string `json:"single_attempt"`
	InternalRetries   string `json:"internal_retries"`
	CooldownReplay    string `json:"cooldown_replay"`
	AccountPooling    string `json:"account_pooling"`
	AutomaticFallback string `json:"automatic_fallback"`
	ComboRouting      string `json:"combo_routing"`
}

// LoadProvidersFile reads and strictly parses one provider declaration
// document and builds the #79 provider.Registry. Unknown JSON fields, unknown
// enum values, empty provider sets, duplicate provider IDs and invalid
// shapes fail closed before any dispatch. The document carries only
// non-secret material (auth_ref is a reference names, never a value).
func LoadProvidersFile(path string) (*provider.Registry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("provider declarations unavailable: %w", err)
	}
	defer file.Close()
	return parseProviders(file)
}

func parseProviders(reader io.Reader) (*provider.Registry, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var document providersDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("provider declarations: %v", err)
	}
	if len(document.Providers) == 0 {
		return nil, fmt.Errorf("provider declarations: at least one provider must be configured")
	}
	configs := make([]provider.Config, 0, len(document.Providers))
	for index := range document.Providers {
		entry := &document.Providers[index]
		family, err := provider.ParseProtocolFamily(entry.ProtocolFamily)
		if err != nil {
			return nil, fmt.Errorf("provider declarations #%d (%s): %v", index+1, displayProviderName(entry.ProviderID), err)
		}
		authRequirement := provider.AuthRequirement(strings.TrimSpace(entry.AuthRequirement))
		profile, err := buildProfile(entry.Profile)
		if err != nil {
			return nil, fmt.Errorf("provider declarations #%d (%s): %v", index+1, displayProviderName(entry.ProviderID), err)
		}
		configs = append(configs, provider.Config{
			ProviderID:      entry.ProviderID,
			ProtocolFamily:  family,
			BaseURL:         entry.BaseURL,
			Model:           entry.Model,
			Auth:            provider.SecretRef(strings.TrimSpace(entry.AuthRef)),
			AuthRequirement: authRequirement,
			Options:         entry.Options,
			Profile:         profile,
			ConfigVersion:   entry.ConfigVersion,
		})
	}
	registry, err := provider.NewRegistry(configs...)
	if err != nil {
		return nil, fmt.Errorf("provider declarations: %v", err)
	}
	// Every declared provider must be self-consistent before any selection:
	// invalid configurations fail here, not at dispatch.
	for _, config := range configs {
		if err := config.Validate(); err != nil {
			return nil, fmt.Errorf("provider declarations: %v", err)
		}
	}
	return registry, nil
}

func buildProfile(entry providersProfile) (provider.CapabilityProfile, error) {
	if strings.TrimSpace(entry.ProfileVersion) == "" {
		return provider.CapabilityProfile{}, fmt.Errorf("profile.profile_version must not be empty")
	}
	if len(entry.Capabilities) == 0 {
		return provider.CapabilityProfile{}, fmt.Errorf("profile.capabilities must declare at least one capability")
	}
	capabilities := make(provider.Capabilities, len(entry.Capabilities))
	for _, name := range entry.Capabilities {
		capability := provider.Capability(strings.TrimSpace(name))
		if !provider.IsKnown(capability) {
			return provider.CapabilityProfile{}, fmt.Errorf("profile declares unknown capability %q", name)
		}
		capabilities[capability] = true
	}
	safety := provider.SafeRouteSafety()
	if entry.RouteSafety != nil {
		parsed, err := buildSafety(*entry.RouteSafety)
		if err != nil {
			return provider.CapabilityProfile{}, err
		}
		safety = parsed
	}
	return provider.CapabilityProfile{
		ProfileVersion:   strings.TrimSpace(entry.ProfileVersion),
		Capabilities:     capabilities,
		RouteSafety:      safety,
		MaxRequestBytes:  entry.MaxRequestBytes,
		MaxResponseBytes: entry.MaxResponseBytes,
	}, nil
}

func buildSafety(entry providersSafety) (provider.RouteSafety, error) {
	parseAmplification := func(name, value string) (provider.AmplificationStatus, error) {
		switch strings.TrimSpace(value) {
		case "disabled":
			return provider.AmplificationDisabled, nil
		case "enabled":
			return provider.AmplificationEnabled, nil
		default:
			return provider.AmplificationUnknown, fmt.Errorf("route_safety.%s must be %q or %q", name, "disabled", "enabled")
		}
	}
	internalRetries, err := parseAmplification("internal_retries", entry.InternalRetries)
	if err != nil {
		return provider.RouteSafety{}, err
	}
	cooldownReplay, err := parseAmplification("cooldown_replay", entry.CooldownReplay)
	if err != nil {
		return provider.RouteSafety{}, err
	}
	accountPooling, err := parseAmplification("account_pooling", entry.AccountPooling)
	if err != nil {
		return provider.RouteSafety{}, err
	}
	automaticFallback, err := parseAmplification("automatic_fallback", entry.AutomaticFallback)
	if err != nil {
		return provider.RouteSafety{}, err
	}
	comboRouting, err := parseAmplification("combo_routing", entry.ComboRouting)
	if err != nil {
		return provider.RouteSafety{}, err
	}
	var accounting provider.AttemptAccounting
	switch strings.TrimSpace(entry.AttemptAccounting) {
	case "single":
		accounting = provider.AttemptAccountingSingle
	case "receipts":
		accounting = provider.AttemptAccountingReceipts
	default:
		return provider.RouteSafety{}, fmt.Errorf("route_safety.attempt_accounting must be %q or %q", "single", "receipts")
	}
	var singleAttempt provider.SingleAttemptGuarantee
	switch strings.TrimSpace(entry.SingleAttempt) {
	case "guaranteed":
		singleAttempt = provider.SingleAttemptGuaranteed
	case "unknown":
		singleAttempt = provider.SingleAttemptUnknown
	default:
		return provider.RouteSafety{}, fmt.Errorf("route_safety.single_attempt must be %q or %q", "guaranteed", "unknown")
	}
	safety := provider.RouteSafety{
		AttemptAccounting: accounting,
		SingleAttempt:     singleAttempt,
		InternalRetries:   internalRetries,
		CooldownReplay:    cooldownReplay,
		AccountPooling:    accountPooling,
		AutomaticFallback: automaticFallback,
		ComboRouting:      comboRouting,
	}
	if err := safety.Validate(); err != nil {
		return provider.RouteSafety{}, fmt.Errorf("route_safety: %v", err)
	}
	return safety, nil
}

func displayProviderName(providerID string) string {
	if strings.TrimSpace(providerID) == "" {
		return "<missing provider_id>"
	}
	return providerID
}
