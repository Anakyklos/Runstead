package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/governor"
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

func TestSIWCDomainWriterLockHelper(t *testing.T) {
	mode := os.Getenv("RUNSTEAD_SIWC_WRITER_HELPER")
	if mode == "" {
		return
	}
	var out, errOut bytes.Buffer
	var args []string
	switch mode {
	case "decide":
		args = []string{"decide", "siwc-task", "action-000001", "approved", "--state-domain", "siwc"}
	case "improvement":
		args = []string{"improvement", "review", "proposal-1", "--decision", "approved", "--state-domain", "siwc"}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	if code := run(context.Background(), args, &out, &errOut); code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC domain unavailable") {
		t.Fatalf("%s writer result = (%d, %q), want bounded lock refusal", mode, code, errOut.String())
	}
}

func TestSIWCPhysicalAliasProcessHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_SIWC_ALIAS_HELPER") != "1" {
		return
	}
	location, err := resolveCommandStateDomain("siwc", true, "", false, false, nil)
	if err != nil {
		_, _ = os.Stdout.WriteString("REFUSED=" + stateDomainDiagnostic(err, true))
		return
	}
	lock, err := acquireSIWCDomainLock(context.Background(), location)
	if err != nil {
		_, _ = os.Stdout.WriteString("REFUSED=" + err.Error())
		return
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release SIWC helper lock: %v", err)
		}
	}()
	if err := os.WriteFile(os.Getenv("RUNSTEAD_SIWC_ALIAS_ENTERED"), []byte("entered"), 0o600); err != nil {
		t.Fatalf("write entered marker: %v", err)
	}
	_, _ = os.Stdout.WriteString("ENTERED")
	select {}
}

func TestSIWCRecoveryProcessHelper(t *testing.T) {
	switch os.Getenv("RUNSTEAD_SIWC_RECOVERY_HELPER") {
	case "seed-prepared":
		domain := siwcstate.Domain{
			Dir:      os.Getenv("RUNSTEAD_SIWC_RECOVERY_STATE"),
			Identity: siwcstate.Manifest{ConfigIdentity: os.Getenv("RUNSTEAD_SIWC_RECOVERY_IDENTITY")},
		}
		lock, err := siwcstate.AcquireLock(context.Background(), domain)
		if err != nil {
			t.Fatalf("acquire setup lock: %v", err)
		}
		defer func() {
			if err := lock.Release(); err != nil {
				t.Errorf("release crash-helper SIWC lock: %v", err)
			}
		}()
		store, err := state.Open(state.Options{Path: filepath.Join(domain.Dir, state.DefaultDBFile)})
		if err != nil {
			t.Fatalf("open SIWC store: %v", err)
		}
		defer func() {
			if err := store.Close(); err != nil {
				t.Errorf("close crash-helper SIWC store: %v", err)
			}
		}()
		now := time.Now().UTC()
		persisted := governor.PersistedState{
			AccountPolicyID: "runstead-cli", ProviderID: "siwc-cli-test", ModelPool: "instant", Model: "gpt-test",
			AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
			Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
			RollingEvents: []governor.LedgerEvent{{At: now, TaskID: "siwc-task"}},
			TaskStates:    []governor.TaskStateRecord{{TaskID: "siwc-task", Attempts: 1, LastTouched: now}},
		}
		identity := cliSIWCIdentity()
		identity.ConfigIdentity = domain.Identity.ConfigIdentity
		if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
			TaskID: "siwc-task", ClientRequestID: "siwc-request-1", ProviderID: identity.ProviderID,
			ModelPool: "instant", Model: identity.Model, ProtocolFamily: identity.ProtocolFamily,
			ConfigIdentity: identity.ConfigIdentity, AttemptSequence: 1, StartedAt: now, State: persisted,
		}); err != nil {
			t.Fatalf("persist prepared provider attempt: %v", err)
		}
		if err := os.WriteFile(os.Getenv("RUNSTEAD_SIWC_RECOVERY_READY"), []byte("prepared"), 0o600); err != nil {
			t.Fatalf("write prepared marker: %v", err)
		}
		select {}
	default:
		return
	}
}

func seedSIWCDomain(t *testing.T, home, xdg, stateDir, taskID string, withPendingApproval bool, registeredIdentity ...provider.Identity) provider.Identity {
	return seedSIWCDomainWithBinding(t, home, xdg, stateDir, taskID, withPendingApproval, true, registeredIdentity...)
}

func seedSIWCDomainWithBinding(t *testing.T, home, xdg, stateDir, taskID string, withPendingApproval, bindDatabase bool, registeredIdentity ...provider.Identity) provider.Identity {
	t.Helper()
	identity := cliSIWCIdentity()
	if len(registeredIdentity) > 0 {
		identity = registeredIdentity[0]
	}
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
	if bindDatabase {
		if _, err := store.DB().Exec("INSERT INTO meta (key, value) VALUES (?, ?)", siwcstate.DomainMarkerKey, identity.ConfigIdentity); err != nil {
			t.Fatal(err)
		}
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

func TestSIWCDecideAndImprovementWritersHonorCrossProcessLock(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "registered-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", true)
	location, err := resolveCommandStateDomain("siwc", true, "", false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := siwcstate.AcquireLock(context.Background(), *location.SIWC)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	for _, mode := range []string{"decide", "improvement"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSIWCDomainWriterLockHelper$")
		cmd.Env = append(os.Environ(), "RUNSTEAD_SIWC_WRITER_HELPER="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s subprocess: %v: %s", mode, err, output)
		}
	}
}

func TestImprovementStateDomainDiagnosticsRemainDistinct(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)

	t.Run("missing domain", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"improvement", "list", "--state-domain", "siwc"}, &out, &errOut)
		if code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC state domain unavailable") {
			t.Fatalf("improvement missing-domain result = (%d, %q)", code, errOut.String())
		}
	})

	canonical := filepath.Join(base, "canonical")
	seedSIWCDomain(t, home, xdg, canonical, "siwc-task", false)
	t.Run("divergent override", func(t *testing.T) {
		alternate := filepath.Join(base, "alternate")
		t.Setenv("RUNSTEAD_STATE_DIR", alternate)
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"improvement", "list", "--state-domain=siwc"}, &out, &errOut)
		if code != exitUnavailable || !strings.Contains(errOut.String(), "override diverges") {
			t.Fatalf("improvement divergent-domain result = (%d, %q)", code, errOut.String())
		}
		if _, err := os.Stat(alternate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("divergent improvement path was created: %v", err)
		}
	})

	t.Run("unsafe registered path", func(t *testing.T) {
		t.Setenv("RUNSTEAD_STATE_DIR", "")
		if err := os.Unsetenv("RUNSTEAD_STATE_DIR"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(canonical, 0o755); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"improvement", "list", "--state-domain=siwc"}, &out, &errOut)
		if code != exitUnavailable || !strings.Contains(errOut.String(), "unsafe SIWC state path") {
			t.Fatalf("improvement unsafe-domain result = (%d, %q)", code, errOut.String())
		}
	})
}

func TestSIWCManifestCannotClaimLegacyDatabaseForApprovalOrImprovement(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "legacy-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCDomainWithBinding(t, home, xdg, stateDir, "legacy-task", true, false)
	dbPath := filepath.Join(stateDir, state.DefaultDBFile)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"decide", "legacy-task", "action-000001", "approved", "--state-domain", "siwc"}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC state domain unavailable") {
		t.Fatalf("decide against legacy DB = (%d, %q), want fail-closed domain rejection", code, errOut.String())
	}

	change := filepath.Join(base, "change.json")
	if err := os.WriteFile(change, []byte(`{"version":1,"profile":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{
		"improvement", "propose", "--state-domain", "siwc", "--kind", "composition",
		"--scope", "workspace", "--title", "synthetic", "--target", "composition",
		"--base", "v1", "--change", change, "--rationale", "synthetic",
		"--expected-benefit", "synthetic", "--validation-plan", "go-test",
	}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC state domain unavailable") {
		t.Fatalf("improvement against legacy DB = (%d, %q), want fail-closed domain rejection", code, errOut.String())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy database changed after rejected actions: err=%v", err)
	}
	store, err := state.Open(state.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending, err := store.PendingApprovals(context.Background(), "legacy-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("legacy pending approvals = %d, want unchanged 1", len(pending))
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

	if _, err := resolveCommandStateDomain("siwc", true, filepath.Join(base, "alternate"), true, true, &identity); !errors.Is(err, siwcstate.ErrDivergentPath) {
		t.Fatalf("divergent --state-dir error = %v, want ErrDivergentPath", err)
	}
	if _, err := os.Stat(filepath.Join(base, "alternate")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("divergent --state-dir created a path: %v", err)
	}
	if _, err := resolveCommandStateDomain("siwc", true, canonical, true, true, &identity); err != nil {
		t.Fatalf("canonical --state-dir override should be accepted: %v", err)
	}

	t.Setenv("RUNSTEAD_STATE_DIR", filepath.Join(base, "env-alternate"))
	if _, err := resolveCommandStateDomain("siwc", true, "", false, false, &identity); !errors.Is(err, siwcstate.ErrDivergentPath) {
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
	if _, err := resolveCommandStateDomain("siwc", true, "", false, false, nil); !errors.Is(err, siwcstate.ErrDomainUnavailable) {
		t.Fatalf("changed discovery root error = %v, want ErrDomainUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(changedXDG, "runstead", "siwc", "runstead.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed discovery root created a replacement DB: %v", err)
	}
}

func TestSIWCResumeLocksDomainBeforeProviderIdentityRefusal(t *testing.T) {
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
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"resume", "siwc-task", "--state-domain", "siwc", "--providers", providers, "--provider-id", "siwc-cli-test"}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), `provider divergence: task "siwc-task" ran with model "" but "gpt-test" was selected`) {
		t.Fatalf("resume = (%d, %q), want exact provider-model identity refusal", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, ".siwc-domain-lock-v1")); err != nil {
		t.Fatalf("resume did not establish the persistent domain lock marker: %v", err)
	}
}

func TestSIWCDatabaseHardlinkAliasesRefuseConcurrentDomainEntry(t *testing.T) {
	base := t.TempDir()
	homeA, xdgA := filepath.Join(base, "home-a"), filepath.Join(base, "xdg-a")
	homeB, xdgB := filepath.Join(base, "home-b"), filepath.Join(base, "xdg-b")
	stateA, stateB := filepath.Join(base, "state-a"), filepath.Join(base, "state-b")
	for _, home := range []string{homeA, homeB} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedSIWCDomain(t, homeA, xdgA, stateA, "task-a", false)
	seedSIWCDomain(t, homeB, xdgB, stateB, "task-b", false)
	if err := os.Remove(filepath.Join(stateB, state.DefaultDBFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stateA, state.DefaultDBFile), filepath.Join(stateB, state.DefaultDBFile)); err != nil {
		t.Fatalf("create shared SQLite inode: %v", err)
	}

	enteredA := filepath.Join(base, "entered-a")
	enteredB := filepath.Join(base, "entered-b")
	start := func(ctx context.Context, home, xdg, entered string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIWCPhysicalAliasProcessHelper$")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xdg,
			"RUNSTEAD_SIWC_ALIAS_HELPER=1", "RUNSTEAD_SIWC_ALIAS_ENTERED="+entered)
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, second := start(ctx, homeA, xdgA, enteredA), start(ctx, homeB, xdgB, enteredB)
	type result struct {
		name   string
		output []byte
		err    error
	}
	results := make(chan result, 2)
	go func() {
		output, err := first.CombinedOutput()
		results <- result{name: "domain A", output: output, err: err}
	}()
	go func() {
		output, err := second.CombinedOutput()
		results <- result{name: "domain B", output: output, err: err}
	}()
	outputs := make(map[string]string, 2)
	var firstErr, secondErr error
	for range 2 {
		result := <-results
		outputs[result.name] = string(result.output)
		if result.name == "domain A" {
			firstErr = result.err
		} else {
			secondErr = result.err
		}
	}
	firstOutput, secondOutput := []byte(outputs["domain A"]), []byte(outputs["domain B"])
	if firstErr != nil || secondErr != nil {
		t.Fatalf("alias subprocesses failed: first=(%v,%q) second=(%v,%q)", firstErr, firstOutput, secondErr, secondOutput)
	}
	for name, output := range outputs {
		if !strings.Contains(output, "REFUSED=unsafe SIWC state path") {
			t.Errorf("%s output = %q, want preflight refusal", name, output)
		}
	}
	for _, marker := range []string{enteredA, enteredB} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("aliased domain entered its critical section (%s): %v", marker, err)
		}
	}
}

func TestSIWCPostLockRevalidationRejectsNewDatabaseHardlink(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	identity := seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", false)
	location, err := resolveCommandStateDomain("siwc", true, "", false, false, &identity)
	if err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(base, "other-domain")
	if err := os.Mkdir(aliasDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stateDir, state.DefaultDBFile), filepath.Join(aliasDir, state.DefaultDBFile)); err != nil {
		t.Fatalf("create database hardlink after initial preflight: %v", err)
	}
	lock, err := acquireSIWCDomainLock(context.Background(), location)
	if lock != nil {
		if releaseErr := lock.Release(); releaseErr != nil {
			t.Errorf("release unexpectedly returned post-lock SIWC lock: %v", releaseErr)
		}
	}
	if !errors.Is(err, siwcstate.ErrDomainUnavailable) {
		t.Fatalf("post-lock domain revalidation = %v, want hardlink alias rejection", err)
	}
}

func TestSIWCPreparedAttemptRecoveryAcrossProcessCrash(t *testing.T) {
	base := t.TempDir()
	cliBinary := filepath.Join(base, "runstead")
	buildCLI := exec.Command("go", "build", "-o", cliBinary, ".")
	buildDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	buildCLI.Dir = buildDir
	if output, err := buildCLI.CombinedOutput(); err != nil {
		t.Fatalf("build real Runstead CLI for recovery E2E: %v: %s", err, output)
	}
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	stateDir := filepath.Join(base, "registered-state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	identity := loadSIWCFixtureIdentity(t, providers)
	setSIWCEnv(t, home, xdg)
	seedSIWCDomain(t, home, xdg, stateDir, "siwc-task", false, identity)

	configJSON, err := json.Marshal(map[string]string{
		"provider_id": identity.ProviderID, "protocol_family": string(identity.ProtocolFamily),
		"provider_model": identity.Model, "provider_config_identity": identity.ConfigIdentity,
		"provider_profile_version": identity.ProfileVersion, "provider_adapter_version": identity.AdapterVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE tasks SET config_json = ? WHERE task_id = ?`, string(configJSON), "siwc-task"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(base, "prepared")
	crasher := exec.Command(os.Args[0], "-test.run=^TestSIWCRecoveryProcessHelper$")
	crasher.Env = append(os.Environ(), "RUNSTEAD_SIWC_RECOVERY_HELPER=seed-prepared",
		"RUNSTEAD_SIWC_RECOVERY_STATE="+stateDir, "RUNSTEAD_SIWC_RECOVERY_IDENTITY="+identity.ConfigIdentity,
		"RUNSTEAD_SIWC_RECOVERY_READY="+ready)
	if err := crasher.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = crasher.Process.Kill()
			_, _ = crasher.Process.Wait()
			t.Fatal("writer process did not durably prepare a provider attempt")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := crasher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = crasher.Process.Wait()
	assertSIWCPreparedAfterCrash(t, home, xdg, stateDir, identity)

	resume := func() (int, string) {
		cmd := exec.Command(cliBinary, "resume", "siwc-task", "--state-domain", "siwc",
			"--providers", providers, "--provider-id", "siwc-cli-test")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xdg)
		output, err := cmd.CombinedOutput()
		if err == nil {
			return 0, string(output)
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(output)
		}
		t.Fatalf("resume process failed before CLI exit: %v: %s", err, output)
		return -1, string(output)
	}
	for process := 1; process <= 2; process++ {
		code, output := resume()
		if code != exitUnavailable || !strings.Contains(output, "durable recovery completed; SIWC inference remains unavailable") {
			t.Fatalf("resume process %d = (%d,%q), want recovered offline refusal", process, code, output)
		}
		assertSIWCRecoveredAccounting(t, home, xdg, stateDir, identity, process)
	}
}

func assertSIWCPreparedAfterCrash(t *testing.T, home, xdg, stateDir string, identity provider.Identity) {
	t.Helper()
	setSIWCEnv(t, home, xdg)
	location, err := resolveCommandStateDomain("siwc", true, "", false, false, &identity)
	if err != nil {
		t.Fatalf("pre-resume SIWC domain preflight: %v", err)
	}
	lock, err := acquireSIWCDomainLock(context.Background(), location)
	if err != nil {
		t.Fatalf("pre-resume SIWC lock: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release SIWC observation lock: %v", err)
		}
	}()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SIWC observation store: %v", err)
		}
	}()
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM provider_attempts WHERE task_id = ?`, "siwc-task").Scan(&status); err != nil {
		t.Fatal(err)
	}
	var ledgerRows, taskAttempts int
	if err := store.DB().QueryRow(`SELECT count(*) FROM governor_ledger`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT attempts FROM governor_task_states WHERE task_id = ?`, "siwc-task").Scan(&taskAttempts); err != nil {
		t.Fatal(err)
	}
	if status != "prepared" || ledgerRows != 1 || taskAttempts != 1 {
		t.Fatalf("after killed writer: attempt=%q ledger=%d task attempts=%d; want durable prepared row and one debit", status, ledgerRows, taskAttempts)
	}
}

func assertSIWCRecoveredAccounting(t *testing.T, home, xdg, stateDir string, identity provider.Identity, resumes int) {
	t.Helper()
	setSIWCEnv(t, home, xdg)
	location, err := resolveCommandStateDomain("siwc", true, "", false, false, &identity)
	if err != nil {
		t.Fatalf("post-resume SIWC domain preflight: %v", err)
	}
	lock, err := acquireSIWCDomainLock(context.Background(), location)
	if err != nil {
		t.Fatalf("post-resume SIWC lock: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("release post-resume SIWC lock: %v", err)
		}
	}()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close post-resume SIWC store: %v", err)
		}
	}()
	var status, recoveryReason string
	var uncertain, debited int
	if err := store.DB().QueryRow(`SELECT status, uncertain, attempt_debited, recovery_reason FROM provider_attempts WHERE task_id = ?`, "siwc-task").Scan(&status, &uncertain, &debited, &recoveryReason); err != nil {
		t.Fatal(err)
	}
	if status != "reconciled" || uncertain != 1 || debited != 1 || recoveryReason != "upstream_may_have_been_reached" {
		t.Fatalf("durable attempt=(%q,%d,%d,%q), want reconciled uncertain debit", status, uncertain, debited, recoveryReason)
	}
	var attempts, ledgerRows, taskAttempts, taskRetries, nextAttempt, singletonCount int
	var policyID, providerID, modelPool, model string
	if err := store.DB().QueryRow(`SELECT count(*) FROM provider_attempts WHERE task_id = ?`, "siwc-task").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM governor_ledger`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT attempts, retries FROM governor_task_states WHERE task_id = ?`, "siwc-task").Scan(&taskAttempts, &taskRetries); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*), account_policy_id, provider_id, model_pool, model, next_attempt FROM governor_state WHERE id = 1`).Scan(
		&singletonCount, &policyID, &providerID, &modelPool, &model, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	var resumeCount int
	if err := store.DB().QueryRow(`SELECT resume_count FROM tasks WHERE task_id = ?`, "siwc-task").Scan(&resumeCount); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || ledgerRows != 1 || taskAttempts != 1 || taskRetries != 0 || singletonCount != 1 ||
		policyID != "runstead-cli" || providerID != "siwc-cli-test" || modelPool != "instant" || model != "gpt-test" ||
		nextAttempt != 2 || resumeCount != resumes {
		t.Fatalf("durable accounting attempts=%d ledger=%d task=(%d,%d) singleton=%d policy=%q provider=%q pool=%q model=%q next=%d resumes=%d; expected one original debit and %d recovery passes", attempts, ledgerRows, taskAttempts, taskRetries, singletonCount, policyID, providerID, modelPool, model, nextAttempt, resumeCount, resumes)
	}
	var preparedEvents, reconciledEvents int
	if err := store.DB().QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'provider_attempt_prepared'`, "siwc-task").Scan(&preparedEvents); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'provider_attempt_reconciled'`, "siwc-task").Scan(&reconciledEvents); err != nil {
		t.Fatal(err)
	}
	if preparedEvents != 1 || reconciledEvents != 1 {
		t.Fatalf("durable provider journal prepared=%d reconciled=%d; want exactly one of each", preparedEvents, reconciledEvents)
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

func TestSIWCRunValidatesRegisteredDomainThenRefusesUnsupportedAdapter(t *testing.T) {
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
	if code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC Responses wire contract is not implemented; refusing dispatch") {
		t.Fatalf("run = (%d, %q), want explicit offline adapter refusal", code, errOut.String())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("adapter refusal unexpectedly changed the SIWC database")
	}
	if _, err := os.Stat(filepath.Join(stateDir, ".siwc-domain-lock-v1")); err != nil {
		t.Fatalf("run did not establish the persistent domain lock marker: %v", err)
	}
}

func TestSIWCRunBlocksFreshAdmissionForPreparedAttempt(t *testing.T) {
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
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	persisted := governor.PersistedState{
		AccountPolicyID: "runstead-cli", ProviderID: identity.ProviderID, ModelPool: "instant", Model: identity.Model,
		AllowanceProfile: governor.ProfileInstant, NextAttempt: 2,
		Circuit:    governor.CircuitSnapshot{State: governor.CircuitClosed},
		Ceilings:   governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
		TaskStates: []governor.TaskStateRecord{{TaskID: "siwc-task", Attempts: 1, LastTouched: now}},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "siwc-task", ClientRequestID: "siwc-task-0001", ProviderID: identity.ProviderID,
		ModelPool: "instant", Model: identity.Model, ProtocolFamily: identity.ProtocolFamily,
		ConfigIdentity: identity.ConfigIdentity, AllowanceProfile: governor.ProfileInstant,
		AttemptSequence: 1, StartedAt: now, State: persisted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{
		"run", "--task", "synthetic", "--workspace", base, "--state-domain", "siwc",
		"--providers", providers, "--provider-id", "siwc-cli-test", "--max-steps", "1",
	}, &out, &errOut)
	if code != exitUnavailable || !strings.Contains(errOut.String(), "SIWC admission blocked by unresolved durable attempt") {
		t.Fatalf("run result = (%d, %q), want durable admission barrier", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "refusing dispatch") {
		t.Fatalf("run reached adapter refusal before checking durable admission: %q", errOut.String())
	}
}

func TestExplicitEmptyStateDomainFailsClosedAcrossCommands(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, filepath.Join(base, "xdg"))
	commands := []struct {
		args     []string
		stateDir string
	}{
		{args: []string{"inspect", "task", "--state-domain=", "--state-dir", filepath.Join(base, "inspect-state-inline")}, stateDir: filepath.Join(base, "inspect-state-inline")},
		{args: []string{"inspect", "task", "--state-domain", "", "--state-dir", filepath.Join(base, "inspect-state-separated")}, stateDir: filepath.Join(base, "inspect-state-separated")},
		{args: []string{"decide", "task", "action", "approved", "--state-domain=", "--state-dir", filepath.Join(base, "decide-state-inline")}, stateDir: filepath.Join(base, "decide-state-inline")},
		{args: []string{"decide", "task", "action", "approved", "--state-domain", "", "--state-dir", filepath.Join(base, "decide-state-separated")}, stateDir: filepath.Join(base, "decide-state-separated")},
		{args: []string{"improvement", "list", "--state-domain=", "--state-dir", filepath.Join(base, "improvement-state-inline")}, stateDir: filepath.Join(base, "improvement-state-inline")},
		{args: []string{"improvement", "list", "--state-domain", "", "--state-dir", filepath.Join(base, "improvement-state-separated")}, stateDir: filepath.Join(base, "improvement-state-separated")},
		{args: []string{"run", "--task", "task", "--workspace", base, "--state-domain=", "--state-dir", filepath.Join(base, "run-state-inline")}, stateDir: filepath.Join(base, "run-state-inline")},
		{args: []string{"run", "--task", "task", "--workspace", base, "--state-domain", "", "--state-dir", filepath.Join(base, "run-state-separated")}, stateDir: filepath.Join(base, "run-state-separated")},
		{args: []string{"resume", "task", "--state-domain=", "--state-dir", filepath.Join(base, "resume-state-inline")}, stateDir: filepath.Join(base, "resume-state-inline")},
		{args: []string{"resume", "task", "--state-domain", "", "--state-dir", filepath.Join(base, "resume-state-separated")}, stateDir: filepath.Join(base, "resume-state-separated")},
	}
	for _, test := range commands {
		var out, errOut bytes.Buffer
		code := run(context.Background(), test.args, &out, &errOut)
		if code != exitUsage || !strings.Contains(errOut.String(), "state domain") {
			t.Errorf("run(%q) = (%d, %q), want invalid state-domain selector", test.args, code, errOut.String())
		}
		if _, err := os.Stat(test.stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run(%q) created alternate state path: %v", test.args, err)
		}
	}
}

func TestResumeInvalidProviderConfigurationFailsBeforeOpeningAlternateState(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, filepath.Join(base, "xdg"))
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	for _, test := range []struct {
		name       string
		providerID string
		writeFile  func()
	}{
		{name: "load error", providerID: "siwc-cli-test", writeFile: func() {
			if err := os.WriteFile(providers, []byte(`{"version":2,`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "resolve error", providerID: "missing-provider", writeFile: func() { writeSIWCProviderFixture(t, providers) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.writeFile()
			alternate := filepath.Join(base, strings.ReplaceAll(test.name, " ", "-")+"-state")
			var out, errOut bytes.Buffer
			code := run(context.Background(), []string{
				"resume", "task", "--providers", providers, "--provider-id", test.providerID, "--state-dir", alternate,
			}, &out, &errOut)
			if code != exitUsage || !strings.Contains(strings.ToLower(errOut.String()), "provider") {
				t.Fatalf("resume exit=%d stderr=%q; wanted provider configuration error", code, errOut.String())
			}
			if _, err := os.Stat(alternate); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("resume created state after invalid provider configuration: %v", err)
			}
		})
	}
}

func TestLegacyStateDirectoryResolutionRemainsUnchanged(t *testing.T) {
	want := filepath.Join(t.TempDir(), "legacy")
	location, err := resolveCommandStateDomain("", false, want, true, true, nil)
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
	if _, err := resolveCommandStateDomain("", false, "", true, true, nil); err == nil {
		t.Fatal("run's explicit empty --state-dir must continue to fail")
	}
	location, err := resolveCommandStateDomain("", false, "", true, false, nil)
	if err != nil {
		t.Fatalf("legacy inspect-style empty --state-dir should retain env fallback: %v", err)
	}
	if location.Dir != envDir {
		t.Fatalf("legacy fallback directory = %q, want %q", location.Dir, envDir)
	}
}
