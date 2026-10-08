package composition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

func v2IdentityTestDigest(behavior, account, credential string) string {
	sum := sha256.Sum256([]byte(behavior + "\x00" + account + "\x00" + credential))
	return "provider.v2:sha256:" + hex.EncodeToString(sum[:])
}

func TestSIWCFrozenContractV2AndLegacyV1RemainSeparate(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	input := ResolveInput{
		Profile:         Profile{Version: 1, ProfileID: "audit", ProfileVersion: "1.0.0", Packages: []PackageRef{{ID: "repo.read", Version: "1.0.0"}}},
		PackageRegistry: NewBuiltinRegistry(), ToolRegistry: registry,
		Provider: provider.Identity{
			ProviderID: "siwc-local", ProtocolFamily: provider.FamilyOpenAICompatible, Model: "exact-model",
			ConfigIdentity: v2IdentityTestDigest("sha256:"+strings.Repeat("d", 64), "hmac-sha256:v1:"+strings.Repeat("b", 64), "hmac-sha256:v1:"+strings.Repeat("c", 64)), ProfileVersion: "v1", AdapterVersion: "responses-siwc-v1",
			WireContract:      provider.WireResponsesSIWCV1,
			AccountBinding:    "hmac-sha256:v1:" + strings.Repeat("b", 64),
			CredentialBinding: "hmac-sha256:v1:" + strings.Repeat("c", 64),
			BehaviorDigest:    "sha256:" + strings.Repeat("d", 64),
		},
	}
	v2, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Contract.ContractVersion != 2 {
		t.Fatalf("contract schema %d want 2", v2.Contract.ContractVersion)
	}
	decoded, hash, err := ValidateContract(v2.ContractJSON, v2.ContractHash)
	if err != nil || hash != v2.ContractHash || decoded.Provider.WireContract != "responses_siwc_v1" {
		t.Fatalf("v2 failed roundtrip: %v", err)
	}
	input.Provider.CredentialBinding = "hmac-sha256:v1:" + strings.Repeat("e", 64)
	if _, err := Resolve(input); err == nil {
		t.Fatal("v2 accepted mismatched frozen identity and credential binding")
	}
	input.Provider.ConfigIdentity = v2IdentityTestDigest(input.Provider.BehaviorDigest, input.Provider.AccountBinding, input.Provider.CredentialBinding)
	drift, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(drift.ContractJSON, v2.ContractJSON) || drift.ContractHash == v2.ContractHash {
		t.Fatal("credential drift preserved frozen contract hash")
	}
	input.Provider.WireContract = ""
	input.Provider.AccountBinding = ""
	input.Provider.CredentialBinding = ""
	input.Provider.BehaviorDigest = ""
	input.Provider.ConfigIdentity = "provider.Config{Endpoint:\"https://example.invalid\"}"
	legacy, err := Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Contract.ContractVersion != 1 {
		t.Fatalf("legacy contract schema = %d", legacy.Contract.ContractVersion)
	}
	if _, _, err := ValidateContract(legacy.ContractJSON, legacy.ContractHash); err != nil {
		t.Fatalf("legacy not readable: %v", err)
	}
	if bytes.Contains(legacy.ContractJSON, []byte("wire_contract")) {
		t.Fatal("legacy canonical bytes gained v2 fields")
	}
}
