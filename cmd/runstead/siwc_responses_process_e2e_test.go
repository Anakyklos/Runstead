package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/responsessiwc"
	"github.com/RenyEnnos/Runstead/internal/state"
)

const siwcProcessHelperEnv = "RUNSTEAD_SIWC_RESPONSES_PROCESS_HELPER"

func init() {
	if os.Getenv(siwcProcessHelperEnv) != "1" {
		return
	}
	configuredProviderClient = func(resolved provider.Resolved) (provider.Client, error) {
		return responsessiwc.NewLoopbackForTest(resolved, os.Getenv("RUNSTEAD_SIWC_RESPONSES_ENDPOINT"))
	}
}

func TestSIWCResponsesMultiTurnProcessHelper(t *testing.T) {
	if os.Getenv(siwcProcessHelperEnv) != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("RUNSTEAD_SIWC_RESPONSES_ARGS")), &args); err != nil {
		t.Fatalf("decode child CLI arguments: %v", err)
	}
	code := run(context.Background(), args, os.Stdout, os.Stderr)
	if code != exitSuccess && !(len(args) > 0 && args[0] == "resume" && code == exitHumanReview) {
		t.Fatalf("child runstead CLI exit=%d", code)
	}
	if len(args) > 0 && args[0] == "resume" {
		fmt.Fprintf(os.Stdout, "SIWC_HELPER_EXIT=%d\n", code)
	}
}

func TestSIWCResponsesMultiTurnCLIProcessPersistsTwoGovernedCalls(t *testing.T) {
	base := t.TempDir()
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "state-home")
	stateDir, workspace := filepath.Join(base, "registered-state"), filepath.Join(base, "workspace")
	for _, dir := range []string{home, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	identity := loadSIWCFixtureIdentity(t, providers)
	seedSIWCDomainWithBinding(t, home, xdg, stateDir, "", false, true, identity)
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("local fixture evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acceptance := writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)

	action := `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`
	final := `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"Read the local fixture.","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer offline-access-fixture" {
			t.Errorf("unexpected Responses request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		index := len(requests)
		mu.Unlock()
		text := action
		if index == 2 {
			text = final
		}
		w.Header().Set("Content-Type", "text/event-stream")
		encodedDelta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": fmt.Sprintf("message-%d", index), "output_index": 0, "content_index": 0, "delta": text})
		fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\n", encodedDelta)
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"process-response-%d\",\"status\":\"completed\"}}\n\n", index)
	}))
	defer server.Close()

	args := []string{"run", "--task", "inspect the local fixture", "--workspace", workspace,
		"--providers", providers, "--provider-id", "siwc-cli-test", "--state-domain", "siwc",
		"--acceptance", acceptance, "--min-start-interval", "1ms", "--log-level", "error"}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSIWCResponsesMultiTurnProcessHelper$")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xdg, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		siwcProcessHelperEnv+"=1", "RUNSTEAD_SIWC_RESPONSES_ENDPOINT="+server.URL+"/v1", "RUNSTEAD_SIWC_RESPONSES_ARGS="+string(encodedArgs))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real CLI test process failed: %v\n%s", err, output)
	}
	if bytes.Contains(output, []byte("offline-access-fixture")) {
		t.Fatal("CLI process output leaked the fixture bearer token")
	}

	mu.Lock()
	got := append([]map[string]any(nil), requests...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("physical Responses requests=%d, want exactly two governed Complete calls", len(got))
	}
	for i, request := range got {
		if request["model"] != "gpt-test" || request["store"] != false || request["stream"] != true {
			t.Fatalf("request %d has unexpected wire fields: %#v", i+1, request)
		}
		input, ok := request["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("request %d input=%#v", i+1, request["input"])
		}
		message := input[0].(map[string]any)
		content := message["content"].(string)
		if message["role"] != "user" || !strings.Contains(content, "inspect the local fixture") {
			t.Fatalf("request %d omitted self-contained task transcript: %s", i+1, content)
		}
		if i == 1 && (!strings.Contains(content, "runstead_action") || !strings.Contains(content, "local fixture evidence")) {
			t.Fatalf("second request omitted prior assistant action or observation: %s", content)
		}
	}

	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var taskCount, attemptCount, completedAttempts, ledgerCount int
	if err := store.DB().QueryRow(`SELECT count(*) FROM tasks WHERE status = 'completed'`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*), coalesce(sum(status = 'completed'), 0) FROM provider_attempts`).Scan(&attemptCount, &completedAttempts); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM governor_ledger`).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || attemptCount != 2 || completedAttempts != 2 || ledgerCount != 2 {
		t.Fatalf("durable task/governor accounting tasks=%d attempts=%d completed=%d ledger=%d; want 1/2/2/2", taskCount, attemptCount, completedAttempts, ledgerCount)
	}
	databaseBytes, err := os.ReadFile(filepath.Join(stateDir, state.DefaultDBFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(databaseBytes, []byte("offline-access-fixture")) {
		t.Fatal("SQLite database contains the fixture bearer token")
	}
}

func TestSIWCResponsesDispatchedProcessCrashResumesWithoutReplay(t *testing.T) {
	base := t.TempDir()
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "state-home")
	stateDir, workspace := filepath.Join(base, "registered-state"), filepath.Join(base, "workspace")
	for _, dir := range []string{home, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	providers := filepath.Join(base, "providers.json")
	writeSIWCProviderFixture(t, providers)
	identity := loadSIWCFixtureIdentity(t, providers)
	seedSIWCDomainWithBinding(t, home, xdg, stateDir, "", false, true, identity)

	var requests atomic.Int32
	dispatched := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer offline-access-fixture" {
			t.Errorf("unexpected Responses request: %s %s", r.Method, r.URL.Path)
		}
		requests.Add(1)
		select {
		case dispatched <- struct{}{}:
		default:
		}
		// Hold the response open until the test kills the actual CLI process.
		<-releaseResponse
	}))
	defer server.Close()
	defer close(releaseResponse)

	startCLI := func(args []string) *exec.Cmd {
		t.Helper()
		encodedArgs, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestSIWCResponsesMultiTurnProcessHelper$")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+xdg, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			siwcProcessHelperEnv+"=1", "RUNSTEAD_SIWC_RESPONSES_ENDPOINT="+server.URL+"/v1", "RUNSTEAD_SIWC_RESPONSES_ARGS="+string(encodedArgs))
		return cmd
	}

	taskDescription := "interrupt dispatched SIWC response"
	cmd := startCLI([]string{"run", "--task", taskDescription, "--workspace", workspace,
		"--providers", providers, "--provider-id", "siwc-cli-test", "--state-domain", "siwc",
		"--min-start-interval", "1ms", "--log-level", "error"})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childRunning := true
	defer func() {
		if childRunning {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	select {
	case <-dispatched:
	case <-time.After(10 * time.Second):
		t.Fatal("local Responses server did not observe the governed POST")
	}
	if requests.Load() != 1 {
		t.Fatalf("physical Responses POSTs before interruption=%d, want one", requests.Load())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	childRunning = false

	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := store.DB().QueryRow(`SELECT task_id FROM tasks WHERE objective = ?`, taskDescription).Scan(&taskID); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	var attempts, ledgerRows int
	var attemptStatus string
	if err := store.DB().QueryRow(`SELECT count(*), min(status) FROM provider_attempts WHERE task_id = ?`, taskID).Scan(&attempts, &attemptStatus); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM governor_ledger WHERE task_id = ?`, taskID).Scan(&ledgerRows); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || attemptStatus != "prepared" || ledgerRows != 1 {
		t.Fatalf("after dispatched process crash attempts=%d status=%q ledger=%d; want one durably prepared and debited attempt", attempts, attemptStatus, ledgerRows)
	}

	resumeArgs := []string{"resume", taskID, "--state-domain", "siwc", "--providers", providers, "--provider-id", "siwc-cli-test"}
	for pass := 1; pass <= 2; pass++ {
		resume := startCLI(resumeArgs)
		output, err := resume.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "human review") || !strings.Contains(string(output), fmt.Sprintf("SIWC_HELPER_EXIT=%d", exitHumanReview)) {
			t.Fatalf("resume pass %d = (%v,%q), want durable human-review stop", pass, err, output)
		}
		if bytes.Contains(output, []byte("offline-access-fixture")) {
			t.Fatalf("resume pass %d output leaked the fixture token", pass)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("physical Responses POSTs after resume pass %d=%d, want no replay", pass, got)
		}
	}
	store, err = state.Open(state.Options{Path: filepath.Join(stateDir, state.DefaultDBFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var attemptsAfter, ledgerAfter, reconciled int
	var taskStatus string
	if err := store.DB().QueryRow(`SELECT count(*), coalesce(sum(status = 'reconciled'), 0) FROM provider_attempts WHERE task_id = ?`, taskID).Scan(&attemptsAfter, &reconciled); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM governor_ledger WHERE task_id = ?`, taskID).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT status FROM tasks WHERE task_id = ?`, taskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if attemptsAfter != 1 || reconciled != 1 || ledgerAfter != 1 || taskStatus != "human_review_required" || requests.Load() != 1 {
		t.Fatalf("after recovery attempts=%d reconciled=%d ledger=%d task=%q POSTs=%d; want 1/1/1/human_review_required/1", attemptsAfter, reconciled, ledgerAfter, taskStatus, requests.Load())
	}
}
