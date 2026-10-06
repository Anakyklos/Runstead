package main

import (
	"fmt"
	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"os"
)

func main() {
	const id = "mistral-small-4-canary-v1"
	const base = "https://api.mistral.ai/v1"
	const model = "mistral-small-2603"
	const auth = "MISTRAL_API_KEY"
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: stage1_resolve PROVIDERS.json")
		os.Exit(2)
	}
	registry, err := config.LoadProvidersFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider declaration rejected")
		os.Exit(1)
	}
	resolved, err := registry.Resolve(id, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider resolution rejected")
		os.Exit(1)
	}
	if resolved.ProviderID != id || resolved.ProtocolFamily != provider.FamilyOpenAICompatible || resolved.BaseURL != base || resolved.Model != model || resolved.AuthRequirement != provider.AuthReferenceRequired || resolved.Auth != auth || resolved.Profile.RouteSafety != provider.SafeRouteSafety() {
		fmt.Fprintln(os.Stderr, "resolved provider differs from the fixed contract")
		os.Exit(1)
	}
	fmt.Printf("provider_id=%s\nprotocol_family=%s\nbase_url=%s\nmodel=%s\nauth_requirement=reference_required\nauth_ref=MISTRAL_API_KEY\nroute_safety=SafeRouteSafety\nadapter_constructed=false\nprovider_requests=0\n", id, resolved.ProtocolFamily, base, model)
}
