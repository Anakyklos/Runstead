// Command stage1_resolve validates the exact provider declaration without
// constructing an adapter or dispatching a provider request.
package main

import (
	"fmt"
	"os"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

const (
	providerID = "groq-gpt-oss-120b-canary-v5"
	baseURL    = "https://api.groq.com/openai/v1"
	modelID    = "openai/gpt-oss-120b"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: stage1_resolve PROVIDERS.json")
		os.Exit(2)
	}
	registry, err := config.LoadProvidersFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider declaration rejected: %v\n", err)
		os.Exit(1)
	}
	resolved, err := registry.Resolve(providerID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider resolution rejected: %v\n", err)
		os.Exit(1)
	}
	if resolved.ProviderID != providerID || resolved.ProtocolFamily != provider.FamilyOpenAICompatible ||
		resolved.BaseURL != baseURL || resolved.Model != modelID ||
		resolved.AuthRequirement != provider.AuthReferenceRequired || resolved.Auth != "GROQ_API_KEY" {
		fmt.Fprintln(os.Stderr, "resolved provider differs from the Stage 1 contract")
		os.Exit(1)
	}
	fmt.Printf("provider_id=%s\nprotocol_family=%s\nbase_url=%s\nmodel=%s\nauth_requirement=%s\nauth_ref_configured=true\nconfig_identity=%s\nadapter_constructed=false\nprovider_dispatches=0\n",
		resolved.ProviderID, resolved.ProtocolFamily, resolved.BaseURL, resolved.Model,
		resolved.AuthRequirement, resolved.ConfigIdentity)
}
