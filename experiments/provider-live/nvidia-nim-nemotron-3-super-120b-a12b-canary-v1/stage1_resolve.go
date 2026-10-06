package main

import (
	"fmt"
	"os"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func main() {
	const id = "nvidia-nim-nemotron-3-super-120b-a12b-canary-v1"
	const base = "https://integrate.api.nvidia.com/v1"
	const model = "nvidia/nemotron-3-super-120b-a12b"
	const auth = "NVIDIA_API_KEY"
	if len(os.Args) != 2 || os.Getenv(auth) == "" {
		fmt.Fprintln(os.Stderr, "fixed preflight inputs unavailable")
		os.Exit(2)
	}
	registry, err := config.LoadProvidersFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider declaration rejected")
		os.Exit(1)
	}
	resolved, err := registry.Resolve(id, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil || resolved.ProviderID != id || resolved.ProtocolFamily != provider.FamilyOpenAICompatible || resolved.BaseURL != base || resolved.Model != model || resolved.AuthRequirement != provider.AuthReferenceRequired || resolved.Auth != auth || !resolved.Profile.RouteSafety.Equal(provider.SafeRouteSafety()) {
		fmt.Fprintln(os.Stderr, "resolved provider differs from fixed contract")
		os.Exit(1)
	}
	fmt.Printf("provider_id=%s\nprotocol_family=%s\nbase_url=%s\nmodel=%s\nauth_requirement=reference_required\nauth_ref=NVIDIA_API_KEY\nroute_safety=SafeRouteSafety\nadapter_constructed=false\nprovider_requests=0\n", id, resolved.ProtocolFamily, base, model)
}
