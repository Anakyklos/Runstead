package composition

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

func TestParseProfileRejectsUnknownFieldsAndDuplicateKeys(t *testing.T) {
	tests := []string{
		`{"version":1,"profile_id":"audit","profile_version":"1.0.0","packages":[],"policy":"allow"}`,
		`{"version":1,"profile_id":"audit","profile_id":"other","profile_version":"1.0.0","packages":[]}`,
	}
	for _, input := range tests {
		if _, err := ParseProfile([]byte(input)); err == nil || !errors.Is(err, ErrInvalidProfile) {
			t.Fatalf("ParseProfile(%s) error = %v, want ErrInvalidProfile", input, err)
		}
	}
}

func TestParseProfileRejectsUnknownVersionAndMissingPackageVersion(t *testing.T) {
	for _, input := range []string{
		`{"version":2,"profile_id":"audit","profile_version":"1.0.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`,
		`{"version":1,"profile_id":"audit","profile_version":"1.0.0","packages":[{"id":"repo.read"}]}`,
	} {
		if _, err := ParseProfile([]byte(input)); err == nil || !errors.Is(err, ErrInvalidProfile) {
			t.Fatalf("ParseProfile(%s) error = %v, want ErrInvalidProfile", input, err)
		}
	}
}

func TestResolveBuiltinsProducesSortedEffectiveToolsAndStableHash(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{
		Version:        ProfileSchemaVersion,
		ProfileID:      "audit",
		ProfileVersion: "1.0.0",
		Packages: []PackageRef{
			{ID: "repo.write", Version: "1.0.0"},
			{ID: "repo.read", Version: "1.0.0"},
		},
	}
	input := ResolveInput{
		Profile:          profile,
		PackageRegistry:  NewBuiltinRegistry(),
		ToolRegistry:     registry,
		RuntimeIdentity:  "runstead-runtime.v1",
		ProtocolIdentity: "runstead.protocol.v1",
	}
	first, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	profile.Packages[0], profile.Packages[1] = profile.Packages[1], profile.Packages[0]
	input.Profile = profile
	second, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContractHash == "" || first.ContractHash != second.ContractHash {
		t.Fatalf("order changed contract hash: %q vs %q", first.ContractHash, second.ContractHash)
	}
	if !bytes.Equal(first.ContractJSON, second.ContractJSON) {
		t.Fatal("order changed canonical contract bytes")
	}
	wantTools := []string{tools.ToolApplyPatch, tools.ToolGitDiff, tools.ToolGitStatus, tools.ToolListFiles, tools.ToolReadFile, tools.ToolSearchText, tools.ToolWriteFile}
	if got := first.Contract.ToolNames(); !equalStrings(got, wantTools) {
		t.Fatalf("effective tools = %#v, want %#v", got, wantTools)
	}
	if first.EffectiveRegistry == nil || !first.EffectiveRegistry.Allows(tools.ToolWriteFile) || first.EffectiveRegistry.Allows(tools.ToolRunRecipe) {
		t.Fatal("effective registry did not contain exactly the selected package tools")
	}
}

func TestResolveRejectsUnknownPackageAndVersionBeforeExecution(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	base := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}}
	for _, ref := range []PackageRef{{ID: "unknown", Version: "1.0.0"}, {ID: "repo.read", Version: "9.9.9"}} {
		base.Packages[0] = ref
		_, err := Resolve(ResolveInput{Profile: base, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry})
		if err == nil || (!errors.Is(err, ErrUnknownPackage) && !errors.Is(err, ErrUnknownPackageVersion)) {
			t.Fatalf("Resolve(%#v) error = %v, want unknown package/version", ref, err)
		}
	}
}

func TestResolveRejectsDuplicateAndConflictingPackages(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	duplicate := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}, {ID: "repo.read", Version: "1.0.0"}}}
	if _, err := Resolve(ResolveInput{Profile: duplicate, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry}); err == nil || !errors.Is(err, ErrDuplicatePackage) {
		t.Fatalf("duplicate Resolve error = %v, want ErrDuplicatePackage", err)
	}
	versioned, err := NewPackageRegistry(
		CapabilityPackage{ID: "versioned", Version: "1.0.0", Provenance: "test", Kind: PackageKindBuiltin, RuntimeCompatibility: DefaultRuntimeIdentity, Actions: []string{tools.ToolReadFile}, MaxOutputBytes: 1},
		CapabilityPackage{ID: "versioned", Version: "2.0.0", Provenance: "test", Kind: PackageKindBuiltin, RuntimeCompatibility: DefaultRuntimeIdentity, Actions: []string{tools.ToolReadFile}, MaxOutputBytes: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "versioned", Version: "1.0.0"}, {ID: "versioned", Version: "2.0.0"}}}
	if _, err := Resolve(ResolveInput{Profile: ambiguous, PackageRegistry: versioned, ToolRegistry: registry}); err == nil || !errors.Is(err, ErrCompositionConflict) {
		t.Fatalf("ambiguous versions Resolve error = %v, want ErrCompositionConflict", err)
	}
	incompatible, err := NewPackageRegistry(CapabilityPackage{
		ID: "incompatible", Version: "1.0.0", Provenance: "test", Kind: PackageKindBuiltin,
		RuntimeCompatibility: "runstead-runtime.v2", Actions: []string{tools.ToolReadFile}, MaxOutputBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ResolveInput{Profile: Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "incompatible", Version: "1.0.0"}}}, PackageRegistry: incompatible, ToolRegistry: registry}); err == nil || !errors.Is(err, ErrCompositionConflict) {
		t.Fatalf("runtime-incompatible Resolve error = %v, want ErrCompositionConflict", err)
	}
	custom, err := NewPackageRegistry(
		CapabilityPackage{ID: "a", Version: "1.0.0", Provenance: "test", Kind: PackageKindBuiltin, RuntimeCompatibility: DefaultRuntimeIdentity, Actions: []string{tools.ToolReadFile}, MaxOutputBytes: 1, Conflicts: []PackageRef{{ID: "b", Version: "1.0.0"}}},
		CapabilityPackage{ID: "b", Version: "1.0.0", Provenance: "test", Kind: PackageKindBuiltin, RuntimeCompatibility: DefaultRuntimeIdentity, Actions: []string{tools.ToolListFiles}, MaxOutputBytes: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	conflict := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "a", Version: "1.0.0"}, {ID: "b", Version: "1.0.0"}}}
	if _, err := Resolve(ResolveInput{Profile: conflict, PackageRegistry: custom, ToolRegistry: registry}); err == nil || !errors.Is(err, ErrCompositionConflict) {
		t.Fatalf("conflict Resolve error = %v, want ErrCompositionConflict", err)
	}
}

func TestContractHashExcludesProviderSecretsAndChangesOnMaterialInput(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}}
	input := ResolveInput{
		Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry,
		Provider: provider.Identity{
			ProviderID: "local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "model",
			ConfigIdentity: "provider.Config{Endpoint:\"http://localhost\"}", ProfileVersion: "1.0.0", AdapterVersion: "compat v1",
		},
	}
	first, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first.ContractJSON), "secret") || strings.Contains(string(first.ContractJSON), "Authorization") {
		t.Fatalf("contract contains secret-like content: %s", first.ContractJSON)
	}
	input.Profile.ProfileVersion = "2.0.0"
	second, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContractHash == second.ContractHash {
		t.Fatal("material profile version change did not change contract hash")
	}
	if _, _, err := ValidateContract(first.ContractJSON, first.ContractHash); err != nil {
		t.Fatalf("ValidateContract() error = %v", err)
	}
	if _, _, err := ValidateContract(first.ContractJSON, "sha256:bad"); err == nil || !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("ValidateContract with bad hash = %v, want ErrInvalidContract", err)
	}
}

func TestContractRejectsTrustedKernelIdentityReplacement(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveInput{
		Profile:         Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}},
		PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*FrozenExecutionContract)
	}{
		{name: "governor", mutate: func(contract *FrozenExecutionContract) { contract.GovernorIdentity = "operator-governor" }},
		{name: "policy", mutate: func(contract *FrozenExecutionContract) { contract.PolicyIdentity = "operator-policy" }},
		{name: "evidence", mutate: func(contract *FrozenExecutionContract) { contract.EvidenceIdentity = "operator-evidence" }},
		{name: "recovery", mutate: func(contract *FrozenExecutionContract) { contract.RecoveryIdentity = "operator-recovery" }},
		{name: "verifier", mutate: func(contract *FrozenExecutionContract) { contract.VerifierIdentity = "operator-verifier" }},
		{name: "runtime", mutate: func(contract *FrozenExecutionContract) {
			contract.Packages[0].RuntimeCompatibility = "operator-runtime"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := resolved.Contract
			test.mutate(&contract)
			if _, _, err := contract.ContractBytes(); err == nil || !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("tampered contract error = %v, want ErrInvalidContract", err)
			}
		})
	}
}

func TestResolveRejectsUnsanitizedProviderIdentity(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	base := ResolveInput{
		Profile:         Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}},
		PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry,
	}
	for _, identity := range []provider.Identity{
		{ProviderID: "local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "model", ConfigIdentity: "api_key=should-not-persist", ProfileVersion: "1.0.0", AdapterVersion: "compat v1"},
		{ProviderID: "local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "secret-token", ConfigIdentity: "provider.Config{Endpoint:\"http://localhost\"}", ProfileVersion: "1.0.0", AdapterVersion: "compat v1"},
		{ProviderID: "local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "model", ConfigIdentity: "raw-config", ProfileVersion: "1.0.0", AdapterVersion: "compat v1"},
	} {
		base.Provider = identity
		if _, err := Resolve(base); err == nil || !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("Resolve(%#v) error = %v, want ErrInvalidContract", identity, err)
		}
	}
}

func TestResolveRecipeIdentityRequiresExistingCatalog(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Version: 1, ProfileID: "build", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "process.recipes", Version: "1.0.0"}}, RecipeIDs: []string{"test"}}
	_, err = Resolve(ResolveInput{Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry})
	if err == nil || !errors.Is(err, ErrMissingRecipeCatalog) {
		t.Fatalf("missing catalog error = %v, want ErrMissingRecipeCatalog", err)
	}
}

func twoRecipeCatalog(t *testing.T) *recipe.Catalog {
	t.Helper()
	catalog, err := recipe.NewCatalog([]recipe.Recipe{
		{ID: "go-test", Executable: "/bin/echo", Argv: []string{"go-test"}, Capabilities: []recipe.Capability{"execute_repository_code"}},
		{ID: "deploy", Executable: "/bin/echo", Argv: []string{"deploy"}, Capabilities: []recipe.Capability{"execute_repository_code"}},
	})
	if err != nil {
		t.Fatalf("recipe.NewCatalog() error = %v", err)
	}
	return catalog
}

// TestResolveRecipeIDsExactAllowlistSurface proves the M10 recipe_ids blocker
// fix (issue #54 review): a non-empty recipe_ids is an EXACT allowlist. The
// frozen contract, the effective catalog and the runtime registry all expose
// ONLY the selected recipe, and a recipe present in the configured catalog but
// absent from the selection never appears in any of them.
func TestResolveRecipeIDsExactAllowlistSurface(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := twoRecipeCatalog(t)
	profile := Profile{Version: 1, ProfileID: "build", ProfileVersion: "1.0.0",
		Packages:  []PackageRef{{ID: "process.recipes", Version: "1.0.0"}},
		RecipeIDs: []string{"go-test"},
	}
	resolved, err := Resolve(ResolveInput{Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry, Recipes: catalog})
	if err != nil {
		t.Fatal(err)
	}
	// Frozen contract surface: exactly the selection, with its own digest.
	if got := resolved.Contract.RecipeCatalog.RecipeIDs; !equalStrings(got, []string{"go-test"}) {
		t.Fatalf("contract recipe ids = %#v, want [go-test]", got)
	}
	effectiveOnly, err := catalog.Select([]string{"go-test"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Contract.RecipeCatalog.Digest != effectiveOnly.Digest() {
		t.Fatalf("contract digest %q != effective selection digest %q", resolved.Contract.RecipeCatalog.Digest, effectiveOnly.Digest())
	}
	if resolved.Contract.RecipeCatalog.Digest == catalog.Digest() {
		t.Fatal("contract digest must differ from the full configured catalog digest")
	}
	// Effective catalog: only the selected recipe exists.
	if resolved.EffectiveRecipes == nil || resolved.EffectiveRecipes.Len() != 1 {
		t.Fatalf("effective recipes = %v, want exactly go-test", resolved.EffectiveRecipes)
	}
	if _, ok := resolved.EffectiveRecipes.Get("go-test"); !ok {
		t.Fatal("go-test must remain available in the effective surface")
	}
	if _, ok := resolved.EffectiveRecipes.Get("deploy"); ok {
		t.Fatal("deploy must not be available in the effective surface")
	}
	// Runtime registry: the same surface, so executeRunRecipe cannot resolve
	// a non-selected recipe.
	if got := resolved.EffectiveRegistry.RecipeCatalog().IDs(); !equalStrings(got, []string{"go-test"}) {
		t.Fatalf("registry recipe ids = %#v, want [go-test]", got)
	}
	if _, ok := resolved.EffectiveRegistry.Recipe("go-test"); !ok {
		t.Fatal("registry must resolve go-test")
	}
	if _, ok := resolved.EffectiveRegistry.Recipe("deploy"); ok {
		t.Fatal("registry must not resolve deploy")
	}
	if !resolved.EffectiveRegistry.Allows(tools.ToolRunRecipe) {
		t.Fatal("run_recipe must be enabled by the process.recipes package")
	}
}

// TestResolveEmptyRecipeIDsSelectsWholeCatalogDeliberately pins the documented
// semantics: an EMPTY recipe_ids with run_recipe enabled deliberately selects
// the whole configured catalog, and the effective digest equals the full
// catalog digest. This behavior is intentional and regression-tested, never
// accidental.
func TestResolveEmptyRecipeIDsSelectsWholeCatalogDeliberately(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := twoRecipeCatalog(t)
	profile := Profile{Version: 1, ProfileID: "build", ProfileVersion: "1.0.0",
		Packages: []PackageRef{{ID: "process.recipes", Version: "1.0.0"}},
	}
	resolved, err := Resolve(ResolveInput{Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry, Recipes: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Contract.RecipeCatalog.RecipeIDs; !equalStrings(got, catalog.IDs()) {
		t.Fatalf("empty recipe_ids contract ids = %#v, want whole catalog %#v", got, catalog.IDs())
	}
	if resolved.Contract.RecipeCatalog.Digest != catalog.Digest() {
		t.Fatalf("empty recipe_ids digest %q != configured catalog digest %q", resolved.Contract.RecipeCatalog.Digest, catalog.Digest())
	}
	if resolved.EffectiveRecipes == nil || resolved.EffectiveRecipes.Len() != catalog.Len() {
		t.Fatalf("empty recipe_ids effective surface = %v, want the whole catalog", resolved.EffectiveRecipes)
	}
	if _, ok := resolved.EffectiveRegistry.Recipe("deploy"); !ok {
		t.Fatal("empty recipe_ids with run_recipe enabled must keep deploy available")
	}
}

// TestResolveRecipeSelectionDriftChangesContract proves a material change in
// the recipe selection changes the frozen contract hash, so resume rejects it
// as drift instead of silently widening or narrowing the surface.
func TestResolveRecipeSelectionDriftChangesContract(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := twoRecipeCatalog(t)
	base := ResolveInput{Profile: Profile{Version: 1, ProfileID: "build", ProfileVersion: "1.0.0",
		Packages: []PackageRef{{ID: "process.recipes", Version: "1.0.0"}}},
		PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry, Recipes: catalog}
	onlyTest := base
	onlyTest.Profile.RecipeIDs = []string{"go-test"}
	onlyTestResolved, err := Resolve(onlyTest)
	if err != nil {
		t.Fatal(err)
	}
	widened := base
	widened.Profile.RecipeIDs = []string{"go-test", "deploy"}
	widenedResolved, err := Resolve(widened)
	if err != nil {
		t.Fatal(err)
	}
	if onlyTestResolved.ContractHash == widenedResolved.ContractHash {
		t.Fatal("widening the recipe selection must change the contract hash")
	}
	narrowed := base
	narrowed.Profile.RecipeIDs = []string{"deploy"}
	narrowedResolved, err := Resolve(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if onlyTestResolved.ContractHash == narrowedResolved.ContractHash {
		t.Fatal("changing the recipe selection must change the contract hash")
	}
	// Recipe order is not semantic: reordering the SAME selection keeps the
	// exact contract bytes and hash.
	reordered := onlyTest
	reordered.Profile.RecipeIDs = []string{"go-test", "deploy"}
	reorderedResolved, err := Resolve(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(reorderedResolved.Contract.RecipeCatalog.RecipeIDs, []string{"deploy", "go-test"}) {
		t.Fatalf("reordered selection ids = %#v, want sorted [deploy go-test]", reorderedResolved.Contract.RecipeCatalog.RecipeIDs)
	}
	if reorderedResolved.ContractHash != widenedResolved.ContractHash {
		t.Fatal("reordering the same recipe selection must not change the contract hash")
	}
}

// TestResolveRecipeIDSWithoutRunRecipePackageStaysInert proves recipe_ids
// without a run_recipe tool records the effective selection but exposes no
// recipe tool: the surface cannot execute anything.
func TestResolveRecipeIDSWithoutRunRecipePackageStaysInert(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := twoRecipeCatalog(t)
	profile := Profile{Version: 1, ProfileID: "read", ProfileVersion: "1.0.0",
		Packages:  []PackageRef{{ID: "repo.read", Version: "1.0.0"}},
		RecipeIDs: []string{"go-test"},
	}
	resolved, err := Resolve(ResolveInput{Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry, Recipes: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Contract.RecipeCatalog.RecipeIDs; !equalStrings(got, []string{"go-test"}) {
		t.Fatalf("contract recipe ids = %#v, want [go-test]", got)
	}
	if resolved.Contract.RecipeCatalog.Digest == "" {
		t.Fatal("contract must carry the effective selection digest")
	}
	if resolved.EffectiveRegistry.Allows(tools.ToolRunRecipe) {
		t.Fatal("without the process.recipes package no run_recipe tool may exist")
	}
	if resolved.EffectiveRecipes == nil || resolved.EffectiveRecipes.Len() != 1 {
		t.Fatalf("effective recipes = %v, want inert go-test selection", resolved.EffectiveRecipes)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestResolveRecipePolicyIdentityFromEffectiveSurface proves the issue #54
// review fix: the frozen contract's RecipePolicyIdentity is derived from the
// EFFECTIVE recipe ids, never from the full configured catalog. A policy mode
// assigned to an unselected recipe (deploy=deny) does not appear in the
// contract identity, and a mode change of a SELECTED recipe changes the
// contract material.
func TestResolveRecipePolicyIdentityFromEffectiveSurface(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := twoRecipeCatalog(t)
	base := ResolveInput{
		Profile: Profile{Version: 1, ProfileID: "build", ProfileVersion: "1.0.0",
			Packages:  []PackageRef{{ID: "process.recipes", Version: "1.0.0"}},
			RecipeIDs: []string{"go-test"}},
		PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry, Recipes: catalog,
		RecipePolicy: policy.Config{RecipeModes: map[string]policy.Mode{
			"go-test": policy.ModeAllow, "deploy": policy.ModeDeny,
		}},
	}

	// The unselected recipe's mode must be absent from the frozen identity.
	resolved, err := Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Contract.RecipePolicyIdentity != "go-test=allow" {
		t.Fatalf("contract recipe policy identity = %q, want effective surface only (go-test=allow)", resolved.Contract.RecipePolicyIdentity)
	}
	if strings.Contains(resolved.Contract.RecipePolicyIdentity, "deploy") {
		t.Fatalf("contract recipe policy identity must not mention the unselected recipe: %q", resolved.Contract.RecipePolicyIdentity)
	}

	// Changing the mode of the SELECTED recipe is material contract drift.
	denied := base
	denied.RecipePolicy = policy.Config{RecipeModes: map[string]policy.Mode{
		"go-test": policy.ModeDeny, "deploy": policy.ModeDeny,
	}}
	deniedResolved, err := Resolve(denied)
	if err != nil {
		t.Fatal(err)
	}
	if deniedResolved.Contract.RecipePolicyIdentity != "go-test=deny" {
		t.Fatalf("denied contract recipe policy identity = %q, want go-test=deny", deniedResolved.Contract.RecipePolicyIdentity)
	}
	if resolved.ContractHash == deniedResolved.ContractHash {
		t.Fatal("changing the selected recipe's policy mode must change the contract hash")
	}

	// Empty recipe_ids (whole-catalog surface) renders the identity over the
	// full surface, including every configured mode.
	whole := base
	whole.Profile.RecipeIDs = nil
	wholeResolved, err := Resolve(whole)
	if err != nil {
		t.Fatal(err)
	}
	if wholeResolved.Contract.RecipePolicyIdentity != "deploy=deny,go-test=allow" {
		t.Fatalf("whole-catalog identity = %q, want deploy=deny,go-test=allow", wholeResolved.Contract.RecipePolicyIdentity)
	}
}
