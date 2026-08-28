package main

// Issue #92 integration: the real runtime path
//   provider config -> registry/resolve -> compatible adapter ->
//   agent.Executor (governor-owned retry) -> synthetic httptest endpoint
// The synthetic server counts physical requests and proves:
//   attempt 1 -> admission 1 -> physical request 1 (429/503)
//   retry    -> admission 2 -> physical request 2 (200 envelope)
// and never admission 1 -> two physical requests. The durable accounting
// (provider attempts and governor retries) is inspected from SQLite.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// e2eRetryWire is a provider-shaped endpoint that fails the first N requests
// with a retryable status (429/503) and then serves the deterministic
// runstead envelopes, counting every physical request.
type e2eRetryWire struct {
	family     provider.ProtocolFamily
	responses  []string
	firstFails int
	status     int
	retryAfter string
	mu         sync.Mutex
	requests   int
}

func (w *e2eRetryWire) handler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		w.mu.Lock()
		w.requests++
		count := w.requests
		w.mu.Unlock()
		if count <= w.firstFails {
			if w.retryAfter != "" {
				response.Header().Set("Retry-After", w.retryAfter)
			}
			response.WriteHeader(w.status)
			return
		}
		index := count - w.firstFails - 1
		if index >= len(w.responses) {
			index = len(w.responses) - 1
		}
		body := e2eWrapResponse(w.family, w.responses[index])
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(body)
	})
}

func (w *e2eRetryWire) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

// retryRunArgs builds run arguments with the bounded retry policy enabled.
func retryRunArgs(t *testing.T, workspace, stateDir, acceptance, providersFile, providerID string) []string {
	t.Helper()
	args := monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, providerID)
	return append(args, "--retry-policy", "bounded")
}

// assertDurableAccounting reads attempts/retries from the persisted governor
// task states and the provider attempt row count via the public store API.
func assertDurableAccounting(t *testing.T, stateDir, taskID string, wantAttempts int, wantRetries int) {
	t.Helper()
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	persisted, ok, err := store.GovernorState(ctx)
	if err != nil || !ok {
		t.Fatalf("read governor state: ok=%v err=%v", ok, err)
	}
	attempts, retries := -1, -1
	for _, taskState := range persisted.TaskStates {
		if taskState.TaskID == taskID {
			attempts, retries = taskState.Attempts, taskState.Retries
			break
		}
	}
	if attempts != wantAttempts || retries != wantRetries {
		t.Fatalf("governor task state = attempts %d retries %d, want %d/%d", attempts, retries, wantAttempts, wantRetries)
	}
	snapshot, err := store.LoadRecoverySnapshot(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ProviderAttempts) != wantAttempts {
		t.Fatalf("provider attempt rows = %d, want %d (each retry is its own durable attempt)", len(snapshot.ProviderAttempts), wantAttempts)
	}
}

// TestRetryE2ERateLimitRecoversThroughNewAdmission runs the full CLI path
// through each compatible family: the first physical request returns 429 +
// Retry-After, the retry (a new governed admission) recovers, and the task
// completes with exactly 3 physical requests and durable attempts/retries.
func TestRetryE2ERateLimitRecoversThroughNewAdmission(t *testing.T) {
	script := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"inspected","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	}
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			wire := &e2eRetryWire{family: family.ProtocolFamily, responses: script, firstFails: 1, status: http.StatusTooManyRequests, retryAfter: "1"}
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			providersFile := writeProvidersFile(t, family, map[string]string{"retry-provider": server.URL + "/v1"})
			acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

			stateDir := t.TempDir()
			var out, errOut strings.Builder
			code := run(context.Background(), retryRunArgs(t, workspace, stateDir, acceptance, providersFile, "retry-provider"), &out, &errOut)
			if code != exitSuccess {
				t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "outcome: completed") {
				t.Fatalf("run must complete:\n%s", out.String())
			}
			// One admission per physical request: turn 1 = 429 + retry,
			// turn 2 = final => 3 physical requests.
			if got := wire.count(); got != 3 {
				t.Fatalf("physical requests = %d, want 3 (attempt + retry + next turn)", got)
			}
			taskID := taskIDFromOutput(t, errOut.String())
			assertDurableAccounting(t, stateDir, taskID, 3, 1)
		})
	}
}

// TestRetryE2ETransient503Recovers testa um 503 transitório no mesmo caminho.
func TestRetryE2ETransient503Recovers(t *testing.T) {
	script := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"inspected","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	}
	family := e2eFamilies[0]
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wire := &e2eRetryWire{family: family.ProtocolFamily, responses: script, firstFails: 1, status: http.StatusServiceUnavailable}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	providersFile := writeProvidersFile(t, family, map[string]string{"retry-provider": server.URL + "/v1"})
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), retryRunArgs(t, workspace, stateDir, acceptance, providersFile, "retry-provider"), &out, &errOut)
	if code != exitSuccess {
		t.Fatalf("run exit = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if got := wire.count(); got != 3 {
		t.Fatalf("physical requests = %d, want 3 (503 + retry + next turn)", got)
	}
	taskID := taskIDFromOutput(t, errOut.String())
	assertDurableAccounting(t, stateDir, taskID, 3, 1)
}

// TestRetryE2EAuthFailureNeverRetries: 401 is not retryable even with the
// bounded policy enabled; exactly one physical request happens.
func TestRetryE2EAuthFailureNeverRetries(t *testing.T) {
	family := e2eFamilies[0]
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wire := &e2eRetryWire{family: family.ProtocolFamily, responses: []string{}, firstFails: 100, status: http.StatusUnauthorized}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	providersFile := writeProvidersFile(t, family, map[string]string{"retry-provider": server.URL + "/v1"})
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), retryRunArgs(t, workspace, stateDir, acceptance, providersFile, "retry-provider"), &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("auth failure must not complete")
	}
	if got := wire.count(); got != 1 {
		t.Fatalf("physical requests = %d, want exactly 1 (auth never retried)", got)
	}
	if !strings.Contains(errOut.String(), "authentication") {
		t.Fatalf("failure classification must surface:\n%s", errOut.String())
	}
}

// TestRetryE2EOffByDefault: without --retry-policy bounded, a 429 is
// returned as the historical single attempt (no implicit retries).
func TestRetryE2EOffByDefault(t *testing.T) {
	family := e2eFamilies[0]
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wire := &e2eRetryWire{family: family.ProtocolFamily, responses: []string{}, firstFails: 100, status: http.StatusTooManyRequests}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	providersFile := writeProvidersFile(t, family, map[string]string{"retry-provider": server.URL + "/v1"})
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "retry-provider"), &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("429 without policy must not complete")
	}
	if got := wire.count(); got != 1 {
		t.Fatalf("physical requests = %d, want 1 (retry disabled by default)", got)
	}
}

// TestRetryE2ERetryPolicyRejectedOnScripted is a CLI fail-closed regression.
func TestRetryE2ERetryPolicyRejectedOnScripted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t, `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"x","evidence":[]}</runstead_final>`)
	var out, errOut strings.Builder
	code := run(context.Background(), []string{
		"run", "--task", "x", "--workspace", workspace,
		"--scripted", script, "--retry-policy", "bounded",
		"--state-dir", t.TempDir(), "--log-level", "error",
	}, &out, &errOut)
	if code != exitUsage {
		t.Fatalf("exit = %d, want usage; stderr:\n%s", code, errOut.String())
	}
}

// TestRetryE2ERestartNoAutomaticReplay prova que o restart do processo nunca
// reexecuta automaticamente uma tentativa histórica incerta: o retry
// orquestrado é process-local; após reiniciar, nenhuma chamada física é
// disparada apenas por existirem Rows preparadas.
func TestRetryE2ERestartNoAutomaticReplay(t *testing.T) {
	family := e2eFamilies[0]
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wire := &e2eRetryWire{family: family.ProtocolFamily, responses: []string{}, firstFails: 100, status: http.StatusTooManyRequests}
	server := httptest.NewServer(wire.handler())
	t.Cleanup(server.Close)
	providersFile := writeProvidersFile(t, family, map[string]string{"retry-provider": server.URL + "/v1"})

	// Crash right after the first governed provider attempt (task stays
	// running with one prepared/consumed attempt).
	args := append(monotonicRunArgs(t, workspace, t.TempDir(), writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`), providersFile, "retry-provider"), "--retry-policy", "bounded")
	stateDir := t.TempDir()
	args = replaceStateDir(args, stateDir)
	command := exec.Command(os.Args[0], "-test.run=TestRuntimeCodingLoopCrashHelper")
	command.Env = append(os.Environ(),
		"RUNSTEAD_CODING_CRASH_HELPER=1",
		"RUNSTEAD_CODING_CRASH_POINT=provider_tx2_before",
		"RUNSTEAD_CODING_CRASH_AFTER=1",
		"RUNSTEAD_CODING_CRASH_ARGS="+strings.Join(args, "\x1f"),
	)
	output, err := command.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 42 {
		t.Fatalf("crash helper exit = %v\n%s", err, output)
	}
	before := wire.count()
	if before != 1 {
		t.Fatalf("crashed run physical requests = %d, want 1", before)
	}

	// Reopen the durable state (restart simulation): no provider request may
	// be issued just because rows/gov state exist.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if after := wire.count(); after != before {
		t.Fatalf("restart issued %d new physical requests; retries must not replay after restart", after-before)
	}
}

func replaceStateDir(args []string, dir string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--state-dir" {
			out = append(out, "--state-dir", dir)
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

var _ = fmt.Sprintf
