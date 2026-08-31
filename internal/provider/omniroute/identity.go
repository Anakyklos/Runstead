package omniroute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const (
	// IdentityProviderID is the stable provider identity of the OmniRoute
	// adapter. The downstream provider selected by the protected lane remains
	// part of the sanitized configuration identity below.
	IdentityProviderID = "omniroute"
	// IdentityProfileVersion identifies the non-secret OmniRoute configuration
	// identity format. It is separate from provider.CapabilityProfile, which is
	// the compatibility contract of configured provider endpoints.
	IdentityProfileVersion = "omniroute-config.v1"
)

// IdentityFromConfig derives the provider-neutral identity of a validated
// OmniRoute configuration. It is safe to persist: APIKey and ConnectionID are
// deliberately excluded, while the derived account-lane hash remains so a
// resume cannot silently select a different pinned connection.
//
// The returned ConfigIdentity uses provider.Config.Sanitized so it remains
// compatible with the existing provider identity contract. The OmniRoute-only
// fields are bound into ConfigVersion by a digest of non-secret material.
func IdentityFromConfig(config Config) provider.Identity {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if normalized, err := normalizeBaseURL(baseURL); err == nil {
		baseURL = normalized
	} else {
		baseURL = "#unparseable-endpoint"
	}
	managementURL := strings.TrimSpace(config.ManagementBaseURL)
	if managementURL == "" {
		managementURL = managementURLFromBase(baseURL)
	} else if normalized, err := normalizeBaseURL(managementURL); err == nil {
		managementURL = normalized
	} else {
		managementURL = "#unparseable-endpoint"
	}
	model := strings.TrimSpace(config.Model)
	chatEndpoint := strings.TrimSpace(config.ChatEndpoint)
	if chatEndpoint == "" {
		chatEndpoint = defaultChatEndpoint
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxRequestBytes := config.MaxRequestBytes
	if maxRequestBytes == 0 {
		maxRequestBytes = defaultBodyLimit
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultBodyLimit
	}

	// This is a closed, ordered struct rather than a map so the identity digest
	// cannot depend on map iteration order. It contains no API key or raw
	// connection id.
	nonSecret := struct {
		BaseURL               string
		ManagementBaseURL     string
		Provider              string
		AccountLaneHash       string
		EnableAttemptReceipts bool
		ChatEndpoint          string
		Timeout               string
		MaxRequestBytes       int
		MaxResponseBytes      int
		RouteSafety           provider.RouteSafety
	}{
		BaseURL:               baseURL,
		ManagementBaseURL:     managementURL,
		Provider:              strings.TrimSpace(config.Provider),
		AccountLaneHash:       strings.TrimSpace(config.AccountLaneHash),
		EnableAttemptReceipts: config.EnableAttemptReceipts,
		ChatEndpoint:          chatEndpoint,
		Timeout:               timeout.String(),
		MaxRequestBytes:       maxRequestBytes,
		MaxResponseBytes:      maxResponseBytes,
		RouteSafety:           config.RouteSafety,
	}
	encoded, _ := json.Marshal(nonSecret)
	digest := sha256.Sum256(encoded)
	configVersion := IdentityProfileVersion + ":" + hex.EncodeToString(digest[:])

	sanitized := provider.Config{
		ProviderID:      IdentityProviderID,
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		BaseURL:         baseURL,
		Model:           model,
		Auth:            provider.SecretRef("configured"),
		AuthRequirement: provider.AuthReferenceRequired,
		Options: map[string]string{
			"account_lane_hash":        nonSecret.AccountLaneHash,
			"chat_endpoint":            nonSecret.ChatEndpoint,
			"management_base_url":      nonSecret.ManagementBaseURL,
			"max_request_bytes":        strconv.Itoa(nonSecret.MaxRequestBytes),
			"max_response_bytes":       strconv.Itoa(nonSecret.MaxResponseBytes),
			"omniroute_provider":       nonSecret.Provider,
			"attempt_receipts_enabled": strconv.FormatBool(nonSecret.EnableAttemptReceipts),
			"timeout":                  nonSecret.Timeout,
		},
		Profile: provider.CapabilityProfile{
			ProfileVersion: IdentityProfileVersion,
			RouteSafety:    config.RouteSafety,
		},
		ConfigVersion: configVersion,
	}.Sanitized()

	return provider.Identity{
		ProviderID:     IdentityProviderID,
		ProtocolFamily: provider.FamilyOpenAICompatible,
		Model:          model,
		ConfigIdentity: sanitized,
		ProfileVersion: IdentityProfileVersion,
		AdapterVersion: AdapterVersion,
	}
}

// IsIdentity reports whether an identity was produced by IdentityFromConfig.
// The marker is intentionally narrow so an operator-chosen compatible provider
// whose ID happens to be "omniroute" is not mistaken for this adapter lane.
func IsIdentity(identity provider.Identity) bool {
	return identity.ProviderID == IdentityProviderID && identity.ProfileVersion == IdentityProfileVersion && strings.Contains(identity.ConfigIdentity, `ConfigVersion:"`+IdentityProfileVersion+`:`)
}
