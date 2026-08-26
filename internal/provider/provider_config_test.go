package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func safeProfile() CapabilityProfile {
	return CapabilityProfile{
		ProfileVersion: "v1",
		Capabilities: Capabilities{
			CapabilityTextTurn:         true,
			CapabilityRunsteadProtocol: true,
		},
		RouteSafety: SafeRouteSafety(),
	}
}

func openAIConfig(providerID string) Config {
	return Config{
		ProviderID:      providerID,
		ProtocolFamily:  FamilyOpenAICompatible,
		BaseURL:         "https://" + providerID + ".example.invalid/v1",
		Model:           "model-" + providerID,
		Auth:            SecretRef("RUNSTEAD_TEST_SECRET"),
		Profile:         safeProfile(),
		AuthRequirement: AuthReferenceRequired,
		ConfigVersion:   "1",
	}
}

func mustResolve(t *testing.T, registry *Registry, providerID string) *Resolved {
	t.Helper()
	resolved, err := registry.Resolve(providerID, RequiredCapabilities(), SafeRouteSafety())
	if err != nil {
		t.Fatalf("Resolve(%q) returned unexpected error: %v", providerID, err)
	}
	return resolved
}

// 1. Two different provider IDs using openai_compatible resolve through the
// same family/contract with no vendor branching in the loop.
func TestSameProtocolFamilyDifferentProviderIDs(t *testing.T) {
	registry, err := NewRegistry(
		openAIConfig("tokenrouter"),
		openAIConfig("local-gateway"),
	)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	first := mustResolve(t, registry, "tokenrouter")
	second := mustResolve(t, registry, "local-gateway")

	if first.ProtocolFamily != FamilyOpenAICompatible || second.ProtocolFamily != FamilyOpenAICompatible {
		t.Fatalf("both providers must resolve as openai_compatible, got %q and %q", first.ProtocolFamily, second.ProtocolFamily)
	}
	if first.ProviderID == second.ProviderID {
		t.Fatalf("provider identity must stay distinct: %q == %q", first.ProviderID, second.ProviderID)
	}
	if first.BaseURL == second.BaseURL || first.Model == second.Model {
		t.Fatalf("each configured endpoint/model must be preserved: %+v vs %+v", first, second)
	}
}

// 2. All three protocol families are representable through the same
// provider-neutral boundary.
func TestThreeFamiliesRepresentableThroughSameContract(t *testing.T) {
	families := []ProtocolFamily{FamilyOpenAICompatible, FamilyAnthropicCompatible, FamilyGoogleCompatible}
	var configs []Config
	for index, family := range families {
		config := openAIConfig("provider-" + string(family))
		config.ProtocolFamily = family
		config.Model = "family-model-" + string(rune('0'+index))
		configs = append(configs, config)
	}
	registry, err := NewRegistry(configs...)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	for _, family := range families {
		resolved := mustResolve(t, registry, "provider-"+string(family))
		if resolved.ProtocolFamily != family {
			t.Fatalf("expected family %q, got %q", family, resolved.ProtocolFamily)
		}
	}
}

// 3. Unknown provider ID fails before dispatch.
func TestUnknownProviderFailsBeforeDispatch(t *testing.T) {
	registry, err := NewRegistry(openAIConfig("known"))
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry.Resolve("no-such-provider", RequiredCapabilities(), SafeRouteSafety())
	if !errors.Is(resolveErr, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", resolveErr)
	}
}

// 4. Unknown protocol family fails before dispatch.
func TestUnknownProtocolFamilyFailsBeforeDispatch(t *testing.T) {
	config := openAIConfig("odd-one")
	config.ProtocolFamily = ProtocolFamily("gptweb_official")
	// Registry construction does not validate; resolution must.
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolveErr := registry.resolveExpectInvalid(t, "odd-one")
	if !strings.Contains(resolveErr, "unknown protocol family") {
		t.Fatalf("expected fail-closed unknown protocol family error, got %q", resolveErr)
	}
}

func TestParseProtocolFamilyRejectsUnknownAndEmpty(t *testing.T) {
	if _, err := ParseProtocolFamily(""); err == nil {
		t.Fatal("empty protocol family must not parse")
	}
	if _, err := ParseProtocolFamily("chatgptweb"); err == nil {
		t.Fatal("unknown protocol family must not parse")
	}
	for _, family := range ProtocolFamilies {
		parsed, err := ParseProtocolFamily(string(family))
		if err != nil || parsed != family {
			t.Fatalf("supported family %q must parse, got %q, %v", family, parsed, err)
		}
	}
}

// 5. Missing or UNKNOWN required capability fails before dispatch with the
// capability identified.
func TestMissingRequiredCapabilityFailsBeforeDispatch(t *testing.T) {
	config := openAIConfig("no-protocol-capability")
	config.Profile.Capabilities = Capabilities{CapabilityTextTurn: true} // runstead_protocol missing

	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry.Resolve("no-protocol-capability", RequiredCapabilities(), SafeRouteSafety())
	if !errors.Is(resolveErr, ErrInvalidProviderConfig) {
		t.Fatalf("expected ErrInvalidProviderConfig, got %v", resolveErr)
	}
	if !strings.Contains(resolveErr.Error(), string(CapabilityRunsteadProtocol)) {
		t.Fatalf("error must identify the missing capability, got: %v", resolveErr)
	}
}

// Unknown capabilities fail closed on both sides: an unknown DECLARED
// capability is refused by profile validation, and an unknown REQUIRED
// capability can never be satisfied by whatever the endpoint declares.
func TestUnknownDeclaredAndRequiredCapabilitiesFailClosed(t *testing.T) {
	declaredUnknown := openAIConfig("declared-unknown-cap")
	declaredUnknown.Profile.Capabilities[Capability("future_unknown")] = true

	registry, err := NewRegistry(declaredUnknown)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolveErr := registry.resolveExpectInvalid(t, "declared-unknown-cap")
	if !strings.Contains(resolveErr, "unknown capability") {
		t.Fatalf("an unknown declared capability must be refused by validation, got %q", resolveErr)
	}

	// Even with every known capability proven, a requirement outside the closed
	// vocabulary must never resolve.
	everything := openAIConfig("unknown-requirement")
	everything.Profile.Capabilities = Capabilities{
		CapabilityTextTurn:         true,
		CapabilityRunsteadProtocol: true,
		CapabilityNativeTools:      true,
		CapabilityStreaming:        true,
		CapabilityCancellation:     true,
	}
	registry2, err := NewRegistry(everything)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, reqErr := registry2.Resolve("unknown-requirement", []Capability{Capability("future_unknown")}, SafeRouteSafety())
	if !errors.Is(reqErr, ErrInvalidProviderConfig) || !strings.Contains(reqErr.Error(), "future_unknown") {
		t.Fatalf("an unknown required capability must fail closed by name, got %v", reqErr)
	}
	if err := (Capabilities{Capability("future_unknown"): true}).Validate(); err == nil {
		t.Fatal("Capabilities.Validate must refuse keys outside the closed vocabulary")
	}
	for _, known := range []Capability{CapabilityTextTurn, CapabilityRunsteadProtocol, CapabilityNativeTools, CapabilityStreaming, CapabilityCancellation} {
		if !IsKnown(known) {
			t.Fatalf("known capability %q must be recognized", known)
		}
	}
}

func TestEmptyCapabilitySetFailsClosed(t *testing.T) {
	config := openAIConfig("no-capabilities")
	config.Profile.Capabilities = nil // nothing declared; zero value is empty

	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if _, resolveErr := registry.Resolve("no-capabilities", RequiredCapabilities(), SafeRouteSafety()); resolveErr == nil {
		t.Fatal("an empty capability set must fail closed for any required capability")
	}

	caps := Capabilities{}
	if err := caps.SupportsRequired([]Capability{CapabilityNativeTools}); err == nil {
		t.Fatal("unknown capability on a zero-value set must fail closed")
	}
}

// 6. Provider and model are selected explicitly: an empty model is refused.
func TestModelSelectionIsExplicit(t *testing.T) {
	config := openAIConfig("missing-model")
	config.Model = "   "

	registry, err := NewRegistry(config, openAIConfig("explicit"))
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry.Resolve("missing-model", RequiredCapabilities(), SafeRouteSafety())
	if !errors.Is(resolveErr, ErrInvalidProviderConfig) || !strings.Contains(resolveErr.Error(), "model") {
		t.Fatalf("a blank model must fail closed with a model-specific error, got %v", resolveErr)
	}
	resolved := mustResolve(t, registry, "explicit")
	if resolved.Model != "model-explicit" {
		t.Fatalf("resolved model must be the exact configured model, got %q", resolved.Model)
	}
}

// 7. Changing provider endpoint/config does not alter agent-loop semantics:
// resolution is pure configuration normalization; the Resolved contract shape
// (and therefore the governor/agent seam) is identical across endpoints.
func TestEndpointChangeKeepsContractSemantics(t *testing.T) {
	base := openAIConfig("endpoint-a")
	changed := base
	changed.BaseURL = "https://elsewhere.example.invalid/api/v2/"
	changed.ConfigVersion = "2"

	registryA, err := NewRegistry(base)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	registryB, err := NewRegistry(changed)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolvedA := mustResolve(t, registryA, "endpoint-a")
	resolvedB := mustResolve(t, registryB, "endpoint-a")

	if resolvedA.BaseURL != "https://endpoint-a.example.invalid/v1" {
		t.Fatalf("unexpected normalized base URL %q", resolvedA.BaseURL)
	}
	if resolvedB.BaseURL != "https://elsewhere.example.invalid/api/v2" {
		t.Fatalf("changed endpoint must normalize without trailing slash, got %q", resolvedB.BaseURL)
	}
	// The contract itself is unchanged: same family, same profile version,
	// same route-safety declaration, same model identity.
	if resolvedA.ProtocolFamily != resolvedB.ProtocolFamily ||
		resolvedA.Model != resolvedB.Model ||
		resolvedA.Profile.RouteSafety != resolvedB.Profile.RouteSafety ||
		resolvedA.Profile.ProfileVersion != resolvedB.Profile.ProfileVersion {
		t.Fatalf("endpoint change must not alter contract semantics: %+v vs %+v", resolvedA, resolvedB)
	}
}

// 8. Provider wire types do not leak into the runtime: everything crossing
// the boundary is from this package (strings, typed enums, RouteSafety). The
// compile-time assertions below pin the boundary types used by callers.
func TestResolvedBoundaryCarriesNoWireTypes(t *testing.T) {
	resolved := mustResolve(t, NewRegistryOrFatal(t), "boundary")
	var _ string = resolved.ProviderID
	var _ string = resolved.ConfigIdentity
	var _ ProtocolFamily = resolved.ProtocolFamily
	var _ RouteSafety = resolved.Profile.RouteSafety
	var _ SecretRef = resolved.Auth
}

// 9. Insecure RouteSafety keeps failing closed through the configuration
// boundary too.
func TestIncompatibleRouteSafetyFailsClosed(t *testing.T) {
	unsafeRequired := SafeRouteSafety()
	unsafeRequired.AccountPooling = AmplificationEnabled

	registry, err := NewRegistry(openAIConfig("pooling"))
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if _, resolveErr := registry.Resolve("pooling", RequiredCapabilities(), unsafeRequired); !errors.Is(resolveErr, ErrUnsafeRoute) && resolveErr == nil {
		t.Fatalf("an amplification-enabling requirement must never resolve, got %v", resolveErr)
	}

	declaredUnsafe := openAIConfig("declared-unsafe")
	declaredUnsafe.Profile.RouteSafety = RouteSafety{} // zero value: unknown accounting
	registry2, err := NewRegistry(declaredUnsafe)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolveErr := registry2.resolveExpectInvalid(t, "declared-unsafe")
	if !strings.Contains(resolveErr, "route safety") {
		t.Fatalf("undeclared route safety must be named in the error, got %q", resolveErr)
	}
}

// 10. Retry/fallback/account pooling cannot be masked as safe configuration:
// a declared profile that enables any amplification is refused even when the
// required side looks safe, because declarations must match exactly.
func TestAmplificationCannotBeMaskedAsSafeConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*RouteSafety){
		"internal retries":   func(s *RouteSafety) { s.InternalRetries = AmplificationEnabled },
		"cooldown replay":    func(s *RouteSafety) { s.CooldownReplay = AmplificationEnabled },
		"account pooling":    func(s *RouteSafety) { s.AccountPooling = AmplificationEnabled },
		"automatic fallback": func(s *RouteSafety) { s.AutomaticFallback = AmplificationEnabled },
		"combo routing":      func(s *RouteSafety) { s.ComboRouting = AmplificationEnabled },
	} {
		config := openAIConfig("masked-" + strings.ReplaceAll(name, " ", "-"))
		config.Profile.RouteSafety = SafeRouteSafety()
		mutate(&config.Profile.RouteSafety)

		registry, err := NewRegistry(config)
		if err != nil {
			t.Fatalf("NewRegistry failed for %s: %v", name, err)
		}
		if _, resolveErr := registry.Resolve(config.ProviderID, RequiredCapabilities(), SafeRouteSafety()); resolveErr == nil {
			t.Fatalf("%s enabled in the declared profile must not resolve as safe configuration", name)
		}
	}
	// A receipt-aware declaration cannot pose as single-attempt either.
	receiptPose := openAIConfig("receipt-pose")
	receiptPose.Profile.RouteSafety = ReceiptRouteSafety()
	registry, err := NewRegistry(receiptPose)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if _, resolveErr := registry.Resolve("receipt-pose", RequiredCapabilities(), SafeRouteSafety()); resolveErr == nil {
		t.Fatal("a receipt-aware declaration cannot satisfy a single-attempt route")
	}
}

// 11+12. Secrets never appear in metadata/errors/traces/contract identity;
// sanitized identity is inspectable without revealing secrets. Fixtures use
// only reference names, never real credential values.
// BaseURL is an endpoint root: credential-carrying URL components are
// refused so nothing sensitive can reach Sanitized()/ConfigIdentity.
func TestBaseURLRejectsCredentialBearingComponents(t *testing.T) {
	for name, baseURL := range map[string]string{
		"userinfo":       "https://user:secret@example.invalid/v1",
		"user only":      "https://user@example.invalid/v1",
		"query string":   "https://example.invalid/v1?api_key=abc",
		"forced query":   "https://example.invalid/v1?",
		"fragment":       "https://example.invalid/v1#cred",
		"query and frag": "https://example.invalid/v1?key=abc#frag",
	} {
		config := openAIConfig("url-" + strings.ReplaceAll(name, " ", "-"))
		config.BaseURL = baseURL
		registry, err := NewRegistry(config)
		if err != nil {
			t.Fatalf("NewRegistry failed for %s: %v", name, err)
		}
		_, resolveErr := registry.Resolve(config.ProviderID, RequiredCapabilities(), SafeRouteSafety())
		if !errors.Is(resolveErr, ErrInvalidProviderConfig) {
			t.Fatalf("credential-bearing endpoint %q (%s) must be refused before dispatch, got %v", baseURL, name, resolveErr)
		}
	}

	clean := openAIConfig("clean-url")
	clean.BaseURL = "https://example.invalid:8443/v1/"
	registry, err := NewRegistry(clean)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolved := mustResolve(t, registry, "clean-url")
	if resolved.BaseURL != "https://example.invalid:8443/v1" || strings.Contains(resolved.ConfigIdentity, ":8443") == false {
		t.Fatalf("a clean endpoint with port must resolve and render in identity: %q / %q", resolved.BaseURL, resolved.ConfigIdentity)
	}
}

// Sanitized (and therefore String/GoString) must be intrinsically safe for
// ANY Config value, including ones that never passed Validate: an endpoint
// carrying credentials in userinfo/query/fragment is canonicalized so those
// components can never appear in traces or diagnostics.
func TestSanitizedIsIntrinsicallySafeForInvalidConfigs(t *testing.T) {
	for name, baseURL := range map[string]string{
		"userinfo":    "https://user:secret@example.invalid/v1",
		"query":       "https://example.invalid/v1?api_key=supersecret",
		"fragment":    "https://example.invalid/v1#topsecret",
		"all at once": "https://bob:hunter2@example.invalid/v1?key=abc#frag",
		"unparseable": "://not a url",
		"scheme only": "https://",
	} {
		config := Config{ProviderID: "leaky-" + strings.ReplaceAll(name, " ", "-"), BaseURL: baseURL}
		for label, rendered := range map[string]string{
			"Sanitized": config.Sanitized(),
			"String":    config.String(),
			"GoString":  config.GoString(),
		} {
			for _, secret := range []string{"secret", "supersecret", "topsecret", "hunter2", "bob:", "api_key=abc"} {
				if strings.Contains(rendered, secret) {
					t.Fatalf("%s on %s config leaked %q: %s", label, name, secret, rendered)
				}
			}
		}
	}

	// A valid endpoint still renders its identifying parts.
	valid := openAIConfig("valid-render")
	rendered := valid.Sanitized()
	if !strings.Contains(rendered, "https://valid-render.example.invalid/v1") {
		t.Fatalf("a valid endpoint must still be identifiable in sanitized identity: %s", rendered)
	}

	// An unparseable endpoint reduces to the fixed placeholder instead of
	// echoing untrusted bytes.
	if !strings.Contains((Config{BaseURL: "://not a url"}).Sanitized(), "#unparseable-endpoint") {
		t.Fatal("an unparseable endpoint must render as the fixed placeholder")
	}

	// Resolution keeps proving the same guarantee end to end.
	registry, err := NewRegistry(valid)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolved := mustResolve(t, registry, "valid-render")
	if !strings.Contains(resolved.ConfigIdentity, "https://valid-render.example.invalid/v1") {
		t.Fatalf("resolved identity must carry the clean endpoint: %q", resolved.ConfigIdentity)
	}
}

func TestSanitizedIdentityNeverContainsSecretMaterial(t *testing.T) {
	config := openAIConfig("sanitized")
	config.Options = map[string]string{"api_version": "2024-01-01"}

	rendered := config.Sanitized()
	if strings.Contains(rendered, string(config.Auth)) {
		t.Fatal("sanitized identity must not contain the secret reference value")
	}
	if strings.Contains(rendered, "api_version=2024-01-01") || strings.Contains(rendered, "2024-01-01") {
		t.Fatal("option values are untrusted input and must not render into sanitized identity")
	}
	if !strings.Contains(rendered, "AuthRef:true") {
		t.Fatalf("sanitized identity should record that auth is configured, got %q", rendered)
	}

	registry, regErr := NewRegistry(config)
	if regErr != nil {
		t.Fatalf("NewRegistry failed: %v", regErr)
	}
	resolved := mustResolve(t, registry, "sanitized")
	if strings.Contains(resolved.ConfigIdentity, string(config.Auth)) {
		t.Fatal("config identity must not contain the secret reference value")
	}
	if strings.Contains(resolved.ConfigIdentity, "sk-reallysecretvalue123") {
		t.Fatal("config identity leaked fixture secret material")
	}
}

func TestErrorOutputNeverContainsSecretValues(t *testing.T) {
	config := openAIConfig("errors")
	config.Auth = SecretRef("RUNSTEAD_SECRET_ENV_NAME")
	config.Options = map[string]string{"leak": "Bearer sk-reallysecretvalue123"}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry.Resolve("errors", RequiredCapabilities(), SafeRouteSafety())
	if resolveErr == nil {
		t.Fatal("credential-shaped option value must refuse resolution")
	}
	if strings.Contains(resolveErr.Error(), "sk-reallysecretvalue123") {
		t.Fatalf("validation errors must never contain secret-shaped material: %v", resolveErr)
	}
	if strings.Contains(resolveErr.Error(), string(SecretRef("RUNSTEAD_SECRET_ENV_NAME"))) {
		t.Fatalf("even non-secret references stay out of validation errors: %v", resolveErr)
	}
}

// Registry.Config must return a DEFENSIVE COPY of Options. The review blocker
// (#96): the stored Config is returned by value but its Options map would
// share the backing map, so mutating the returned configuration changed what a
// later Resolve proved while ConfigIdentity (which never renders option
// values) stayed identical. This regression proves the registry cannot be
// silently reconfigured through the object Config() returns.
func TestRegistryConfigReturnsDefensiveOptionsCopy(t *testing.T) {
	config := openAIConfig("config-copy")
	config.Options = map[string]string{
		"max_tokens":        "2048",
		"anthropic_version": "2023-06-01",
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}

	cfg, err := registry.Config("config-copy")
	if err != nil {
		t.Fatalf("Config() failed: %v", err)
	}
	if cfg.Options["max_tokens"] != "2048" || cfg.Options["anthropic_version"] != "2023-06-01" {
		t.Fatalf("Config() options = %v, want the configured pair", cfg.Options)
	}

	// Mutate the returned configuration extensively: a later Resolve must
	// observe the ORIGINAL values, never the mutation.
	cfg.Options["max_tokens"] = "1"
	cfg.Options["anthropic_version"] = "2020-01-01"
	cfg.Options["smuggled"] = "should-not-exist"
	cfg.Options = map[string]string{"replaced": "entirely"}

	resolved := mustResolve(t, registry, "config-copy")
	if resolved.Options["max_tokens"] != "2048" || resolved.Options["anthropic_version"] != "2023-06-01" {
		t.Fatalf("Resolve after Config() mutation saw %v, want the original validated pair", resolved.Options)
	}
	if len(resolved.Options) != 2 {
		t.Fatalf("Resolve after Config() mutation saw %v, want exactly the two original options", resolved.Options)
	}

	// A second Config() must still return the original values too.
	again, err := registry.Config("config-copy")
	if err != nil {
		t.Fatalf("second Config() failed: %v", err)
	}
	if again.Options["max_tokens"] != "2048" || again.Options["anthropic_version"] != "2023-06-01" || len(again.Options) != 2 {
		t.Fatalf("second Config() options = %v, want the pristine configured pair", again.Options)
	}
}

// Resolved must propagate the validated non-secret protocol options as a
// DEFENSIVE COPY (#88). Protocol adapters consume strictly necessary options
// (for example the Messages-style max_tokens and anthropic-version semantics)
// without a parallel configuration model; mutating the operator Config after
// resolve, or the resolved copy, must never affect the other side.
func TestResolvedOptionsPropagateAsDefensiveCopy(t *testing.T) {
	config := openAIConfig("options")
	config.Options = map[string]string{
		"max_tokens":        "2048",
		"anthropic_version": "2023-06-01", // scenario value; family is openai here only for the shared contract test
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolved := mustResolve(t, registry, "options")

	if resolved.Options["max_tokens"] != "2048" || resolved.Options["anthropic_version"] != "2023-06-01" {
		t.Fatalf("resolved options = %v, want propagated values", resolved.Options)
	}
	if len(resolved.Options) != 2 {
		t.Fatalf("resolved options = %v, want exactly the configured pair", resolved.Options)
	}

	// Mutation after resolve must not leak either way.
	config.Options["max_tokens"] = "mutated-after-resolve"
	if resolved.Options["max_tokens"] != "2048" {
		t.Fatal("resolved options changed after the operator Config mutated; the copy is not defensive")
	}
	resolved.Options["max_tokens"] = "mutated-by-adapter"
	if config.Options["max_tokens"] != "mutated-after-resolve" {
		t.Fatal("operator Config options changed after the resolved copy mutated; the copy is not defensive")
	}

	// A second resolve must not observe the adapter-side mutation.
	second := mustResolve(t, registry, "options")
	if second.Options["max_tokens"] != "2048" {
		t.Fatalf("second resolve observed adapter-side mutation: %v", second.Options)
	}
}

// Resolved.Options must never carry secret material and must not be able to
// smuggle credential-shaped values past the closed Config validation: the
// defensive copy is taken only AFTER Config.Validate refused such values.
func TestResolvedOptionsStayNonSecretAndValidated(t *testing.T) {
	config := openAIConfig("options-validated")
	config.Options = map[string]string{"max_tokens": "1024"}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolved := mustResolve(t, registry, "options-validated")
	for key, value := range resolved.Options {
		if looksCredentialShaped(value) {
			t.Fatalf("resolved option %q carries credential-shaped value %q", key, value)
		}
		if strings.TrimSpace(key) == "" {
			t.Fatal("resolved options contain an empty key")
		}
	}
	if strings.Contains(resolved.ConfigIdentity, "1024") {
		t.Fatal("option values must never render into the deterministic config identity")
	}

	// No options configured stays nil, and resolution still succeeds.
	plain := openAIConfig("options-empty")
	plainRegistry, regErr := NewRegistry(plain)
	if regErr != nil {
		t.Fatalf("NewRegistry failed: %v", regErr)
	}
	plainResolved := mustResolve(t, plainRegistry, "options-empty")
	if plainResolved.Options != nil {
		t.Fatalf("resolved options = %v, want nil when the operator configured none", plainResolved.Options)
	}
}

// 13. The existing fake provider keeps working against the same contract and
// remains usable as the deterministic offline path.
func TestFakeProviderContinuesWorking(t *testing.T) {
	fake := NewFake(Response{Text: "ok", Metadata: ResponseMetadata{DeliveryState: DeliveryCompleted}})
	var client Client = fake
	safetyAware, ok := client.(SafetyAware)
	if !ok {
		t.Fatal("fake provider must remain SafetyAware")
	}
	if safetyAware.RouteSafety() != SafeRouteSafety() {
		t.Fatal("fake provider route safety must remain the safe single-attempt declaration")
	}
	response, err := fake.Complete(context.Background(), Request{Prompt: "hello", Model: "scripted"})
	if err != nil || response.Text != "ok" {
		t.Fatalf("fake provider Complete failed: %v", err)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("expected exactly one accounted attempt, got %d", fake.Attempts())
	}
}

func TestCapabilityGatingForStreamingCancellationAndNativeTools(t *testing.T) {
	profile := safeProfile()
	// Streaming/cancellation supported when explicitly proven...
	profile.Capabilities[CapabilityStreaming] = true
	profile.Capabilities[CapabilityCancellation] = true
	if err := profile.Supported().SupportsRequired([]Capability{CapabilityStreaming, CapabilityCancellation}); err != nil {
		t.Fatalf("proven capabilities must satisfy requirements: %v", err)
	}
	// ...and native tools stays opt-in evidence, never implied by family.
	config := openAIConfig("native-tools")
	config.ProtocolFamily = FamilyAnthropicCompatible
	config.Profile = profile
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	if err := registry.mustResolveSupported(t, "native-tools").Profile.Supported().Has(CapabilityNativeTools); err {
		t.Fatal("anthropic_compatible must not imply native tool support by family name")
	}
}

func TestCapabilityProfileValidation(t *testing.T) {
	profile := safeProfile()
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid profile must validate: %v", err)
	}
	noVersion := safeProfile()
	noVersion.ProfileVersion = ""
	if err := noVersion.Validate(); err == nil {
		t.Fatal("profile without version must fail closed")
	}
	badVersion := safeProfile()
	badVersion.ProfileVersion = "v999"
	if err := badVersion.Validate(); err == nil {
		t.Fatal("unsupported profile version must fail closed")
	}
	unsafe := safeProfile()
	unsafe.RouteSafety = RouteSafety{}
	if err := unsafe.Validate(); err == nil {
		t.Fatal("profile with unknown route safety must fail closed")
	}
	negativeBounds := safeProfile()
	negativeBounds.MaxRequestBytes = -1
	if err := negativeBounds.Validate(); err == nil {
		t.Fatal("negative size bound must fail closed")
	}
}

// Authentication is declared explicitly per endpoint and never inferred from
// the protocol family: an authless gateway resolves, a required-but-missing or
// blank reference fails closed before dispatch.
func TestAuthenticationReferenceRequiredBeforeDispatch(t *testing.T) {
	config := openAIConfig("authless")
	config.Auth = ""

	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry.Resolve("authless", RequiredCapabilities(), SafeRouteSafety())
	if !errors.Is(resolveErr, ErrInvalidProviderConfig) || !strings.Contains(resolveErr.Error(), "authentication reference") {
		t.Fatalf("required authentication without reference must fail before dispatch, got %v", resolveErr)
	}
}

func TestAuthlessEndpointResolvesWithoutFamilyInference(t *testing.T) {
	authless := openAIConfig("local-gateway")
	authless.AuthRequirement = AuthNone
	authless.Auth = ""

	registry, err := NewRegistry(authless)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolved := mustResolve(t, registry, "local-gateway")
	if resolved.AuthRequirement != AuthNone || resolved.Auth != "" {
		t.Fatalf("a declared-authless endpoint must resolve without any reference: %+v", resolved)
	}

	blankRef := openAIConfig("whitespace-ref")
	blankRef.Auth = SecretRef("   ")
	registry2, err := NewRegistry(blankRef)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	_, resolveErr := registry2.Resolve("whitespace-ref", RequiredCapabilities(), SafeRouteSafety())
	if !errors.Is(resolveErr, ErrInvalidProviderConfig) || !strings.Contains(resolveErr.Error(), "blank authentication reference") {
		t.Fatalf("a whitespace-only reference must never pose as configured authentication, got %v", resolveErr)
	}

	undeclared := openAIConfig("undeclared-requirement")
	undeclared.AuthRequirement = ""
	registry3, err := NewRegistry(undeclared)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolveErrText := registry3.resolveExpectInvalid(t, "undeclared-requirement")
	if !strings.Contains(resolveErrText, "auth_requirement") {
		t.Fatalf("an undeclared auth requirement must be refused explicitly, got %q", resolveErrText)
	}

	conflicted := openAIConfig("conflicted")
	conflicted.AuthRequirement = AuthNone
	conflicted.Auth = SecretRef("RUNSTEAD_TEST_SECRET")
	registry4, err := NewRegistry(conflicted)
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	resolveErrText = registry4.resolveExpectInvalid(t, "conflicted")
	if !strings.Contains(resolveErrText, "auth_requirement none") {
		t.Fatalf("auth_none with a configured reference must stay honest and refuse, got %q", resolveErrText)
	}
}

func TestDuplicateProviderIDRefused(t *testing.T) {
	if _, err := NewRegistry(openAIConfig("dup"), openAIConfig("dup")); err == nil {
		t.Fatal("duplicate provider ids are a configuration error, never a silent overwrite")
	}
}

func TestInvalidBaseURLRefused(t *testing.T) {
	for name, baseURL := range map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"scheme":     "ftp://example.invalid",
		"no host":    "https:///path",
	} {
		config := openAIConfig("bad-url")
		config.BaseURL = baseURL
		registry, err := NewRegistry(config)
		if err != nil {
			t.Fatalf("NewRegistry failed for %s: %v", name, err)
		}
		if _, resolveErr := registry.Resolve("bad-url", RequiredCapabilities(), SafeRouteSafety()); resolveErr == nil {
			t.Fatalf("invalid endpoint %q (%s) must fail before dispatch", baseURL, name)
		}
	}
}

// --- small helpers to keep failure messages readable ---

func NewRegistryOrFatal(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry(openAIConfig("explicit"), openAIConfig("boundary"))
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	return registry
}

func (r *Registry) resolveExpectInvalid(t *testing.T, providerID string) string {
	t.Helper()
	_, err := r.Resolve(providerID, RequiredCapabilities(), SafeRouteSafety())
	if err == nil {
		t.Fatal("expected resolution to fail")
	}
	return err.Error()
}

func (r *Registry) mustResolveSupported(t *testing.T, providerID string) Resolved {
	t.Helper()
	return *mustResolve(t, r, providerID)
}
