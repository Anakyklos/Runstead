package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/agent"
	"github.com/RenyEnnos/Runstead/internal/provider/omniroute"
)

// fakeOmniRouteServer serves the management gateway contract fixtures and the
// protected chat POST exactly once, recording request headers and the POST
// count so tests can prove the pinned lane wire behavior deterministically.
type fakeOmniRouteServer struct {
	server        *httptest.Server
	chatPosts     atomic.Int32
	chatPath      string
	lastRequestID string
	lastConnPin   string
	lastReceiptV1 string
	receiptBody   func(requestID string) string
}

func newFakeOmniRouteServer(t *testing.T, chatStatus int, chatBody string) *fakeOmniRouteServer {
	t.Helper()
	fake := &fakeOmniRouteServer{
		chatPath: "/v1/providers/chatgpt-web/chat/completions",
		receiptBody: func(requestID string) string {
			now := time.Now().UTC().Format(time.RFC3339)
			started := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
			return fmt.Sprintf(`{"schema_version":1,"client_request_id":%q,"finalized":true,"receipts":[{"schema_version":1,"attempt_id":"fixture-attempt-1","client_request_id":%q,"sequence":1,"provider":"chatgpt-web","model":"chatgpt-web/model","account_lane_hash":%q,"started_at":%q,"completed_at":%q,"outcome":"success","trigger":"initial","upstream_reached":true}]}`,
				requestID, requestID, omniroute.LaneHashForConnection("conn-test-123"), started, now)
		},
	}
	management := map[string]string{
		"/api/resilience":             readContractFixture(t, "resilience-safe.json"),
		"/api/settings":               readContractFixture(t, "settings-safe.json"),
		"/api/models/alias":           readContractFixture(t, "model-aliases-safe.json"),
		"/api/settings/model-aliases": readContractFixture(t, "settings-model-aliases-safe.json"),
		"/api/fallback/chains":        readContractFixture(t, "fallback-chains-safe.json"),
		"/api/combos":                 readContractFixture(t, "combos-safe.json"),
		"/api/model-combo-mappings":   readContractFixture(t, "model-combo-mappings-safe.json"),
		"/api/providers":              readContractFixture(t, "providers-safe.json"),
		"/api/rate-limits":            readContractFixture(t, "rate-limits-safe.json"),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fake.chatPath {
			fake.chatPosts.Add(1)
			fake.lastRequestID = r.Header.Get("X-Runstead-Client-Request-Id")
			fake.lastConnPin = r.Header.Get("X-OmniRoute-Connection")
			fake.lastReceiptV1 = r.Header.Get("X-Runstead-Attempt-Receipts")
			w.Header().Set("Content-Type", "application/json")
			if chatBody != "" {
				w.Header().Set("X-OmniRoute-Attempt-Receipts", fake.receiptBody(fake.lastRequestID))
				io.WriteString(w, chatBody)
				return
			}
			w.WriteHeader(chatStatus)
			return
		}
		if body, ok := management[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	return fake
}

func readContractFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "internal", "provider", "omniroute", "testdata", "contract", "management", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contract fixture %s: %v", name, err)
	}
	return string(body)
}

func TestRunLiveOmniRouteWithoutConnectionPinFailsClosed(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "inspect the repo",
		"--omniroute-base-url", "http://omniroute.test/v1",
		"--omniroute-api-key", "secret",
		"--omniroute-model", "chatgpt-web/model",
		// Legacy single-attempt declaration: it can never authorize the live
		// receipt lane, so the run must still fail closed on the missing pin.
		"--omniroute-safe-route",
	}), &out, &errOut)
	if code != exitUnavailable {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, exitUnavailable, errOut.String())
	}
	if !strings.Contains(errOut.String(), "OMNIROUTE_CONNECTION_ID") {
		t.Fatalf("run diagnostic = %q, want connection pin requirement", errOut.String())
	}
}

func TestRunLiveOmniRouteGatewayUnhealthyBlocksBeforeModelRequest(t *testing.T) {
	workspace := t.TempDir()
	fake := newFakeOmniRouteServer(t, http.StatusOK, "")
	defer fake.server.Close()
	t.Setenv("OMNIROUTE_BASE_URL", fake.server.URL+"/v1")
	t.Setenv("OMNIROUTE_API_KEY", "secret")
	t.Setenv("OMNIROUTE_MODEL", "chatgpt-web/model")
	t.Setenv("OMNIROUTE_CONNECTION_ID", "conn-test-123")

	// Make the management gateway contract drift: every management endpoint
	// 404s, so the probe classifies the contract as protocol_changed.
	fake.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fake.chatPath {
			fake.chatPosts.Add(1)
			http.Error(w, "unexpected chat POST", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	})

	var out, errOut bytes.Buffer
	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "inspect the repo", "--workspace", workspace,
		"--min-start-interval", "1ms", "--log-level", "error",
	}), &out, &errOut)
	if code != exitUnavailable {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, exitUnavailable, errOut.String())
	}
	if !strings.Contains(errOut.String(), "gateway contract is not healthy") {
		t.Fatalf("run diagnostic = %q, want gateway contract failure", errOut.String())
	}
	if fake.chatPosts.Load() != 0 {
		t.Fatalf("chat POSTs = %d, want 0 before a healthy gateway contract", fake.chatPosts.Load())
	}
}

func TestRunLiveOmniRoutePinnedLaneReachesProviderExactlyOnce(t *testing.T) {
	workspace := t.TempDir()
	final := `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`
	chatBody := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, final)
	fake := newFakeOmniRouteServer(t, http.StatusOK, chatBody)
	defer fake.server.Close()
	t.Setenv("OMNIROUTE_BASE_URL", fake.server.URL+"/v1")
	t.Setenv("OMNIROUTE_API_KEY", "secret")
	t.Setenv("OMNIROUTE_MODEL", "chatgpt-web/model")
	t.Setenv("OMNIROUTE_CONNECTION_ID", "conn-test-123")

	var out, errOut bytes.Buffer
	code := run(context.Background(), withStateDir(t, []string{
		"run", "--task", "inspect the workspace", "--workspace", workspace,
		"--min-start-interval", "1ms", "--log-level", "error",
	}), &out, &errOut)
	// The protected round trip completed: one governed model request reached
	// the fake provider and returned a receipt. The ungrounded final stops the
	// loop deterministically without any network.
	if code != agent.OutcomeFinalNotGrounded.ExitCode() {
		t.Fatalf("run exit code = %d, want %d\nstderr:\n%s", code, agent.OutcomeFinalNotGrounded.ExitCode(), errOut.String())
	}
	if fake.chatPosts.Load() != 1 {
		t.Fatalf("chat POSTs = %d, want exactly 1", fake.chatPosts.Load())
	}
	if fake.lastConnPin != "conn-test-123" {
		t.Fatalf("connection pin header = %q, want conn-test-123", fake.lastConnPin)
	}
	if fake.lastReceiptV1 != "v1" {
		t.Fatalf("receipt opt-in header = %q, want v1", fake.lastReceiptV1)
	}
	if fake.lastRequestID == "" {
		t.Fatal("client request id header must be present")
	}
	if !strings.Contains(errOut.String(), "final_not_grounded") {
		t.Fatalf("run diagnostic = %q, want final_not_grounded", errOut.String())
	}
}

func TestProfileOmniRouteCrashResumeReusesFrozenProviderIdentity(t *testing.T) {
	workspace := t.TempDir()
	profile := writeCompositionProfile(t, `{"version":1,"profile_id":"omni-audit","profile_version":"1.0.0","packages":[{"id":"repo.read","version":"1.0.0"}]}`)
	final := `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`
	chatBody := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, final)
	fake := newFakeOmniRouteServer(t, http.StatusOK, chatBody)
	defer fake.server.Close()

	stateDir := t.TempDir()
	omniArgs := []string{
		"--omniroute-base-url", fake.server.URL + "/v1",
		"--omniroute-api-key", "fixture-api-key",
		"--omniroute-model", "chatgpt-web/model",
		"--omniroute-connection-id", "conn-test-123",
	}
	runArgs := append([]string{
		"run", "--task", "inspect the workspace", "--workspace", workspace,
		"--profile", profile, "--state-dir", stateDir,
		"--min-start-interval", "1ms", "--log-level", "error",
	}, omniArgs...)
	code, output := runCrashedRun(t, runArgs, "provider_tx1_after")
	if code != 42 {
		t.Fatalf("crashed OmniRoute run exit = %d, want 42\n%s", code, output)
	}
	taskID := taskIDFromOutput(t, output)
	if got := fake.chatPosts.Load(); got != 0 {
		t.Fatalf("chat POSTs before resume = %d, want 0 after TX1 crash", got)
	}
	rendered := inspectRendered(t, stateDir, taskID)
	for _, want := range []string{
		"profile: omni-audit@1.0.0",
		"provider: omniroute family=openai_compatible model=chatgpt-web/model",
		"status=prepared",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("inspect missing %q after OmniRoute crash:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "fixture-api-key") || strings.Contains(rendered, "conn-test-123") {
		t.Fatalf("inspect leaked OmniRoute credential or connection pin:\n%s", rendered)
	}

	var missingOut, missingErr strings.Builder
	missingCode := run(context.Background(), []string{
		"resume", taskID, "--state-dir", stateDir, "--profile", profile, "--log-level", "error",
	}, &missingOut, &missingErr)
	if missingCode != exitUnavailable || !strings.Contains(missingErr.String(), "original OmniRoute configuration") {
		t.Fatalf("resume without OmniRoute inputs = %d, want unavailable with explicit configuration error\nstderr:\n%s", missingCode, missingErr.String())
	}
	if got := fake.chatPosts.Load(); got != 0 {
		t.Fatalf("chat POSTs after rejected resume = %d, want 0", got)
	}
	driftOmniArgs := append([]string(nil), omniArgs...)
	for index := range driftOmniArgs {
		if driftOmniArgs[index] == "--omniroute-model" && index+1 < len(driftOmniArgs) {
			driftOmniArgs[index+1] = "chatgpt-web/other-model"
		}
	}
	var driftOut, driftErr strings.Builder
	driftCode := run(context.Background(), append([]string{
		"resume", taskID, "--state-dir", stateDir, "--profile", profile, "--log-level", "error",
	}, driftOmniArgs...), &driftOut, &driftErr)
	if driftCode != exitUnavailable || !strings.Contains(driftErr.String(), "OmniRoute configuration divergence") {
		t.Fatalf("resume with drifted OmniRoute model = %d, want unavailable with explicit divergence\nstderr:\n%s", driftCode, driftErr.String())
	}
	if got := fake.chatPosts.Load(); got != 0 {
		t.Fatalf("chat POSTs after drifted resume = %d, want 0", got)
	}

	resumeArgs := append([]string{
		"resume", taskID, "--state-dir", stateDir, "--profile", profile,
		"--log-level", "error",
	}, omniArgs...)
	var resumeOut, resumeErr strings.Builder
	resumeCode := run(context.Background(), resumeArgs, &resumeOut, &resumeErr)
	if resumeCode != exitGovernorBlocked {
		t.Fatalf("OmniRoute resume exit = %d, want %d (conservative recovery block)\nstderr:\n%s\nstdout:\n%s", resumeCode, exitGovernorBlocked, resumeErr.String(), resumeOut.String())
	}
	if !strings.Contains(resumeErr.String(), "conservative accounting is unsafe") {
		t.Fatalf("OmniRoute resume must explain the conservative recovery block:\n%s", resumeErr.String())
	}
	if got := fake.chatPosts.Load(); got != 0 {
		t.Fatalf("chat POSTs across crash/rejected resume = %d, want 0 because uncertain receipt-aware work is never retried", got)
	}
	after := inspectRendered(t, stateDir, taskID)
	if !strings.Contains(after, "provider: omniroute family=openai_compatible model=chatgpt-web/model") {
		t.Fatalf("resume lost frozen OmniRoute provider identity:\n%s", after)
	}
	if !strings.Contains(after, "status=reconciled") || !strings.Contains(after, "recovery_blocked") {
		t.Fatalf("resume must reconcile the prepared attempt and preserve the recovery block:\n%s", after)
	}
}
