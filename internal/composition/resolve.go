package composition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// ResolveInput supplies existing non-authoritative seams to composition. The
// resolver does not execute any of them.
type ResolveInput struct {
	Profile             Profile
	PackageRegistry     PackageRegistry
	Provider            provider.Identity
	ToolRegistry        *tools.Registry
	Recipes             *recipe.Catalog
	RuntimeIdentity     string
	ProtocolIdentity    string
	WritePolicyIdentity string
	// RecipePolicy is the operator recipe policy CONFIG (the same config the
	// existing policy seam evaluates). The resolver renders the contract's
	// RecipePolicyIdentity from the EFFECTIVE recipe surface so the frozen
	// identity never references a recipe the Profile did not select. The
	// config is untouched by composition: policy/approval remains the trusted
	// kernel authority.
	RecipePolicy         policy.Config
	AcceptancePlanDigest string
}

// Resolved is the materialized composition and the existing restricted tool
// registry that the caller may pass to the normal agent loop.
type Resolved struct {
	Contract          FrozenExecutionContract
	ContractJSON      []byte
	ContractHash      string
	EffectiveRegistry *tools.Registry
	// EffectiveRecipes is the Profile-selected recipe surface. It contains
	// EXACTLY the recipes available to the task (the whole configured catalog
	// when recipe_ids is empty and run_recipe is enabled, per the documented
	// deliberate semantics). It is never nil when the effective registry
	// exposes run_recipe, so runtime and frozen contract always agree.
	EffectiveRecipes *recipe.Catalog
}

func Resolve(input ResolveInput) (Resolved, error) {
	if err := input.Profile.Validate(); err != nil {
		return Resolved{}, err
	}
	if input.ToolRegistry == nil {
		return Resolved{}, compositionError(ErrorMissingTool, ErrMissingTool, "tool_registry", "the existing tool registry is required")
	}
	if input.PackageRegistry.packages == nil {
		input.PackageRegistry = NewBuiltinRegistry()
	}
	if input.Profile.ProviderID != "" {
		if input.Provider.Empty() || input.Profile.ProviderID != input.Provider.ProviderID {
			return Resolved{}, compositionError(ErrorProviderMismatch, ErrProviderMismatch, "provider_id", "profile provider %q does not match resolved provider", input.Profile.ProviderID)
		}
	}
	runtimeIdentity := strings.TrimSpace(input.RuntimeIdentity)
	if runtimeIdentity == "" {
		runtimeIdentity = DefaultRuntimeIdentity
	}
	protocolIdentity := strings.TrimSpace(input.ProtocolIdentity)
	if protocolIdentity == "" {
		protocolIdentity = DefaultProtocolIdentity
	}

	providerMaterial, err := providerIdentityMaterial(input.Provider)
	if err != nil {
		return Resolved{}, err
	}
	selected := make(map[string]CapabilityPackage, len(input.Profile.Packages))
	selectedVersions := make(map[string]string, len(input.Profile.Packages))
	for index, ref := range input.Profile.Packages {
		if _, exists := selected[packageKey(ref.ID, ref.Version)]; exists {
			return Resolved{}, compositionError(ErrorDuplicatePackage, ErrDuplicatePackage, fmt.Sprintf("packages[%d]", index), "package %q@%q appears more than once", ref.ID, ref.Version)
		}
		pkg, ok := input.PackageRegistry.Lookup(ref.ID, ref.Version)
		if !ok {
			if input.PackageRegistry.HasID(ref.ID) {
				return Resolved{}, compositionError(ErrorUnknownPackageVersion, ErrUnknownPackageVersion, fmt.Sprintf("packages[%d].version", index), "package %q has no version %q", ref.ID, ref.Version)
			}
			return Resolved{}, compositionError(ErrorUnknownPackage, ErrUnknownPackage, fmt.Sprintf("packages[%d].id", index), "package %q is not registered", ref.ID)
		}
		if previousVersion, exists := selectedVersions[pkg.ID]; exists && previousVersion != pkg.Version {
			return Resolved{}, compositionError(ErrorCompositionConflict, ErrCompositionConflict, fmt.Sprintf("packages[%d]", index), "package %q selects incompatible versions %q and %q", pkg.ID, previousVersion, pkg.Version)
		}
		if pkg.RuntimeCompatibility != runtimeIdentity {
			return Resolved{}, compositionError(ErrorCompositionConflict, ErrCompositionConflict, fmt.Sprintf("packages[%d].runtime_compatibility", index), "package %q@%q requires runtime %q, got %q", pkg.ID, pkg.Version, pkg.RuntimeCompatibility, runtimeIdentity)
		}
		selectedVersions[pkg.ID] = pkg.Version
		selected[pkg.ID] = pkg
	}

	selectedIDs := make([]string, 0, len(selected))
	for id := range selected {
		selectedIDs = append(selectedIDs, id)
	}
	sort.Strings(selectedIDs)
	for _, id := range selectedIDs {
		pkg := selected[id]
		for _, dependency := range pkg.Dependencies {
			if _, ok := selected[dependency.ID]; !ok {
				return Resolved{}, compositionError(ErrorCompositionConflict, ErrCompositionConflict, "dependencies", "package %q@%q requires %q@%q to be selected explicitly", pkg.ID, pkg.Version, dependency.ID, dependency.Version)
			}
			selectedDependency := selected[dependency.ID]
			if selectedDependency.Version != dependency.Version {
				return Resolved{}, compositionError(ErrorCompositionConflict, ErrCompositionConflict, "dependencies", "package %q@%q requires %q@%q", pkg.ID, pkg.Version, dependency.ID, dependency.Version)
			}
		}
		for _, conflict := range pkg.Conflicts {
			if selectedConflict, ok := selected[conflict.ID]; ok && selectedConflict.Version == conflict.Version {
				return Resolved{}, compositionError(ErrorCompositionConflict, ErrCompositionConflict, "conflicts", "package %q@%q conflicts with %q@%q", pkg.ID, pkg.Version, conflict.ID, conflict.Version)
			}
		}
	}

	toolSpecs := make(map[string]tools.ToolSpec)
	for _, spec := range input.ToolRegistry.Describe() {
		toolSpecs[spec.Name] = spec
	}
	actionSet := make(map[string]struct{})
	var packages []PackageIdentity
	for _, id := range selectedIDs {
		pkg := selected[id]
		for _, action := range pkg.Actions {
			if _, ok := toolSpecs[action]; !ok {
				return Resolved{}, compositionError(ErrorMissingTool, ErrMissingTool, "packages."+pkg.ID, "package action %q is not present in the existing registry", action)
			}
			actionSet[action] = struct{}{}
		}
		packages = append(packages, packageIdentity(pkg))
	}
	if len(actionSet) == 0 {
		return Resolved{}, compositionError(ErrorMissingTool, ErrMissingTool, "tools", "composition selected no actions")
	}

	actions := make([]string, 0, len(actionSet))
	for action := range actionSet {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	effectiveRegistry, err := input.ToolRegistry.Restricted(actions, "")
	if err != nil {
		return Resolved{}, compositionError(ErrorMissingTool, ErrMissingTool, "tools", "cannot materialize effective registry")
	}

	selectedRecipes := sortedStrings(input.Profile.RecipeIDs)
	usesRecipes := false
	for _, action := range actions {
		if action == tools.ToolRunRecipe {
			usesRecipes = true
			break
		}
	}
	// The effective recipe surface (M10, issue #54): a non-empty recipe_ids is
	// an EXACT allowlist of the recipes available to this composition, and an
	// empty recipe_ids with run_recipe enabled deliberately selects the whole
	// configured catalog (documented in docs/composition.md). The effective
	// catalog is materialized once here and is the ONLY catalog the runtime
	// registry and the frozen contract see, so a recipe configured in the
	// original catalog but absent from the selection can never appear again.
	var effectiveRecipes *recipe.Catalog
	if usesRecipes || len(selectedRecipes) > 0 {
		if input.Recipes == nil {
			return Resolved{}, compositionError(ErrorMissingRecipeCatalog, ErrMissingRecipeCatalog, "recipes", "the existing recipe catalog is required")
		}
		for _, id := range selectedRecipes {
			if _, ok := input.Recipes.Get(id); !ok {
				return Resolved{}, compositionError(ErrorMissingRecipe, ErrMissingRecipe, "recipe_ids", "recipe %q is not present in the catalog", id)
			}
		}
		effectiveIDs := selectedRecipes
		if usesRecipes && len(selectedRecipes) == 0 {
			effectiveIDs = input.Recipes.IDs()
		}
		selected, selectErr := input.Recipes.Select(effectiveIDs)
		if selectErr != nil {
			return Resolved{}, compositionError(ErrorMissingRecipe, ErrMissingRecipe, "recipe_ids", "%v", selectErr)
		}
		effectiveRecipes = selected
	}
	recipeIDs := []string(nil)
	recipeDigest := ""
	if effectiveRecipes != nil {
		recipeIDs = effectiveRecipes.IDs()
		recipeDigest = effectiveRecipes.Digest()
	}

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].ID == packages[j].ID {
			return packages[i].Version < packages[j].Version
		}
		return packages[i].ID < packages[j].ID
	})
	toolsMaterial := make([]ToolIdentity, 0, len(actions))
	for _, action := range actions {
		spec := toolSpecs[action]
		args := make([]ArgumentIdentity, 0, len(spec.Arguments))
		for _, arg := range spec.Arguments {
			args = append(args, ArgumentIdentity{Name: arg.Name, Type: arg.Type, Required: arg.Required, Note: arg.Note})
		}
		sort.Slice(args, func(i, j int) bool { return args[i].Name < args[j].Name })
		toolsMaterial = append(toolsMaterial, ToolIdentity{Name: spec.Name, Summary: spec.Summary, ReadOnly: spec.ReadOnly, Arguments: args})
	}
	toolSchemaDigest, err := digestJSON(toolsMaterial)
	if err != nil {
		return Resolved{}, compositionError(ErrorInvalidContract, ErrInvalidContract, "tool_schema_digest", "cannot encode tool schema")
	}

	contract := FrozenExecutionContract{
		ContractVersion: ContractSchemaVersion, RuntimeIdentity: runtimeIdentity, ProtocolIdentity: protocolIdentity,
		Profile:  ProfileIdentity{ID: input.Profile.ProfileID, Version: input.Profile.ProfileVersion},
		Packages: packages, Provider: providerMaterial, Tools: toolsMaterial, ToolSchemaDigest: toolSchemaDigest,
		RecipeCatalog:       RecipeCatalogIdentity{Digest: recipeDigest, RecipeIDs: recipeIDs},
		WritePolicyIdentity: strings.TrimSpace(input.WritePolicyIdentity),
		// The recipe policy identity is rendered over the EFFECTIVE recipe ids
		// (issue #54 review): the frozen contract must describe exactly the
		// operator policy surface of this composition, never modes for recipes
		// that the Profile did not select.
		RecipePolicyIdentity: strings.TrimSpace(input.RecipePolicy.RecipeSpec(recipeIDs)),
		AcceptancePlanDigest: strings.TrimSpace(input.AcceptancePlanDigest), GovernorIdentity: GovernorIdentity, PolicyIdentity: PolicyIdentity,
		EvidenceIdentity: EvidenceIdentity, RecoveryIdentity: RecoveryIdentity, VerifierIdentity: VerifierIdentity,
	}
	data, hash, err := contract.ContractBytes()
	if err != nil {
		return Resolved{}, err
	}
	contract.ExecutionContractHash = hash
	// The runtime registry and the frozen contract MUST expose the same recipe
	// surface: the restricted tool registry replaces its catalog with the
	// effective selection so executeRunRecipe can never resolve a recipe the
	// Profile did not select.
	return Resolved{Contract: contract, ContractJSON: data, ContractHash: hash, EffectiveRegistry: effectiveRegistry.WithRecipes(effectiveRecipes), EffectiveRecipes: effectiveRecipes}, nil
}

func packageIdentity(pkg CapabilityPackage) PackageIdentity {
	return PackageIdentity{
		ID: pkg.ID, Version: pkg.Version, Provenance: pkg.Provenance, Kind: pkg.Kind, RuntimeCompatibility: pkg.RuntimeCompatibility,
		Actions: sortedStrings(pkg.Actions), Capabilities: sortedStrings(pkg.Capabilities), WorkspaceRequirements: sortedStrings(pkg.WorkspaceRequirements),
		NetworkRequirements: sortedStrings(pkg.NetworkRequirements), EffectClass: pkg.EffectClass, RecoveryClass: pkg.RecoveryClass,
		EvidenceRequirements: sortedStrings(pkg.EvidenceRequirements), VerificationRequirements: sortedStrings(pkg.VerificationRequirements),
		ApprovalBoundary: pkg.ApprovalBoundary, MaxOutputBytes: pkg.MaxOutputBytes, Dependencies: sortedRefs(pkg.Dependencies), Conflicts: sortedRefs(pkg.Conflicts),
	}
}

func sortedRefs(refs []PackageRef) []PackageRef {
	copy := append([]PackageRef(nil), refs...)
	sort.Slice(copy, func(i, j int) bool {
		if copy[i].ID == copy[j].ID {
			return copy[i].Version < copy[j].Version
		}
		return copy[i].ID < copy[j].ID
	})
	return copy
}

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
