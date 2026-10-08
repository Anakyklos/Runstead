package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/siwcstate"
	"github.com/RenyEnnos/Runstead/internal/state"
)

func cliSIWCIdentity() provider.Identity {
	identity := provider.Identity{
		WireContract:      provider.WireResponsesSIWCV1,
		AccountBinding:    "hmac-sha256:v1:" + strings.Repeat("a", 64),
		CredentialBinding: "hmac-sha256:v1:" + strings.Repeat("b", 64),
		BehaviorDigest:    "sha256:" + strings.Repeat("c", 64),
		ProviderID:        "siwc-cli-test",
		ProtocolFamily:    provider.FamilyOpenAICompatible,
		Model:             "gpt-test",
	}
	sum := sha256.Sum256([]byte(identity.BehaviorDigest + "\x00" + identity.AccountBinding + "\x00" + identity.CredentialBinding))
	identity.ConfigIdentity = "provider.v2:sha256:" + hex.EncodeToString(sum[:])
	return identity
}

func seedSIWCDomain(t *testing.T, home, xdg, stateDir, taskID string, withPendingApproval bool, registeredIdentity ...provider.Identity) provider.Identity {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(xdg, "runstead", "siwc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(xdg, "runstead"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(xdg, "runstead", "siwc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.CreateTask(ctx, state.TaskRecord{TaskID: taskID, Objective: "synthetic SIWC task", Workspace: t.TempDir(), Model: "gpt-test", ConfigJSON: []byte(`{"provider_id":"siwc-cli-test"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if withPendingApproval {
		actionID, err := store.RecordAction(ctx, state.ActionRecord{TaskID: taskID, Tool: "write_file", Arguments: []byte(`{}`), Fingerprint: "synthetic-write"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordWritePolicyDecision(ctx, state.WritePolicyDecision{TaskID: taskID, ActionID: actionID, Tool: "write_file", Decision: "approval_required", Reason: "approval_required"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	identity := cliSIWCIdentity()
	if len(registeredIdentity) > 0 {
		identity = registeredIdentity[0]
	}
	manifest := siwcstate.Manifest{
		Version: siwcstate.ManifestVersion, WireContract: identity.WireContract,
		AccountBinding: identity.AccountBinding, CredentialBinding: identity.CredentialBinding,
		BehaviorDigest: identity.BehaviorDigest, ProviderID: identity.ProviderID,
		Model: identity.Model, ConfigIdentity: identity.ConfigIdentity,
	}
	locator := siwcstate.Locator{Version: siwcstate.LocatorVersion, CanonicalDir: stateDir, DomainID: identity.ConfigIdentity}
	for path, value := range map[string]any{
		filepath.Join(xdg, "runstead", "siwc", siwcstate.LocatorFile): locator,
		filepath.Join(stateDir, siwcstate.ManifestFile):               manifest,
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return identity
}

func setSIWCEnv(t *testing.T, home, xdg string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("RUNSTEAD_STATE_DIR", "")
	if err := os.Unsetenv("RUNSTEAD_STATE_DIR"); err != nil {
		t.Fatal(err)
	}
}

func writeSIWCProviderFixture(t *testing.T, path string) {
	t.Helper()
	document := `{
  "version": 2,
  "providers": [{
    "provider_id": "siwc-cli-test",
    "protocol_family": "openai_compatible",
    "base_url": "https://api.openai.com/v1",
    "model": "gpt-test",
    "auth_ref": "SIWC_CONNECTION",
    "auth_requirement": "reference_required",
    "config_version": "v1",
    "wire_contract": "responses_siwc_v1",
    "account_binding": "hmac-sha256:v1:` + strings.Repeat("a", 64) + `",
    "credential_binding": "hmac-sha256:v1:` + strings.Repeat("b", 64) + `",
    "profile": {
      "profile_version": "v1",
      "capabilities": ["text_turn", "runstead_protocol"],
      "route_safety": {
        "attempt_accounting": "single",
        "single_attempt": "guaranteed",
        "internal_retries": "disabled",
        "cooldown_replay": "disabled",
        "account_pooling": "disabled",
        "automatic_fallback": "disabled",
        "combo_routing": "disabled"
      }
    }
  }]
}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadSIWCFixtureIdentity(t *testing.T, path string) provider.Identity {
	t.Helper()
	registry, err := config.LoadProvidersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve("siwc-cli-test", provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	return provider.IdentityFromResolved(*resolved, compat.AdapterVersion)
}

func TestSIWCInspectDecideAndImprovementUseRegisteredDomain(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "registered-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", true)

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"inspect", "siwc-task", "--state-domain", "siwc"}, &out, &errOut); code != exitSuccess {
		t.Fatalf("inspect exit = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Task: siwc-task") {
		t.Fatalf("inspect output did not include seeded SIWC task: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"decide", "siwc-task", "action-000001", "approved", "--state-domain=siwc"}, &out, &errOut); code != exitSuccess {
		t.Fatalf("decide exit = %d, stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "decision=approved") {
		t.Fatalf("decide output = %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"improvement", "list", "--state-domain", "siwc"}, &out, &errOut); code != exitSuccess {
		t.Fatalf("improvement list exit = %d, stderr=%s", code, errOut.String())
	}

	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending, err := store.PendingApprovals(context.Background(), "siwc-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending approvals = %d, want decision persisted in canonical DB", len(pending))
	}
}

func TestSIWCDivergentFlagEnvAndChangedDiscoveryRootFailWithoutCreatingDB(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	canonical := filepath.Join(base, "canonical")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	identity := seedSIWCDomain(t, home, xdg, canonical, "siwc-task", false)

	if _, err := resolveCommandStateDomain("siwc", filepath.Join(base, "alternate"), true, true, &identity); !errors.Is(err, siwcstate.ErrDivergentPath) {
		t.Fatalf("divergent --state-dir error = %v, want ErrDivergentPath", err)
	}
	if _, err := os.Stat(filepath.Join(base, "alternate")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("divergent --state-dir created a path: %v", err)
	}
	if _, err := resolveCommandStateDomain("siwc", canonical, true, true, &identity); err != nil {
		t.Fatalf("canonical --state-dir override should be accepted: %v", err)
	}

	t.Setenv("RUNSTEAD_STATE_DIR", filepath.Join(base, "env-alternate"))
	if _, err := resolveCommandStateDomain("siwc", "", false, false, &identity); !errors.Is(err, siwcstate.ErrDivergentPath) {
		t.Fatalf("divergent RUNSTEAD_STATE_DIR error = %v, want ErrDivergentPath", err)
	}
	if _, err := os.Stat(filepath.Join(base, "env-alternate")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("divergent RUNSTEAD_STATE_DIR created a path: %v", err)
	}

	changedHome := filepath.Join(base, "changed-home")
	changedXDG := filepath.Join(base, "changed-xdg")
	if err := os.MkdirAll(changedHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", changedHome)
	t.Setenv("XDG_STATE_HOME", changedXDG)
	t.Setenv("RUNSTEAD_STATE_DIR", "")
	if err := os.Unsetenv("RUNSTEAD_STATE_DIR"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCommandStateDomain("siwc", "", false, false, nil); !errors.Is(err, siwcstate.ErrDomainUnavailable) {
		t.Fatalf("changed discovery root error = %v, want ErrDomainUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(changedXDG, "runstead", "siwc", "runstead.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed discovery root created a replacement DB: %v", err)
	}
}

func TestSIWCResumeRemainsNonOperationalBeforeOpeningStore(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "registered-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	identity := loadSIWCFixtureIdentity(t, providers)
	seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", false, identity)
	before, err := os.ReadFile(filepath.Join(stateDir, state.DefaultDBFile))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"resume", "siwc-task", "--state-domain", "siwc", "--providers", providers, "--provider-id", "siwc-cli-test"}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), "recovery barrier") {
		t.Fatalf("resume = (%d, %q), want unavailable at the lock/recovery gate", code, errOut.String())
	}
	after, err := os.ReadFile(filepath.Join(stateDir, state.DefaultDBFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("non-operational SIWC resume changed the database")
	}
}

func TestSIWCResumeRequiresDomainBeforeOpeningLegacyPath(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, filepath.Join(base, "xdg"))
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	alternate := filepath.Join(base, "alternate-state")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"resume", "siwc-task", "--providers", providers, "--provider-id", "siwc-cli-test", "--state-dir", alternate,
	}, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "requires --state-domain siwc") {
		t.Fatalf("resume exit=%d stderr=%q; wanted required domain selector", code, errOut.String())
	}
	if _, err := os.Stat(alternate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume created an alternate legacy database path: %v", err)
	}
}

func TestSIWCRunRequiresDomainBeforeOpeningLegacyPath(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, filepath.Join(base, "xdg"))
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	alternate := filepath.Join(base, "alternate-state")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "synthetic", "--workspace", base, "--state-dir", alternate,
		"--providers", providers, "--provider-id", "siwc-cli-test", "--max-steps", "1",
	}, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "requires --state-domain siwc") {
		t.Fatalf("run exit=%d stderr=%q; wanted required domain selector", code, errOut.String())
	}
	if _, err := os.Stat(alternate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run created an alternate state database path: %v", err)
	}
}

func TestSIWCRunValidatesRegisteredDomainThenRefusesBeforeSQLiteOpen(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "registered-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	identity := loadSIWCFixtureIdentity(t, providers)
	seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", false, identity)
	dbPath := filepath.Join(stateDir, state.DefaultDBFile)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "synthetic", "--workspace", base, "--state-domain", "siwc",
		"--state-dir", stateDir, "--providers", providers, "--provider-id", "siwc-cli-test", "--max-steps", "1",
	}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), "recovery barrier") {
		t.Fatalf("run = (%d, %q), want unavailable at the lock/recovery gate", code, errOut.String())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("non-operational SIWC run changed the database")
	}
}

func TestLegacyStateDirectoryResolutionRemainsUnchanged(t *testing.T) {
	want := filepath.Join(t.TempDir(), "legacy")
	location, err := resolveCommandStateDomain("", want, true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if location.Dir != want || location.SIWC != nil {
		t.Fatalf("legacy location = %+v, want path %q without SIWC domain", location, want)
	}
}

func TestLegacyExplicitEmptyStateDirSemanticsRemainCommandSpecific(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "env-state")
	t.Setenv("RUNSTEAD_STATE_DIR", envDir)
	if _, err := resolveCommandStateDomain("", "", true, true, nil); err == nil {
		t.Fatal("run's explicit empty --state-dir must continue to fail")
	}
	location, err := resolveCommandStateDomain("", "", true, false, nil)
	if err != nil {
		t.Fatalf("legacy inspect-style empty --state-dir should retain env fallback: %v", err)
	}
	if location.Dir != envDir {
		t.Fatalf("legacy fallback directory = %q, want %q", location.Dir, envDir)
	}
}
