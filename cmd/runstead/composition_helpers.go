package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/composition"
	"github.com/RenyEnnos/Runstead/internal/policy"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

var errPersistedExecutionContract = errors.New("persisted execution contract is invalid")

func loadProfileFlag(flags *flag.FlagSet, value string) (composition.Profile, bool, error) {
	return loadProfilePath(value, flagWasSet(flags, "profile"))
}

func loadProfilePath(value string, set bool) (composition.Profile, bool, error) {
	if !set {
		return composition.Profile{}, false, nil
	}
	path := strings.TrimSpace(value)
	if path == "" {
		return composition.Profile{}, false, fmt.Errorf("profile path must not be empty")
	}
	profile, err := composition.LoadProfile(path)
	if err != nil {
		return composition.Profile{}, false, err
	}
	return profile, true, nil
}

func resolveProfileProvider(profile composition.Profile, selectedID string, selected bool) (string, bool, error) {
	profileID := strings.TrimSpace(profile.ProviderID)
	if profileID != "" && selected && profileID != selectedID {
		return "", false, fmt.Errorf("profile provider %q conflicts with --provider-id %q", profileID, selectedID)
	}
	if profileID != "" && !selected {
		return profileID, true, nil
	}
	return selectedID, selected, nil
}

func resolveComposition(profile composition.Profile, providerIdentity provider.Identity, registry *tools.Registry, recipes *recipe.Catalog, writePolicy string, recipePolicy policy.Config, acceptanceDigest string) (composition.Resolved, error) {
	return composition.Resolve(composition.ResolveInput{
		Profile:              profile,
		PackageRegistry:      composition.NewBuiltinRegistry(),
		Provider:             providerIdentity,
		ToolRegistry:         registry,
		Recipes:              recipes,
		RuntimeIdentity:      composition.DefaultRuntimeIdentity,
		ProtocolIdentity:     composition.DefaultProtocolIdentity,
		WritePolicyIdentity:  writePolicy,
		RecipePolicy:         recipePolicy,
		AcceptancePlanDigest: acceptanceDigest,
	})
}

// resolveFrozenComposition reconstructs the operator composition and compares
// its exact canonical bytes and hash with the task's durable contract. The
// persisted contract is authoritative: a changed Profile, package registry,
// tool schema, recipe catalog or provider identity is drift, never an implicit
// upgrade or fallback.
func resolveFrozenComposition(profile composition.Profile, providerIdentity provider.Identity, registry *tools.Registry, recipes *recipe.Catalog, writePolicy string, recipePolicy policy.Config, acceptanceDigest string, frozenJSON, frozenHash string) (composition.Resolved, error) {
	if _, _, err := composition.ValidateContract([]byte(frozenJSON), frozenHash); err != nil {
		return composition.Resolved{}, fmt.Errorf("%w: %v", errPersistedExecutionContract, err)
	}
	resolved, err := resolveComposition(profile, providerIdentity, registry, recipes, writePolicy, recipePolicy, acceptanceDigest)
	if err != nil {
		return composition.Resolved{}, err
	}
	if resolved.ContractHash != frozenHash || !bytes.Equal(resolved.ContractJSON, []byte(frozenJSON)) {
		return composition.Resolved{}, fmt.Errorf("profile composition drift: the resolved contract differs from the task's frozen execution contract")
	}
	return resolved, nil
}
