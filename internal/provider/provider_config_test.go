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
		ProviderID:     providerID,
		ProtocolFamily: FamilyOpenAICompatible,
		BaseURL:        "https://" + providerID + ".example.invalid/v1",
		Model:          "model-" + providerID,
		Auth:           SecretRef("RUNSTEAD_TEST_SECRET"),
		Profile:        safeProfile(),
		ConfigVersion:  "1",
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
	if _, err := NewRegistry(config); err == nil {
		// Registry construction does not validate; resolution must.
		registry, regErr := NewRegistry(config)
		if regErr != nil {
			t.Fatalf("NewRegistry failed: %v", regErr)
		}
		_, resolveErr := registry.Resolve("odd-one", RequiredCapabilities(), SafeRouteSafety())
		if !errors.Is(resolveErr, ErrInvalidProviderConfig) || !strings.Contains(resolveErr.Error(), "unknown protocol family") {
			t.Fatalf("expected fail-closed unknown protocol family error, got %v", resolveErr)
		}
		return
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
