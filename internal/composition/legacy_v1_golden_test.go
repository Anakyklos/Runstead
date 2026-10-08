package composition

import (
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// This fixture's digest and byte length were measured independently from
// origin/main d36418656407f86a9c5c9745e11059baf265d418 before SIWC.
// It is not regenerated from the implementation under test.
func TestLegacyV1FrozenContractMatchesPreSIWCBaseline(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0",
		Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}}
	resolved, err := Resolve(ResolveInput{
		Profile: profile, PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry,
		Provider: provider.Identity{
			ProviderID: "local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "model",
			ConfigIdentity: "provider.Config{Endpoint:\"http://localhost\"}",
			ProfileVersion: "1.0.0", AdapterVersion: "compat v1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const oldHash = "sha256:787f47760512880a22e5118c50a8e575fc77851d26a350452101cc9ba752ea84"
	if resolved.ContractHash != oldHash || len(resolved.ContractJSON) != 2116 {
		t.Fatalf("v1 changed: hash=%s bytes=%d (want %s / 2116)", resolved.ContractHash, len(resolved.ContractJSON), oldHash)
	}
	if _, _, err := ValidateContract(resolved.ContractJSON, resolved.ContractHash); err != nil {
		t.Fatalf("legacy validation rejected: %v", err)
	}
}
