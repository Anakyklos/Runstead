package composition

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
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
