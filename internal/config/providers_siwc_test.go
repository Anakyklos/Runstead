package config

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func validSIWCProviderDocument() string {
	doc := strings.Replace(validDocument(), `"version": 1`, `"version": 2`, 1)
	doc = strings.Replace(doc, "http://127.0.0.1:8080/v1", "https://api.openai.com/v1", 1)
	doc = strings.Replace(doc, `"auth_requirement": "none"`, `"auth_requirement": "reference_required", "auth_ref": "SIWC_CONNECTION"`, 1)
	doc = strings.Replace(doc, `"config_version": "v1"`, `"config_version": "v1", "wire_contract": "responses_siwc_v1", "account_binding": "hmac-sha256:v1:`+strings.Repeat("a", 64)+`", "credential_binding": "hmac-sha256:v1:`+strings.Repeat("b", 64)+`"`, 1)
	return doc
}

func TestSIWCV2DocumentParsesTypedIdentityWithoutActivation(t *testing.T) {
	registry, err := parseProviders(strings.NewReader(validSIWCProviderDocument()))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve("local-openai", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.WireContract != provider.WireResponsesSIWCV1 {
		t.Fatalf("wire = %q", resolved.WireContract)
	}
	if resolved.AccountBinding == "" || resolved.CredentialBinding == "" || resolved.BehaviorDigest == "" {
		t.Fatalf("missing v2 identities: %+v", resolved)
	}
	id := provider.IdentityFromResolved(*resolved, "responses-siwc-v1-unavailable")
	if id.WireContract != provider.WireResponsesSIWCV1 || id.ConfigIdentity == "" || id.AccountBinding == "" {
		t.Fatalf("incomplete identity: %+v", id)
	}
}

func TestSIWCV2FailsClosedOnUnprovenFields(t *testing.T) {
	base := validSIWCProviderDocument()
	cases := map[string]string{
		"unknown-wire":  strings.Replace(base, "responses_siwc_v1", "unrecognized_v1", 1),
		"missing-wire":  strings.Replace(base, `"wire_contract": "responses_siwc_v1", `, "", 1),
		"bad-binding":   strings.Replace(base, "hmac-sha256:v1:"+strings.Repeat("a", 64), "plain-account", 1),
		"extra-options": strings.Replace(base, `"config_version": "v1",`, `"config_version": "v1", "options": {"temperature":"1"},`, 1),
		"wrong-family":  strings.Replace(base, "openai_compatible", "anthropic_compatible", 1),
		"duplicate-key": strings.Replace(base, `"wire_contract": "responses_siwc_v1",`, `"wire_contract": "responses_siwc_v1", "wire_contract": "responses_siwc_v1",`, 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProviders(strings.NewReader(doc)); err == nil {
				t.Fatal("accepted invalid v2 SIWC declaration")
			}
		})
	}
}

func TestSIWCV2ChangesBindingWithoutManualConfigVersionBump(t *testing.T) {
	a := mustLoad(t, validSIWCProviderDocument())
	doc := strings.Replace(validSIWCProviderDocument(), "hmac-sha256:v1:"+strings.Repeat("b", 64), "hmac-sha256:v1:"+strings.Repeat("c", 64), 1)
	b := mustLoad(t, doc)
	ra, err := a.Resolve("local-openai", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Resolve("local-openai", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	if ra.ConfigIdentity == rb.ConfigIdentity {
		t.Fatal("credential drift did not alter config identity")
	}
	if ra.BehaviorDigest != rb.BehaviorDigest {
		t.Fatal("credential drift must not change behavior digest")
	}
}

func TestLegacyProvidersDocumentRejectsPresenceOfSIWCFieldsEvenWhenEmpty(t *testing.T) {
	doc := strings.Replace(validDocument(), `"config_version": "v1",`, `"config_version": "v1", "wire_contract": "",`, 1)
	if _, err := parseProviders(strings.NewReader(doc)); err == nil {
		t.Fatal("v1 accepted a v2-only field")
	}
}
