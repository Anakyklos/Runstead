package main

// Issue #91 review regressions: configured bounds recorded at run/resume
// time must never undo an observed tightening persisted on the same
// unchanged identity, and the property must survive reruns, resume and a
// real reopen of the SQLite file (monotonic durable boundary).

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/config"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/state"
)

const profileTestBound = 8000

// profileTighteningIdentity resolves the pinned identity of the providers
// file so the test can apply observations through the same key the runtime
// uses.
func profileTighteningIdentity(t *testing.T, providersFile, providerID string) provider.Identity {
	t.Helper()
	registry, err := config.LoadProvidersFile(providersFile)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.Resolve(providerID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		t.Fatal(err)
	}
	return provider.IdentityFromResolved(*resolved, compat.AdapterVersion)
}

// applyObservedTightening writes the restrictive observation directly
// through the durable monotonic boundary (the legitimate runtime API).
func applyObservedTightening(t *testing.T, stateDir string, identity provider.Identity, value int64, evidence string) {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ApplyOperationalProfileUpdates(context.Background(), identity, nil, []provider.ProfileUpdate{{
		Field: provider.FieldMaxRequestBytes, Value: value, Provenance: provider.ProvenanceObserved, EvidenceRef: evidence,
	}}); err != nil {
		t.Fatal(err)
	}
}

func loadTightening(t *testing.T, stateDir string, identity provider.Identity) provider.ProfileValue {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile, err := store.LoadOperationalProfile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil {
		return provider.ProfileValue{}
	}
	return profile.Effective(provider.FieldMaxRequestBytes)
}

func monotonicRunArgs(t *testing.T, workspace, stateDir, acceptance, providersFile, providerID string) []string {
	t.Helper()
	return []string{
		"run", "--task", "inspect the workspace and report",
		"--workspace", workspace,
		"--providers", providersFile,
		"--provider-id", providerID,
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
}

// TestRunRerunRestartKeepObservedTightening runs the full scenario required
// by the review: persist configured=8000, tighten to observed=2048, run the
// SAME configuration again, and confirm the field stays 2048/observed after
// the rerun and after a real SQLite reopen.
func TestRunRerunRestartKeepObservedTightening(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	miniScript := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"inspected","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	}
	// The SAME endpoint serves both runs; repeat the deterministic script so
	// each governed admission receives its own envelope (counted 1:1).
	responses := append(append([]string{}, miniScript...), miniScript...)
	wire := newE2EWire(provider.FamilyOpenAICompatible, responses...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "mono-provider", server.URL+"/v1", profileTestBound)
	identity := profileTighteningIdentity(t, providersFile, "mono-provider")
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	// First run: configured=8000 persisted.
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	if code := run(context.Background(), monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "mono-provider"), &out, &errOut); code != exitSuccess {
		t.Fatalf("run#1 exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if v := loadTightening(t, stateDir, identity); v.Value != profileTestBound || v.Provenance != provider.ProvenanceConfigured {
		t.Fatalf("run#1 must persist configured=%d, got %+v", profileTestBound, v)
	}

	// Tighten through the durable boundary to observed=2048.
	applyObservedTightening(t, stateDir, identity, 2048, "obs-000001")

	// Second run with the SAME configuration: the replayed configured bound
	// must NOT undo the observed tightening.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "mono-provider"), &out, &errOut); code != exitSuccess {
		t.Fatalf("run#2 exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if v := loadTightening(t, stateDir, identity); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("rerun undid the observed tightening: %+v", v)
	}

	// Restart (reopen) does not alter the property.
	if v := loadTightening(t, stateDir, identity); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("restart lost the monotonic property: %+v", v)
	}
}

// TestResumeKeepsObservedTightening crashes a run after the first governed
// provider attempt, tightens the durable profile to observed=2048, then
// resumes with the SAME configuration: the resumed configured replay must
// leave 2048/observed intact.
func TestResumeKeepsObservedTightening(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Request 1 answers the crashed run, requests 2-3 the resume script.
	responses := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"inspected","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	}
	wire := newE2EWire(provider.FamilyOpenAICompatible, responses...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "mono-provider", server.URL+"/v1", profileTestBound)
	identity := profileTighteningIdentity(t, providersFile, "mono-provider")
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	stateDir := t.TempDir()
	args := monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "mono-provider")
	command := exec.Command(os.Args[0], "-test.run=TestRuntimeCodingLoopCrashHelper")
	command.Env = append(os.Environ(),
		"RUNSTEAD_CODING_CRASH_HELPER=1",
		"RUNSTEAD_CODING_CRASH_POINT=provider_tx1_after",
		"RUNSTEAD_CODING_CRASH_AFTER=1",
		"RUNSTEAD_CODING_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := command.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 42 {
		t.Fatalf("crash helper exit = %v\n%s", err, output)
	}
	taskID := taskIDFromOutput(t, string(output))
	if v := loadTightening(t, stateDir, identity); v.Value != profileTestBound || v.Provenance != provider.ProvenanceConfigured {
		t.Fatalf("crashed run must have persisted configured=%d, got %+v", profileTestBound, v)
	}

	// Tighten the durable profile while the task is interrupted.
	applyObservedTightening(t, stateDir, identity, 2048, "obs-000001")

	// Resume through the SAME configuration.
	var out, errOut strings.Builder
	resumeArgs := []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--providers", providersFile,
		"--provider-id", "mono-provider",
		"--acceptance", acceptance,
		"--log-level", "error",
	}
	if code := run(context.Background(), resumeArgs, &out, &errOut); code != exitSuccess {
		t.Fatalf("resume exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "outcome: completed") {
		t.Fatalf("resume must complete:\n%s", out.String())
	}
	if v := loadTightening(t, stateDir, identity); v.Value != 2048 || v.Provenance != provider.ProvenanceObserved {
		t.Fatalf("resume undid the observed tightening: %+v", v)
	}
}

// newHTTPServerForWire wraps an e2eWire in an httptest server.
func newHTTPServerForWire(wire *e2eWire) *httptest.Server {
	return httptest.NewServer(wire.handler())
}

// writeProvidersFileWithBounds writes a single-provider declarations file
// whose capability profile exposes max_request_bytes.
func writeProvidersFileWithBounds(t *testing.T, family provider.ProtocolFamily, providerID, baseURL string, maxRequestBytes int) string {
	t.Helper()
	var optionsJSON string
	if family == provider.FamilyAnthropicCompatible {
		optionsJSON = `{"max_tokens":"256","anthropic_version":"2023-06-01"}`
	} else {
		optionsJSON = "null"
	}
	routeSafety := `{"attempt_accounting":"single","single_attempt":"guaranteed","internal_retries":"disabled","cooldown_replay":"disabled","account_pooling":"disabled","automatic_fallback":"disabled","combo_routing":"disabled"}`
	document := `{
		"version": 1,
		"providers": [
			{
				"provider_id": "` + providerID + `",
				"protocol_family": "` + string(family) + `",
				"base_url": "` + baseURL + `",
				"model": "e2e-model",
				"auth_requirement": "none",
				"options": ` + optionsJSON + `,
				"config_version": "v1",
				"profile": {
					"profile_version": "v1",
					"capabilities": ["text_turn", "runstead_protocol"],
					"route_safety": ` + routeSafety + `,
					"max_request_bytes": ` + fmt.Sprintf("%d", maxRequestBytes) + `
				}
			}
		]
	}`
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
