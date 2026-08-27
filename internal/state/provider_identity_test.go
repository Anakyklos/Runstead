package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// TestProviderIdentityPersistedAndRendered proves the #14 provider-neutral
// execution identity (protocol family, sanitized config identity, sanitized
// request id) is durably persisted per attempt and rendered by inspect,
// and that credential-shaped content in any of those fields is redacted
// before it can reach the store.
func TestProviderIdentityPersistedAndRendered(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(Options{Path: dir + "/runstead.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	const taskID = "task-identity"
	if err := store.CreateTask(ctx, TaskRecord{TaskID: taskID, Objective: "objective", Workspace: dir}); err != nil {
		t.Fatal(err)
	}
	prepared := governor.ProviderPrepared{
		TaskID:          taskID,
		ClientRequestID: "req-000001",
		ProviderID:      "identity-a",
		ModelPool:       "instant",
		Model:           "model-a",
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		ConfigIdentity:  "provider.Config{ProviderID:\"identity-a\" ProtocolFamily:\"openai_compatible\" Endpoint:\"https://example.invalid/v1\" Model:\"model-a\" AuthRequirement:\"none\"}",
		AttemptSequence: 1,
		StartedAt:       time.Now().UTC(),
		State:           governor.PersistedState{AccountPolicyID: "policy", ProviderID: "identity-a"},
	}
	if err := store.RecordProviderPrepared(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	finished := governor.ProviderFinished{
		TaskID:          taskID,
		ClientRequestID: "req-000001",
		Outcome:         governor.OutcomeSuccess,
		UpstreamReached: true,
		DeliveryState:   provider.DeliveryCompleted,
		AttemptDebited:  1,
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		ConfigIdentity:  prepared.ConfigIdentity,
		RequestID:       "sk-Bearer-credential-shaped-value-should-be-redacted",
		State:           governor.PersistedState{AccountPolicyID: "policy", ProviderID: "identity-a"},
	}
	if err := store.RecordProviderFinished(ctx, finished); err != nil {
		t.Fatal(err)
	}

	var rendered strings.Builder
	if err := store.RenderInspect(ctx, &rendered, taskID); err != nil {
		t.Fatal(err)
	}
	output := rendered.String()
	for _, want := range []string{
		"provider=identity-a",
		"family=openai_compatible",
		"config_identity=provider.Config{ProviderID:\"identity-a\"",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("inspect missing %q:\n%s", want, output)
		}
	}
	// The credential-shaped request id must never reach the store raw.
	if strings.Contains(output, "Bearer-credential-shaped-value-should-be-redacted") {
		t.Fatalf("inspect rendered the credential-shaped request id:\n%s", output)
	}
	if !strings.Contains(output, "request_id=<redacted>") {
		t.Fatalf("inspect must render the redacted request id:\n%s", output)
	}
	raw, err := store.db.QueryContext(ctx, "SELECT request_id, config_identity FROM provider_attempts WHERE task_id = ?", taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if !raw.Next() {
		t.Fatal("provider attempt row missing")
	}
	var requestID, configIdentity string
	if err := raw.Scan(&requestID, &configIdentity); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requestID, "credential-shaped") {
		t.Fatalf("durable request_id leaked credential shape: %q", requestID)
	}
	if !strings.Contains(configIdentity, "identity-a") {
		t.Fatalf("durable config identity missing the sanitized provider identity: %q", configIdentity)
	}
}

// TestProviderIdentityFromConfigSnapshotIsStableAndSanitized covers the
// extraction of the persisted identity from the task configuration snapshot.
func TestProviderIdentityFromConfigSnapshotIsStableAndSanitized(t *testing.T) {
	snapshot := `{"provider_id":"p1","protocol_family":"google_compatible","provider_model":"m1","provider_config_identity":"provider.Config{...}","provider_profile_version":"v1","provider_adapter_version":"compatible-provider-v0.1"}`
	identity := ProviderIdentityFromConfigSnapshot(snapshot)
	if identity.ProviderID != "p1" || identity.ProtocolFamily != "google_compatible" || identity.Model != "m1" {
		t.Fatalf("identity extraction wrong: %#v", identity)
	}
	if identity.AdapterVersion != "compatible-provider-v0.1" {
		t.Fatalf("adapter version = %q", identity.AdapterVersion)
	}
	// Missing/empty identity renders nothing and never guesses.
	if empty := ProviderIdentityFromConfigSnapshot(`{}`); empty.ProviderID != "" {
		t.Fatalf("empty snapshot must render no identity")
	}
}
