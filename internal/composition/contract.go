package composition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

const ContractSchemaVersion = 1

const (
	DefaultRuntimeIdentity  = "runstead-runtime.v1"
	DefaultProtocolIdentity = "runstead.protocol.v1"
	GovernorIdentity        = "runstead.governor.v1"
	PolicyIdentity          = "runstead.policy.v1"
	EvidenceIdentity        = "runstead.evidence.v1"
	RecoveryIdentity        = "runstead.recovery.v1"
	VerifierIdentity        = "runstead.verifier.v1"
)

// ProfileIdentity is the exact operator profile identity frozen in a task.
type ProfileIdentity struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// PackageIdentity is the non-secret package material frozen in a task.
type PackageIdentity struct {
	ID                       string       `json:"id"`
	Version                  string       `json:"version"`
	Provenance               string       `json:"provenance"`
	Kind                     PackageKind  `json:"kind"`
	RuntimeCompatibility     string       `json:"runtime_compatibility"`
	Actions                  []string     `json:"actions"`
	Capabilities             []string     `json:"capabilities"`
	WorkspaceRequirements    []string     `json:"workspace_requirements"`
	NetworkRequirements      []string     `json:"network_requirements"`
	EffectClass              string       `json:"effect_class"`
	RecoveryClass            string       `json:"recovery_class"`
	EvidenceRequirements     []string     `json:"evidence_requirements"`
	VerificationRequirements []string     `json:"verification_requirements"`
	ApprovalBoundary         string       `json:"approval_boundary"`
	MaxOutputBytes           int          `json:"max_output_bytes"`
	Dependencies             []PackageRef `json:"dependencies,omitempty"`
	Conflicts                []PackageRef `json:"conflicts,omitempty"`
}

// ProviderIdentity is the sanitized subset of provider.Identity. It contains
// no authentication reference or wire configuration.
type ProviderIdentity struct {
	ProviderID     string `json:"provider_id,omitempty"`
	ProtocolFamily string `json:"protocol_family,omitempty"`
	Model          string `json:"model,omitempty"`
	ConfigIdentity string `json:"config_identity,omitempty"`
	ProfileVersion string `json:"provider_profile_version,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
}

// ArgumentIdentity and ToolIdentity are the static schema material supplied by
// the existing tools registry.
type ArgumentIdentity struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Note     string `json:"note"`
}

type ToolIdentity struct {
	Name      string             `json:"name"`
	Summary   string             `json:"summary"`
	ReadOnly  bool               `json:"read_only"`
	Arguments []ArgumentIdentity `json:"arguments,omitempty"`
}

type RecipeCatalogIdentity struct {
	Digest    string   `json:"digest,omitempty"`
	RecipeIDs []string `json:"recipe_ids,omitempty"`
}

// FrozenExecutionContract is the non-secret material that governs the
// composition of one task. ExecutionContractHash is held outside canonical
// JSON and is persisted alongside the exact ContractJSON bytes.
type FrozenExecutionContract struct {
	ContractVersion       int                   `json:"contract_version"`
	RuntimeIdentity       string                `json:"runtime_identity"`
	ProtocolIdentity      string                `json:"protocol_identity"`
	Profile               ProfileIdentity       `json:"profile"`
	Packages              []PackageIdentity     `json:"packages"`
	Provider              ProviderIdentity      `json:"provider"`
	Tools                 []ToolIdentity        `json:"tools"`
	ToolSchemaDigest      string                `json:"tool_schema_digest"`
	RecipeCatalog         RecipeCatalogIdentity `json:"recipe_catalog"`
	WritePolicyIdentity   string                `json:"write_policy_identity,omitempty"`
	RecipePolicyIdentity  string                `json:"recipe_policy_identity,omitempty"`
	AcceptancePlanDigest  string                `json:"acceptance_plan_digest,omitempty"`
	GovernorIdentity      string                `json:"governor_identity"`
	PolicyIdentity        string                `json:"policy_identity"`
	EvidenceIdentity      string                `json:"evidence_identity"`
	RecoveryIdentity      string                `json:"recovery_identity"`
	VerifierIdentity      string                `json:"verifier_identity"`
	ExecutionContractHash string                `json:"-"`
}

// ToolNames returns the sorted effective tool names.
func (c FrozenExecutionContract) ToolNames() []string {
	names := make([]string, 0, len(c.Tools))
	for _, tool := range c.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// ContractBytes canonicalizes the contract material and computes its SHA-256
// without incorporating the hash into the material itself.
func (c FrozenExecutionContract) ContractBytes() ([]byte, string, error) {
	c.ExecutionContractHash = ""
	if err := validateContractMaterial(c); err != nil {
		return nil, "", err
	}
	canonical := canonicalContract(c)
	data, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "document", "cannot encode contract")
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateContract checks exact canonical bytes and the separately persisted
// hash. A reordered or edited contract is corruption, not a new composition.
func ValidateContract(data []byte, hash string) (FrozenExecutionContract, string, error) {
	if !validHash(hash) {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "hash", "contract hash has invalid format")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "document", "%v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var contract FrozenExecutionContract
	if err := decoder.Decode(&contract); err != nil {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "document", "decode failed")
	}
	if err := ensureEOF(decoder); err != nil {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "document", "%v", err)
	}
	if err := validateContractMaterial(contract); err != nil {
		return FrozenExecutionContract{}, "", err
	}
	canonical, computed, err := contract.ContractBytes()
	if err != nil {
		return FrozenExecutionContract{}, "", err
	}
	if computed != hash {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "hash", "contract hash does not match persisted bytes")
	}
	if !bytes.Equal(canonical, data) {
		return FrozenExecutionContract{}, "", compositionError(ErrorInvalidContract, ErrInvalidContract, "document", "contract bytes are not canonical")
	}
	contract.ExecutionContractHash = computed
	return contract, computed, nil
}

func validHash(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func providerIdentityMaterial(identity provider.Identity) (ProviderIdentity, error) {
	material := ProviderIdentity{
		ProviderID: identity.ProviderID, ProtocolFamily: string(identity.ProtocolFamily), Model: identity.Model,
		ConfigIdentity: identity.ConfigIdentity, ProfileVersion: identity.ProfileVersion, AdapterVersion: identity.AdapterVersion,
	}
	if err := validateProviderIdentity(material); err != nil {
		return ProviderIdentity{}, compositionError(ErrorInvalidContract, ErrInvalidContract, "provider", "%v", err)
	}
	return material, nil
}

func validateProviderIdentity(identity ProviderIdentity) error {
	values := []struct {
		name  string
		value string
	}{
		{name: "provider_id", value: identity.ProviderID},
		{name: "protocol_family", value: identity.ProtocolFamily},
		{name: "model", value: identity.Model},
		{name: "config_identity", value: identity.ConfigIdentity},
		{name: "provider_profile_version", value: identity.ProfileVersion},
		{name: "adapter_version", value: identity.AdapterVersion},
	}
	anyValue := false
	for _, field := range values {
		if field.value == "" {
			continue
		}
		anyValue = true
		if strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("%s must not have surrounding whitespace", field.name)
		}
		if strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s contains a control character", field.name)
		}
		if looksCredentialShaped(field.value) {
			return fmt.Errorf("%s contains credential-shaped content", field.name)
		}
	}
	if !anyValue {
		return nil
	}
	if identity.ProviderID == "" || identity.Model == "" || identity.ConfigIdentity == "" || identity.ProfileVersion == "" || identity.AdapterVersion == "" {
		return fmt.Errorf("configured provider identity requires id, model, config identity, profile version and adapter version")
	}
	if !provider.ProtocolFamily(identity.ProtocolFamily).Valid() {
		return fmt.Errorf("unknown protocol family %q", identity.ProtocolFamily)
	}
	if !strings.HasPrefix(identity.ConfigIdentity, "provider.Config{") {
		return fmt.Errorf("config identity is not a provider.Config sanitized identity")
	}
	return nil
}

func looksCredentialShaped(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"authorization:", "bearer ", "set-cookie:", "api_key", "apikey", "password", "secret", "token", "cookie", "sk-", "-----begin",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validateContractMaterial(c FrozenExecutionContract) error {
	if c.ContractVersion != ContractSchemaVersion {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "contract_version", "unsupported contract version %d", c.ContractVersion)
	}
	if strings.TrimSpace(c.RuntimeIdentity) == "" || strings.TrimSpace(c.ProtocolIdentity) == "" {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "identity", "runtime and protocol identities are required")
	}
	if strings.TrimSpace(c.Profile.ID) == "" || strings.TrimSpace(c.Profile.Version) == "" {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "profile", "profile id and version are required")
	}
	if err := validateProviderIdentity(c.Provider); err != nil {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "provider", "%v", err)
	}
	if len(c.Packages) == 0 {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "packages", "at least one package is required")
	}
	seenPackages := make(map[string]struct{}, len(c.Packages))
	for index, pkg := range c.Packages {
		if strings.TrimSpace(pkg.ID) == "" || strings.TrimSpace(pkg.Version) == "" || strings.TrimSpace(pkg.Provenance) == "" {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("packages[%d]", index), "package id, version and provenance are required")
		}
		if pkg.Kind != PackageKindBuiltin {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("packages[%d].kind", index), "unsupported package kind %q", pkg.Kind)
		}
		if strings.TrimSpace(pkg.RuntimeCompatibility) == "" || len(pkg.Actions) == 0 || pkg.MaxOutputBytes <= 0 {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("packages[%d]", index), "package runtime compatibility, actions and positive output bound are required")
		}
		if pkg.RuntimeCompatibility != c.RuntimeIdentity {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("packages[%d].runtime_compatibility", index), "package %q@%q is incompatible with runtime %q", pkg.ID, pkg.Version, c.RuntimeIdentity)
		}
		key := packageKey(pkg.ID, pkg.Version)
		if _, exists := seenPackages[key]; exists {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, "packages", "duplicate package %q@%q", pkg.ID, pkg.Version)
		}
		seenPackages[key] = struct{}{}
		for _, field := range []struct {
			name   string
			values []string
		}{
			{name: "actions", values: pkg.Actions},
			{name: "capabilities", values: pkg.Capabilities},
			{name: "workspace_requirements", values: pkg.WorkspaceRequirements},
			{name: "network_requirements", values: pkg.NetworkRequirements},
			{name: "evidence_requirements", values: pkg.EvidenceRequirements},
			{name: "verification_requirements", values: pkg.VerificationRequirements},
		} {
			if err := validateStringSet(field.values); err != nil {
				return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("packages[%d].%s", index, field.name), "%v", err)
			}
		}
	}
	if len(c.Tools) == 0 || strings.TrimSpace(c.ToolSchemaDigest) == "" {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "tools", "effective tools and tool schema digest are required")
	}
	seenTools := make(map[string]struct{}, len(c.Tools))
	for index, tool := range c.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, fmt.Sprintf("tools[%d].name", index), "tool name is required")
		}
		if _, exists := seenTools[tool.Name]; exists {
			return compositionError(ErrorInvalidContract, ErrInvalidContract, "tools", "duplicate tool %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
		seenArguments := make(map[string]struct{}, len(tool.Arguments))
		for _, argument := range tool.Arguments {
			if strings.TrimSpace(argument.Name) == "" || strings.TrimSpace(argument.Type) == "" {
				return compositionError(ErrorInvalidContract, ErrInvalidContract, "tools."+tool.Name+".arguments", "argument name and type are required")
			}
			if _, exists := seenArguments[argument.Name]; exists {
				return compositionError(ErrorInvalidContract, ErrInvalidContract, "tools."+tool.Name+".arguments", "duplicate argument %q", argument.Name)
			}
			seenArguments[argument.Name] = struct{}{}
		}
	}
	digest, err := digestJSON(canonicalTools(c.Tools))
	if err != nil || digest != c.ToolSchemaDigest {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "tool_schema_digest", "tool schema digest does not match tools")
	}
	if c.GovernorIdentity != GovernorIdentity || c.PolicyIdentity != PolicyIdentity || c.EvidenceIdentity != EvidenceIdentity || c.RecoveryIdentity != RecoveryIdentity || c.VerifierIdentity != VerifierIdentity {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "authority", "trusted-kernel authority identities cannot be replaced")
	}
	if c.RecipeCatalog.Digest == "" && len(c.RecipeCatalog.RecipeIDs) != 0 {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "recipe_catalog", "recipe ids require a catalog digest")
	}
	if err := validateStringSet(c.RecipeCatalog.RecipeIDs); err != nil {
		return compositionError(ErrorInvalidContract, ErrInvalidContract, "recipe_catalog.recipe_ids", "%v", err)
	}
	return nil
}

func canonicalContract(c FrozenExecutionContract) FrozenExecutionContract {
	c.ExecutionContractHash = ""
	c.Packages = append([]PackageIdentity(nil), c.Packages...)
	for index := range c.Packages {
		pkg := &c.Packages[index]
		pkg.Actions = sortedStrings(pkg.Actions)
		pkg.Capabilities = sortedStrings(pkg.Capabilities)
		pkg.WorkspaceRequirements = sortedStrings(pkg.WorkspaceRequirements)
		pkg.NetworkRequirements = sortedStrings(pkg.NetworkRequirements)
		pkg.EvidenceRequirements = sortedStrings(pkg.EvidenceRequirements)
		pkg.VerificationRequirements = sortedStrings(pkg.VerificationRequirements)
		pkg.Dependencies = sortedRefs(pkg.Dependencies)
		pkg.Conflicts = sortedRefs(pkg.Conflicts)
	}
	sort.Slice(c.Packages, func(i, j int) bool {
		if c.Packages[i].ID == c.Packages[j].ID {
			return c.Packages[i].Version < c.Packages[j].Version
		}
		return c.Packages[i].ID < c.Packages[j].ID
	})
	c.Tools = canonicalTools(c.Tools)
	c.RecipeCatalog.RecipeIDs = sortedStrings(c.RecipeCatalog.RecipeIDs)
	return c
}

func canonicalTools(tools []ToolIdentity) []ToolIdentity {
	copy := append([]ToolIdentity(nil), tools...)
	for index := range copy {
		copy[index].Arguments = append([]ArgumentIdentity(nil), copy[index].Arguments...)
		sort.Slice(copy[index].Arguments, func(i, j int) bool { return copy[index].Arguments[i].Name < copy[index].Arguments[j].Name })
	}
	sort.Slice(copy, func(i, j int) bool { return copy[i].Name < copy[j].Name })
	return copy
}

func (c FrozenExecutionContract) String() string {
	data, hash, err := c.ContractBytes()
	if err != nil {
		return fmt.Sprintf("invalid contract: %v", err)
	}
	return fmt.Sprintf("%s (%d bytes)", hash, len(data))
}
