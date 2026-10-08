package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WireContract is the closed non-secret HTTP wire version chosen by provider
// declarations v2. It is not an alternative provider identity or authority.
type WireContract string

const (
	WireChatCompletionsV1 WireContract = "chat_completions_v1"
	WireResponsesSIWCV1   WireContract = "responses_siwc_v1"
)

func isOpaqueBinding(value string) bool {
	if !strings.HasPrefix(value, "hmac-sha256:v1:") {
		return false
	}
	raw := strings.TrimPrefix(value, "hmac-sha256:v1:")
	if len(raw) != 64 {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func validateWireConfig(c Config) error {
	switch c.DocumentVersion {
	case 0, 1:
		if c.WireContract != "" || c.AccountBinding != "" || c.CredentialBinding != "" {
			return fmt.Errorf("legacy provider declaration cannot contain SIWC wire or account identity")
		}
	case 2:
		if c.ProtocolFamily != FamilyOpenAICompatible {
			return fmt.Errorf("v2 wire contracts currently require openai_compatible")
		}
		switch c.WireContract {
		case WireChatCompletionsV1:
			if c.AccountBinding != "" || c.CredentialBinding != "" {
				return fmt.Errorf("Chat Completions cannot use SIWC bindings")
			}
		case WireResponsesSIWCV1:
			if !isOpaqueBinding(c.AccountBinding) || !isOpaqueBinding(c.CredentialBinding) || c.AccountBinding == c.CredentialBinding {
				return fmt.Errorf("SIWC requires distinct valid opaque account and credential bindings")
			}
			if c.AuthRequirement != AuthReferenceRequired || c.Auth.Normalize() == "" || looksCredentialShaped(string(c.Auth)) {
				return fmt.Errorf("SIWC requires an opaque credential reference")
			}
			if c.BaseURL != "https://api.openai.com/v1" {
				return fmt.Errorf("SIWC requires the documented HTTPS endpoint")
			}
		default:
			return fmt.Errorf("unknown or absent v2 wire contract")
		}
		if len(c.Options) != 0 {
			return fmt.Errorf("v2 protocol options must be typed rather than free-form")
		}
	default:
		return fmt.Errorf("unsupported provider document version")
	}
	return nil
}

// siwcBehaviorDigest hashes a CLOSED set of validated non-secret fields only.
// Opaque OAuth bindings are intentionally excluded and frozen independently.
func siwcBehaviorDigest(c Config) string {
	if c.DocumentVersion != 2 {
		return ""
	}
	keys := make([]string, 0, len(c.Profile.Capabilities))
	for k, v := range c.Profile.Capabilities {
		if v {
			keys = append(keys, string(k))
		}
	}
	sort.Strings(keys)
	material := struct {
		Version          int
		ProviderID       string
		Family           ProtocolFamily
		Endpoint         string
		Model            string
		Wire             WireContract
		AuthRequirement  AuthRequirement
		ConfigVersion    string
		Capabilities     []string
		ProfileVersion   string
		RouteSafety      RouteSafety
		MaxRequestBytes  int
		MaxResponseBytes int
	}{c.DocumentVersion, c.ProviderID, c.ProtocolFamily, sanitizedEndpoint(c.BaseURL), c.Model, c.WireContract, c.AuthRequirement, c.ConfigVersion, keys, c.Profile.ProfileVersion, c.Profile.RouteSafety, c.Profile.MaxRequestBytes, c.Profile.MaxResponseBytes}
	encoded, _ := json.Marshal(material)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func siwcConfigIdentity(c Config) string {
	// Unlike legacy Sanitized(), this identity binds v2's behavior and opaque
	// credential handles. It never renders raw OAuth identity or secret values.
	sum := sha256.Sum256([]byte(siwcBehaviorDigest(c) + "\x00" + c.AccountBinding + "\x00" + c.CredentialBinding))
	return "provider.v2:sha256:" + hex.EncodeToString(sum[:])
}
