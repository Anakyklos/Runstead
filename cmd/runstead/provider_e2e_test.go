package main

// Issue #14 provider-neutral E2E: the SAME authoritative Runstead runtime
// (real governor, real tools, real recipes, real verifier, real SQLite, real
// git) executes the full inspect-edit-test-fix coding loop through configured
// provider endpoints of all three supported protocol families. The only
// synthetic seam is the provider-shaped httptest endpoint: it speaks the
// family wire subset, counts physical model-effect requests and wraps the
// deterministic runstead.protocol.v1 text in the family response envelope.
// Everything after the provider boundary is the real runtime.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// e2eFamily is one E2E protocol family row.
type e2eFamily struct {
	provider.ProtocolFamily
	optionsJSON string // anthropic requires non-secret Messages options
}

var e2eFamilies = []e2eFamily{
	{provider.FamilyOpenAICompatible, "null"},
	{provider.FamilyAnthropicCompatible, `{"max_tokens":"256","anthropic_version":"2023-06-01"}`},
	{provider.FamilyGoogleCompatible, "null"},
}

// e2eWire is a provider-shaped httptest double that counts physical requests
// and answers with deterministic runstead.protocol.v1 text wrapped in the
// family wire subset.
type e2eWire struct {
	family    provider.ProtocolFamily
	responses []string
	mu        sync.Mutex
	requests  int
}

func newE2EWire(family provider.ProtocolFamily, responses ...string) *e2eWire {
	return &e2eWire{family: family, responses: append([]string(nil), responses...)}
}

func (w *e2eWire) handler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		w.mu.Lock()
		w.requests++
		count := w.requests
		w.mu.Unlock()
		raw, _ := io.ReadAll(request.Body)
		index := count - 1
		if index >= len(w.responses) {
			index = len(w.responses) - 1
		}
		text := w.responses[index]
		_ = raw
		body := e2eWrapResponse(w.family, text)
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-Id", fmt.Sprintf("req-e2e-%d", count))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(body)
	})
}

func (w *e2eWire) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

func e2eWrapResponse(family provider.ProtocolFamily, text string) []byte {
	encoded, _ := json.Marshal(text)
	var envelope string
	switch family {
	case provider.FamilyOpenAICompatible:
		envelope = fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}]}`, encoded)
	case provider.FamilyAnthropicCompatible:
		envelope = fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"stop_reason":"end_turn"}`, encoded)
	case provider.FamilyGoogleCompatible:
		envelope = fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":%s}]},"finishReason":"STOP"}]}`, encoded)
	default:
		panic("unknown family " + string(family))
	}
	return []byte(envelope)
}

// writeProvidersFile writes one provider declarations document (the #14
// operator surface) for the given family endpoints.
func writeProvidersFile(t *testing.T, family e2eFamily, endpoints map[string]string) string {
	t.Helper()
	type profile struct {
		ProfileVersion string   `json:"profile_version"`
		Capabilities   []string `json:"capabilities"`
		RouteSafety    any      `json:"route_safety,omitempty"`
	}
	type entry struct {
		ProviderID      string          `json:"provider_id"`
		ProtocolFamily  string          `json:"protocol_family"`
		BaseURL         string          `json:"base_url"`
		Model           string          `json:"model"`
		AuthRequirement string          `json:"auth_requirement"`
		Options         json.RawMessage `json:"options,omitempty"`
		ConfigVersion   string          `json:"config_version"`
		Profile         profile         `json:"profile"`
	}
	var providers []entry
	for providerID, baseURL := range endpoints {
		providers = append(providers, entry{
			ProviderID:      providerID,
			ProtocolFamily:  string(family.ProtocolFamily),
			BaseURL:         baseURL,
			Model:           "e2e-model",
			AuthRequirement: "none",
			Options:         json.RawMessage(family.optionsJSON),
			ConfigVersion:   "v1",
			Profile: profile{
				ProfileVersion: "v1",
				Capabilities:   []string{"text_turn", "runstead_protocol"},
			},
		})
	}
	document, err := json.Marshal(map[string]any{"version": 1, "providers": providers})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codingLoopScript(t *testing.T, initialHash, wrongFix, wrongHash, correctFix string) []string {
	t.Helper()
	return []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc_test.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":` + mustQuote(wrongFix) + `,"expected_before_hash":"` + initialHash + `"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"app/calc.go"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"write_file","arguments":{"path":"app/calc.go","content":` + mustQuote(correctFix) + `,"expected_before_hash":"` + wrongHash + `"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"run_recipe","arguments":{"recipe":"test"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Fixed the calculator.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"run_recipe"},{"evidence_id":"obs-000003","tool":"read_file"},{"evidence_id":"obs-000004","tool":"write_file"},{"evidence_id":"obs-000005","tool":"run_recipe"},{"evidence_id":"obs-000006","tool":"read_file"},{"evidence_id":"obs-000007","tool":"write_file"},{"evidence_id":"obs-000008","tool":"run_recipe"}]}</runstead_final>`,
	}
}

// providerRunArgs builds the real CLI run arguments for the full #12 scenario
// through a configured provider endpoint.
func providerRunArgs(t *testing.T, workspace, stateDir, acceptance, providersFile, providerID string) []string {
	t.Helper()
	return []string{
		"run", "--task", "Fix the calculator so the test suite passes.",
		"--workspace", workspace,
		"--providers", providersFile,
		"--provider-id", providerID,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
}

// providerResumeArgs builds the resume arguments continuing the same task
// through the SAME configured provider endpoint.
func providerResumeArgs(t *testing.T, taskID, stateDir, acceptance, providersFile, providerID string) []string {
	t.Helper()
	return []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--providers", providersFile,
		"--provider-id", providerID,
		"--recipes", filepath.Join(codingLoopFixture, "recipes.json"),
		"--recipe-policy", "test=allow",
		"--write-policy", "write_file=allow",
		"--acceptance", acceptance,
		"--log-level", "error",
	}
}

// TestProviderE2ECodingLoopAcrossAllFamilies runs the full deterministic
// inspect/edit/test/fix trajectory through each supported protocol family,
// using the real CLI composition and a provider-shaped httptest endpoint.
func TestProviderE2ECodingLoopAcrossAllFamilies(t *testing.T) {
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			workspace := copyCodingLoopFixture(t)
			initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
			wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
			wrongHash := hashOfBytes([]byte(wrongFix))
			correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
			correctHash := hashOfBytes([]byte(correctFix))
			acceptance := codingLoopAcceptance(t, correctHash)

			wire := newE2EWire(family.ProtocolFamily, codingLoopScript(t, initialHash, wrongFix, wrongHash, correctFix)...)
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			providersFile := writeProvidersFile(t, family, map[string]string{"e2e-provider": server.URL + "/v1"})

			stateDir := t.TempDir()
			var out, errOut strings.Builder
			code := run(context.Background(), providerRunArgs(t, workspace, stateDir, acceptance, providersFile, "e2e-provider"), &out, &errOut)
			if code != exitSuccess {
				t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "outcome: completed") {
				t.Fatalf("run must complete:\n%s", out.String())
			}
			// The verified completion projection exposes the sanitized
			// provider-neutral execution identity (provider ID, protocol
			// family, exact model, config identity, adapter version).
			for _, want := range []string{
				"Provider identity:",
				"provider_id=e2e-provider",
				"protocol_family=" + string(family.ProtocolFamily),
				"model=e2e-model",
				"config_identity=",
				"adapter_version=compatible-provider-v0.1",
				"check=tests-pass type=recipe_exit_zero status=passed",
			} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("verified result missing %q:\n%s", want, out.String())
				}
			}

			// The real workspace holds the corrected implementation and git
			// observes the change.
			content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
			if err != nil {
				t.Fatal(err)
			}
			if hashOfBytes(content) != correctHash {
				t.Fatalf("workspace does not hold the corrected implementation")
			}
			statusCmd := exec.Command("git", "-C", workspace, "status", "--short", "--no-renames", "--", ".")
			statusCmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_NOGLOBAL=1", "GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0")
			statusOutput, err := statusCmd.CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(statusOutput), " M app/calc.go") {
				t.Fatalf("git must observe the modified implementation:\n%s", statusOutput)
			}

			// Durable provider identity is inspectable per attempt.
			taskID := taskIDFromOutput(t, errOut.String())
			rendered := inspectRendered(t, stateDir, taskID)
			for _, want := range []string{
				"family=" + string(family.ProtocolFamily),
				"config_identity=",
				"decision=passed",
				"recipe=test exit=1",
				"recipe=test exit=0",
				"during-task changes: app/calc.go ( M)",
			} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("inspect missing %q:\n%s", want, rendered)
				}
			}

			// Attempt accounting: 9 scripted responses -> 9 governed model
			// turns -> exactly 9 physical requests, no amplification.
			// (8 actions + 1 final proposal.)
			if got := wire.count(); got != 9 {
				t.Fatalf("physical model-effect requests = %d, want exactly 9 (one per governed admission)", got)
			}
		})
	}
}

// TestProviderE2ETwoIdentitiesShareOneAdapter proves, through the REAL agent
// loop, that two different provider_ids can use the same protocol adapter and
// family with no agent-loop changes: the same loop binary executes both
// endpoints end to end.
func TestProviderE2ETwoIdentitiesShareOneAdapter(t *testing.T) {
	miniScript := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Inspected.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"},{"evidence_id":"obs-000002","tool":"list_files"}]}</runstead_final>`,
	}
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			wireAlpha := newE2EWire(family.ProtocolFamily, miniScript...)
			serverAlpha := httptest.NewServer(wireAlpha.handler())
			t.Cleanup(serverAlpha.Close)
			wireBeta := newE2EWire(family.ProtocolFamily, miniScript...)
			serverBeta := httptest.NewServer(wireBeta.handler())
			t.Cleanup(serverBeta.Close)
			providersFile := writeProvidersFile(t, family, map[string]string{
				"identity-alpha": serverAlpha.URL + "/v1",
				"identity-beta":  serverBeta.URL + "/v1",
			})

			for _, providerID := range []string{"identity-alpha", "identity-beta"} {
				stateDir := t.TempDir()
				var out, errOut strings.Builder
				code := run(context.Background(), providerRunArgs(t, workspace, stateDir, acceptance, providersFile, providerID), &out, &errOut)
				if code != exitSuccess {
					t.Fatalf("run(%s) exit = %d\nstderr:\n%s", providerID, code, errOut.String())
				}
				if !strings.Contains(out.String(), "outcome: completed") ||
					!strings.Contains(out.String(), "provider_id="+providerID) ||
					!strings.Contains(out.String(), "protocol_family="+string(family.ProtocolFamily)) {
					t.Fatalf("run(%s) must complete with the sanitized identity:\n%s", providerID, out.String())
				}
			}
			if got := wireAlpha.count(); got != 3 {
				t.Fatalf("identity-alpha physical requests = %d, want 3", got)
			}
			if got := wireBeta.count(); got != 3 {
				t.Fatalf("identity-beta physical requests = %d, want 3", got)
			}
		})
	}
}

// TestProviderE2ECrashAfterFirstWriteResumesThroughSameProvider runs the #13
// interruption boundary through a CONFIGURED provider endpoint: the process
// dies after the first scoped write effect, and resume continues through the
// SAME provider endpoint (explicitly re-supplied), reconciling the write
// without duplicating effects and completing the corrective trajectory.
func TestProviderE2ECrashAfterFirstWriteResumesThroughSameProvider(t *testing.T) {
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			workspace := copyCodingLoopFixture(t)
			initialHash := hashOfBytes([]byte(codingLoopFixtureFile(t, "app/calc.go")))
			wrongFix := codingLoopFixtureFile(t, "fixes/calc-wrong.go")
			wrongHash := hashOfBytes([]byte(wrongFix))
			correctFix := codingLoopFixtureFile(t, "fixes/calc-correct.go")
			correctHash := hashOfBytes([]byte(correctFix))
			acceptance := codingLoopAcceptance(t, correctHash)

			// One shared server: requests 1-4 answer the crashed run script,
			// requests 5-9 answer the resume script.
			runScript := codingLoopScript(t, initialHash, wrongFix, wrongHash, correctFix)[:4]
			resumeScript := codingLoopScript(t, initialHash, wrongFix, wrongHash, correctFix)[4:]
			wire := newE2EWire(family.ProtocolFamily, append(append([]string{}, runScript...), resumeScript...)...)
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			providersFile := writeProvidersFile(t, family, map[string]string{"e2e-crash-provider": server.URL + "/v1"})

			stateDir := t.TempDir()
			args := providerRunArgs(t, workspace, stateDir, acceptance, providersFile, "e2e-crash-provider")
			command := exec.Command(os.Args[0], "-test.run=TestRuntimeCodingLoopCrashHelper")
			command.Env = append(os.Environ(),
				"RUNSTEAD_CODING_CRASH_HELPER=1",
				"RUNSTEAD_CODING_CRASH_POINT=write_after_effect",
				"RUNSTEAD_CODING_CRASH_AFTER=1",
				"RUNSTEAD_CODING_CRASH_ARGS="+strings.Join(args, "\x1f"),
			)
			output, err := command.CombinedOutput()
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 42 {
				t.Fatalf("crash helper exit = %v\n%s", err, output)
			}
			taskID := taskIDFromOutput(t, string(output))

			// The wrong-fix write effect happened and stayed prepared.
			content, err := os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
			if err != nil {
				t.Fatal(err)
			}
			if hashOfBytes(content) != wrongHash {
				t.Fatalf("workspace must hold the wrong fix after the first write crash")
			}
			if rendered := inspectRendered(t, stateDir, taskID); !strings.Contains(rendered, "status=prepared") {
				t.Fatalf("the interrupted write attempt must stay prepared:\n%s", rendered)
			}

			// Resume through the SAME configured provider endpoint.
			var out, errOut strings.Builder
			resumeCode := run(context.Background(), providerResumeArgs(t, taskID, stateDir, acceptance, providersFile, "e2e-crash-provider"), &out, &errOut)
			if resumeCode != exitSuccess {
				t.Fatalf("resume exit = %d\nstderr:\n%s", resumeCode, errOut.String())
			}
			if !strings.Contains(out.String(), "outcome: completed") {
				t.Fatalf("resume must complete:\n%s", out.String())
			}
			content, err = os.ReadFile(filepath.Join(workspace, "app", "calc.go"))
			if err != nil {
				t.Fatal(err)
			}
			if hashOfBytes(content) != correctHash {
				t.Fatalf("final workspace must hold the corrected implementation")
			}
			// The interrupted write was reconciled, never re-executed; exactly
			// one corrective write was executed after resume.
			counts := toolAttemptCounts(t, stateDir, taskID)
			if counts["write_file/reconciled"] != 1 || counts["write_file/completed"] != 1 {
				t.Fatalf("write attempt projections = %v, want one reconciled wrong fix + one completed correction", counts)
			}
			// Request accounting across crash+resume: 4 + 5 = 9 physical
			// requests, one per governed admission; no replay of the finished
			// write turn.
			if got := wire.count(); got != 9 {
				t.Fatalf("physical requests across run+resume = %d, want 9", got)
			}

			// No credential-shaped bytes anywhere in the durable database.
			rawDB, err := os.ReadFile(filepath.Join(stateDir, "runstead.db"))
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"sk-", "Bearer ", "TEST_TOKEN", "Authorization"} {
				if strings.Contains(string(rawDB), forbidden) {
					t.Fatalf("durable state contains credential-shaped content %q", forbidden)
				}
			}
		})
	}
}

// TestProviderRunSelectionFailsClosed proves the explicit operator selection
// surface rejects ambiguous, unknown or invalid selections before any
// dispatch.
func TestProviderRunSelectionFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	providersFile := writeProvidersFile(t, e2eFamilies[0], map[string]string{"alpha": "http://127.0.0.1:1/v1"})
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"providers without provider id", []string{"run", "--workspace", workspace, "--providers", providersFile}, exitUsage},
		{"provider id without providers", []string{"run", "--workspace", workspace, "--provider-id", "alpha"}, exitUsage},
		{"unknown provider id", []string{"run", "--workspace", workspace, "--providers", providersFile, "--provider-id", "missing"}, exitUsage},
		{"scripted combined with providers", []string{"run", "--workspace", workspace, "--scripted", "/tmp/none.jsonl", "--providers", providersFile, "--provider-id", "alpha"}, exitUsage},
		{"no provider at all", []string{"run", "--task", "x", "--workspace", workspace}, exitUnavailable},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			var out, errOut strings.Builder
			code := run(context.Background(), append(testCase.args, "--state-dir", t.TempDir(), "--log-level", "error"), &out, &errOut)
			if code != testCase.code {
				t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, testCase.code, errOut.String())
			}
		})
	}
	// An invalid declarations file fails closed before any dispatch.
	badFile := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badFile, []byte(`{"version":1,"providers":[{"provider_id":"x","protocol_family":"bogus","base_url":"http://x","model":"m","auth_requirement":"none","profile":{"profile_version":"v1","capabilities":["text_turn"]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := run(context.Background(), []string{"run", "--workspace", workspace, "--providers", badFile, "--provider-id", "x", "--state-dir", t.TempDir(), "--log-level", "error"}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("invalid declarations exit = %d, want %d\nstderr:\n%s", code, exitUsage, errOut.String())
	}
}

// TestProviderResumeRejectsSilentProviderSwitch proves resume never switches
// provider identity silently: a task interrupted through one configured
// provider cannot continue through a different provider_id (fail closed
// before the recovery pipeline).
func TestProviderResumeRejectsSilentProviderSwitch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wireAlpha := newE2EWire(provider.FamilyOpenAICompatible,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"list_files","arguments":{"path":"."}}</runstead_action>`,
	)
	serverAlpha := httptest.NewServer(wireAlpha.handler())
	t.Cleanup(serverAlpha.Close)
	wireBeta := newE2EWire(provider.FamilyOpenAICompatible, `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`)
	serverBeta := httptest.NewServer(wireBeta.handler())
	t.Cleanup(serverBeta.Close)
	providersAlpha := writeProvidersFile(t, e2eFamilies[0], map[string]string{"alpha": serverAlpha.URL + "/v1"})
	providersBeta := writeProvidersFile(t, e2eFamilies[0], map[string]string{"beta": serverBeta.URL + "/v1"})

	stateDir := t.TempDir()
	// Crash right after the first governed provider attempt: the task stays
	// running with provider identity alpha persisted.
	args := []string{
		"run", "--task", "inspect the workspace",
		"--workspace", workspace,
		"--providers", providersAlpha,
		"--provider-id", "alpha",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
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

	// Resume through provider beta must be refused: provider id diverged.
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--providers", providersBeta,
		"--provider-id", "beta",
		"--log-level", "error",
	}, &out, &errOut)
	if code != exitUnavailable {
		t.Fatalf("divergent resume exit = %d, want %d\nstderr:\n%s", code, exitUnavailable, errOut.String())
	}
	if !strings.Contains(errOut.String(), "resume never switches providers silently") {
		t.Fatalf("divergence must be reported fail-closed:\n%s", errOut.String())
	}
}
