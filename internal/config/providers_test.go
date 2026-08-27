package config

import (
	"regexp"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// stripRouteSafety removes the route_safety block so tests can exercise
// omission/injection against the real document shape.
func stripRouteSafety(document string) string {
	return regexp.MustCompile(`(?s)"route_safety":\s*\{[^}]*\}`).ReplaceAllString(document, "")
}

func validDocument() string {
	return `{
		"version": 1,
		"providers": [
			{
				"provider_id": "local-openai",
				"protocol_family": "openai_compatible",
				"base_url": "http://127.0.0.1:8080/v1",
				"model": "demo-model",
				"auth_requirement": "none",
				"config_version": "v1",
				"profile": {
					"profile_version": "v1",
					"capabilities": ["text_turn", "runstead_protocol"],
					"route_safety": {
						"attempt_accounting": "single",
						"single_attempt": "guaranteed",
						"internal_retries": "disabled",
						"cooldown_replay": "disabled",
						"account_pooling": "disabled",
						"automatic_fallback": "disabled",
						"combo_routing": "disabled"
					}
				}
			}
		]
	}`
}

func mustLoad(t *testing.T, document string) *provider.Registry {
	t.Helper()
	registry, err := parseProviders(strings.NewReader(document))
	if err != nil {
		t.Fatalf("parseProviders() error = %v", err)
	}
	return registry
}

func TestProvidersFileLoadsAllFamiliesWithDefaults(t *testing.T) {
	registry := mustLoad(t, validDocument())
	resolved, err := registry.Resolve("local-openai", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ProviderID != "local-openai" {
		t.Fatalf("provider id = %q", resolved.ProviderID)
	}
	if resolved.ProtocolFamily != provider.FamilyOpenAICompatible {
		t.Fatalf("family = %q", resolved.ProtocolFamily)
	}
	if resolved.Model != "demo-model" {
		t.Fatalf("model = %q", resolved.Model)
	}
	// The declared safe route is honored exactly as declared.
	if !resolved.Profile.RouteSafety.Equal(provider.SafeRouteSafety()) {
		t.Fatalf("declared route safety must resolve to the safe single-attempt declaration")
	}
	// Omission is fail-closed, never promoted to a safe-route guarantee.
	withoutSafety := stripRouteSafety(validDocument())
	if _, err := parseProviders(strings.NewReader(withoutSafety)); err == nil {
		t.Fatalf("route_safety omission must fail closed, got success")
	}
	for _, family := range []provider.ProtocolFamily{
		provider.FamilyAnthropicCompatible,
		provider.FamilyGoogleCompatible,
	} {
		document := strings.ReplaceAll(validDocument(), "openai_compatible", string(family))
		document = strings.ReplaceAll(document, "local-openai", strings.TrimPrefix(string(family), "m-"))
		registry := mustLoad(t, document)
		providerID := strings.TrimPrefix(string(family), "m-")
		resolved, err := registry.Resolve(providerID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
		if err != nil {
			t.Fatalf("Resolve(%s): %v", family, err)
		}
		if resolved.ProtocolFamily != family {
			t.Fatalf("family = %q, want %q", resolved.ProtocolFamily, family)
		}
	}
}

func TestProvidersFileParsesAuthReferenceAndOptions(t *testing.T) {
	document := `{
		"version": 1,
		"providers": [
			{
				"provider_id": "auth-openai",
				"protocol_family": "openai_compatible",
				"base_url": "https://endpoint.example/v1",
				"model": "model-x",
				"auth_requirement": "reference_required",
				"auth_ref": "MY_API_TOKEN_ENV",
				"options": {"temperature": "0"},
				"config_version": "v2",
				"profile": {
					"profile_version": "v1",
					"capabilities": ["text_turn", "runstead_protocol"],
					"route_safety": {
						"attempt_accounting": "single",
						"single_attempt": "guaranteed",
						"internal_retries": "disabled",
						"cooldown_replay": "disabled",
						"account_pooling": "disabled",
						"automatic_fallback": "disabled",
						"combo_routing": "disabled"
					}
				}
			}
		]
	}`
	registry := mustLoad(t, document)
	resolved, err := registry.Resolve("auth-openai", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Auth != "MY_API_TOKEN_ENV" {
		t.Fatalf("auth ref = %q", resolved.Auth)
	}
	if resolved.AuthRequirement != provider.AuthReferenceRequired {
		t.Fatalf("auth requirement = %q", resolved.AuthRequirement)
	}
	// The sanitized identity lists option keys but NEVER option values.
	if !strings.Contains(resolved.ConfigIdentity, "temperature") {
		t.Fatalf("config identity must name the option key: %s", resolved.ConfigIdentity)
	}
	if strings.Contains(resolved.ConfigIdentity, `"0"`) && strings.Contains(resolved.ConfigIdentity, "temperature") {
		// Values are never rendered; the key alone appears.
		if strings.Contains(resolved.ConfigIdentity, "temperature:0") || strings.Contains(resolved.ConfigIdentity, `temperature":"0`) {
			t.Fatalf("config identity must never render option values: %s", resolved.ConfigIdentity)
		}
	}
}

func TestProvidersFileFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"missing version", func(string) string {
			return strings.Replace(validDocument(), `"version": 1,`, "", 1)
		}},
		{"version zero", func(document string) string {
			return strings.Replace(document, `"version": 1`, `"version": 0`, 1)
		}},
		{"unknown future version", func(document string) string {
			return strings.Replace(document, `"version": 1`, `"version": 2`, 1)
		}},
		{"trailing json", func(document string) string {
			return document + `{"extra":true}`
		}},
		{"missing route safety", func(document string) string {
			return stripRouteSafety(document)
		}},
		{"empty document", func(string) string { return `{}` }},
		{"no providers", func(string) string { return `{"version":1,"providers":[]}` }},
		{"duplicate provider id", func(document string) string {
			return strings.Replace(document, `"providers": [`, `"providers": [`+`{"provider_id":"local-openai","protocol_family":"openai_compatible","base_url":"http://x/v1","model":"m","auth_requirement":"none","profile":{"profile_version":"v1","capabilities":["text_turn"]}},`, 1)
		}},
		{"unknown family", func(document string) string {
			return strings.ReplaceAll(document, "openai_compatible", "bogus_family")
		}},
		{"unknown capability", func(document string) string {
			return strings.ReplaceAll(document, "runstead_protocol", "space_travel")
		}},
		{"missing capabilities", func(document string) string {
			return strings.Replace(document, `"capabilities": ["text_turn", "runstead_protocol"]`, `"capabilities": []`, 1)
		}},
		{"missing profile version", func(document string) string {
			return strings.Replace(document, `"profile_version": "v1"`, `"profile_version": ""`, 1)
		}},
		{"invalid route safety", func(document string) string {
			document = stripRouteSafety(document)
			return strings.Replace(document, `"capabilities": ["text_turn", "runstead_protocol"]`, `"capabilities": ["text_turn", "runstead_protocol"], "route_safety": {"attempt_accounting":"single","single_attempt":"guaranteed","internal_retries":"enabled","cooldown_replay":"disabled","account_pooling":"disabled","automatic_fallback":"disabled","combo_routing":"disabled"}`, 1)
		}},
		{"unknown json field", func(document string) string {
			return strings.Replace(document, `"config_version": "v1"`, `"config_version": "v1", "nonsense": true`, 1)
		}},
		{"invalid auth requirement", func(document string) string {
			return strings.ReplaceAll(document, `"auth_requirement": "none"`, `"auth_requirement": "maybe"`)
		}},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseProviders(strings.NewReader(testCase.mutate(validDocument())))
			if err == nil {
				t.Fatalf("parseProviders must fail closed, got success")
			}
		})
	}
}

func TestProvidersFileRegistryRejectsUnknownSelection(t *testing.T) {
	registry := mustLoad(t, validDocument())
	if _, err := registry.Resolve("missing-id", provider.RequiredCapabilities(), provider.SafeRouteSafety()); err == nil {
		t.Fatalf("unknown provider selection must fail closed")
	}
}
