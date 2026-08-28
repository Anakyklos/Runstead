package main

// Issue #93 E2E coverage: the full CLI path turns typed provider evidence
// into DURABLE conservative operational profile rows, enforcement makes the
// effective bounds real at the request frontier, learning survives restarts
// and identity isolation, and corruption fails closed.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

func learningScript() []string {
	return []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"inspected","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
	}
}

func learningWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func learningAcceptance(t *testing.T) string {
	t.Helper()
	return writeAcceptanceFile(t, `{"version":1,"checks":[{"id":"readme","type":"file_exists","path":"README.md"}]}`)
}

func loadProfileValue(t *testing.T, stateDir string, identity provider.Identity, field provider.ProfileField) provider.ProfileValue {
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
	return profile.Effective(field)
}

// TestLearningCooldownFromRetryAfterAllFamilies: a typed 429 with a proven
// Retry-After wait persists cooldown_millis (observed) with the task's
// structured evidence reference, across every protocol family. The physical
// request count stays 1: learning never issues requests of its own.
func TestLearningCooldownFromRetryAfterAllFamilies(t *testing.T) {
	script := learningScript()
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			wire := &e2eRetryWire{family: family.ProtocolFamily, responses: script, firstFails: 1, status: http.StatusTooManyRequests, retryAfter: "5"}
			server := httptest.NewServer(wire.handler())
			t.Cleanup(server.Close)
			providersFile := writeProvidersFileWithBounds(t, family.ProtocolFamily, "learn-provider", server.URL+"/v1", 0)
			identity := profileTighteningIdentity(t, providersFile, "learn-provider")
			stateDir := t.TempDir()
			var out, errOut strings.Builder
			// No retry policy: the run stops on the first 429, which is exactly
			// what isolates the LEARNING path (observation happens regardless
			// of retry orchestration).
			code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "learn-provider"), &out, &errOut)
			if code == exitSuccess {
				t.Fatalf("run must stop on the first 429\nstderr:\n%s", errOut.String())
			}
			if wire.count() != 1 {
				t.Fatalf("physical requests = %d, want 1 (learning never dispatches its own requests)", wire.count())
			}
			value := loadProfileValue(t, stateDir, identity, provider.FieldCooldownMillis)
			if value.Value != 5000 || value.Provenance != provider.ProvenanceObserved {
				t.Fatalf("cooldown learned = %+v, want 5000/observed", value)
			}
			if value.EvidenceRef.Kind != provider.EvidenceKindTask || !strings.HasPrefix(value.EvidenceRef.ID, "cli-") {
				t.Fatalf("learned row must carry the task evidence reference, got %+v", value.EvidenceRef)
			}
			// No prompt, response, header or other free text may ever reach
			// the profile table: only closed field names, numbers,
			// provenance and structured references.
			assertNoSensitiveProfileRows(t, stateDir, "inspect the workspace")
		})
	}
}

// assertNoSensitiveProfileRows scans the operational profile table and fails
// if any stored cell carries the given private/transcript marker.
func assertNoSensitiveProfileRows(t *testing.T, stateDir, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT field, value, provenance, evidence_ref FROM provider_operational_profiles`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var field, provenance, evidence string
		var value int64
		if err := rows.Scan(&field, &value, &provenance, &evidence); err != nil {
			t.Fatal(err)
		}
		for _, cell := range []string{field, provenance, evidence, fmt.Sprint(value)} {
			if strings.Contains(cell, marker) {
				t.Fatalf("sensitive content leaked into the operational profile: %q", cell)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestLearningSuccessNeverWritesProfile: successful runs across every family
// persist no observed information (success is not evidence of a limit).
func TestLearningSuccessNeverWritesProfile(t *testing.T) {
	script := learningScript()
	for _, family := range e2eFamilies {
		family := family
		t.Run(string(family.ProtocolFamily), func(t *testing.T) {
			wire := newE2EWire(family.ProtocolFamily, script...)
			server := newHTTPServerForWire(wire)
			t.Cleanup(server.Close)
			providersFile := writeProvidersFileWithBounds(t, family.ProtocolFamily, "learn-provider", server.URL+"/v1", 0)
			identity := profileTighteningIdentity(t, providersFile, "learn-provider")
			stateDir := t.TempDir()
			var out, errOut strings.Builder
			if code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "learn-provider"), &out, &errOut); code != exitSuccess {
				t.Fatalf("run exit = %d\nstderr:\n%s", code, errOut.String())
			}
			store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			profile, err := store.LoadOperationalProfile(context.Background(), identity)
			if err != nil {
				t.Fatal(err)
			}
			if profile != nil {
				for field, value := range profile.Values {
					if value.Provenance == provider.ProvenanceObserved {
						t.Fatalf("success run persisted observed %s=%d: success never learns", field, value.Value)
					}
				}
			}
		})
	}
}

// TestLearningEnforcedBoundRefusesOversizedTranscript: a PROVEN observed
// input bound becomes the execution frontier: a task whose first payload
// exceeds it is refused BEFORE any dispatch (zero physical requests), the
// run stops conservatively, and the durable bound is untouched.
func TestLearningEnforcedBoundRefusesOversizedTranscript(t *testing.T) {
	script := learningScript()
	wire := newE2EWire(provider.FamilyOpenAICompatible, script...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "learn-provider", server.URL+"/v1", 0)
	identity := profileTighteningIdentity(t, providersFile, "learn-provider")
	stateDir := t.TempDir()

	// Seed a proven observed bound far below the measured turn-one payload
	// (5204 bytes) through the durable boundary, exactly as a previous run's
	// typed evidence would have.
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyOperationalProfileUpdates(context.Background(), identity, nil, []provider.ProfileUpdate{{
		Field:       provider.FieldMaxRequestBytes,
		Value:       2048,
		Provenance:  provider.ProvenanceObserved,
		EvidenceRef: provider.EvidenceRef{Kind: provider.EvidenceKindEvidence, ID: "obs-000001"},
	}}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	var out, errOut strings.Builder
	code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "learn-provider"), &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must refuse an oversized transcript fail-closed")
	}
	if wire.count() != 0 {
		t.Fatalf("physical requests = %d, want 0 (refused before any dispatch)", wire.count())
	}
	if value := loadProfileValue(t, stateDir, identity, provider.FieldMaxRequestBytes); value.Value != 2048 || value.Provenance != provider.ProvenanceObserved {
		t.Fatalf("durable bound must be untouched: %+v", value)
	}
}

// TestLearningUnsupportedOptionPersistsBit: a typed
// unsupported_response_format signal (Anthropic wire: stop_reason
// "tool_use") is translated by the family-neutral mapping into the closed
// response_format option bit and persisted durably with the task evidence
// reference. The adaptive path itself dispatches nothing extra.
func TestLearningUnsupportedOptionPersistsBit(t *testing.T) {
	// The typed signal: the adapter classifies stop_reason "tool_use" as
	// ErrorUnsupportedResponseFormat without any vendor branch inside the
	// learning path. The first request receives the signal; later requests
	// would receive the deterministic script (the run stops at turn 1, so
	// only the first request is ever made).
	var wireMu sync.Mutex
	first := true
	signal := `{"content":null,"stop_reason":"tool_use"}`
	var physical int
	signalServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		wireMu.Lock()
		physical++
		isFirst := first
		first = false
		wireMu.Unlock()
		if isFirst {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(signal))
			return
		}
		body := e2eWrapResponse(provider.FamilyAnthropicCompatible, learningScript()[0])
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(body)
	}))
	t.Cleanup(signalServer.Close)

	providersFile := writeProvidersFile(t, e2eFamily{provider.FamilyAnthropicCompatible, `{"max_tokens":"256","anthropic_version":"2023-06-01"}`},
		map[string]string{"learn-provider": signalServer.URL + "/v1"})
	identity := profileTighteningIdentity(t, providersFile, "learn-provider")
	stateDir := t.TempDir()
	var out, errOut strings.Builder
	code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "learn-provider"), &out, &errOut)
	if code == exitSuccess {
		t.Fatalf("run must stop on the unsupported-option signal\nstderr:\n%s", errOut.String())
	}
	value := loadProfileValue(t, stateDir, identity, provider.FieldUnsupportedOptions)
	if value.Value != 1 || value.Provenance != provider.ProvenanceObserved {
		t.Fatalf("unsupported_options learned = %+v, want bit 0x1/observed", value)
	}
	if value.EvidenceRef.Kind != provider.EvidenceKindTask || !strings.HasPrefix(value.EvidenceRef.ID, "cli-") {
		t.Fatalf("bit row must carry the task evidence reference, got %+v", value.EvidenceRef)
	}
	if physical != 1 {
		t.Fatalf("physical requests = %d, want exactly 1 (learning never dispatches its own requests)", physical)
	}
}

// TestLearningRestartKeepsEvidenceAndPacesRetries: run#1 learns a 6s
// cooldown from Retry-After (no retry policy; the run stops on the 429). A
// SECOND run with the SAME providers file (identical config identity, fresh
// store open = restart) with retry enabled receives a 429 with a 2s
// Retry-After: the retry backoff takes the maximum of the selected backoff,
// the LEARNED cooldown and the governor window, so a ~6s gap proves the
// durable learned envelope paces the retry after a restart, dominating the
// 2s header. The durable value survives the restart unchanged.
func TestLearningRestartKeepsEvidenceAndPacesRetries(t *testing.T) {
	script := learningScript()
	// ONE server serves both runs on the same URL (same config identity).
	// Request behavior is indexed: request 1 = 429 with 6s Retry-After
	// (run#1 learns it), request 2 = 429 with 2s Retry-After (run#2 must
	// beat it), requests 3+ = the deterministic script (action, final).
	var mu sync.Mutex
	requests := 0
	var stamps []time.Time
	handler := func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests++
		count := requests
		stamps = append(stamps, time.Now())
		mu.Unlock()
		switch {
		case count == 1:
			response.Header().Set("Retry-After", "6")
			response.WriteHeader(http.StatusTooManyRequests)
		case count == 2:
			response.Header().Set("Retry-After", "2")
			response.WriteHeader(http.StatusTooManyRequests)
		default:
			index := (count - 3) % len(script)
			body := e2eWrapResponse(provider.FamilyOpenAICompatible, script[index])
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(body)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "learn-provider", server.URL+"/v1", 0)
	identity := profileTighteningIdentity(t, providersFile, "learn-provider")
	workspace := learningWorkspace(t)
	acceptance := learningAcceptance(t)
	stateDir := t.TempDir()
	args := monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "learn-provider")
	retryArgs := retryRunArgs(t, workspace, stateDir, acceptance, providersFile, "learn-provider")

	// Run#1 (no retry policy): stops on the 429 and persists the learned 6s.
	var out, errOut strings.Builder
	if code := run(context.Background(), args, &out, &errOut); code == exitSuccess {
		t.Fatalf("run#1 must stop on the 429\nstderr:\n%s", errOut.String())
	}
	if value := loadProfileValue(t, stateDir, identity, provider.FieldCooldownMillis); value.Value != 6000 || value.Provenance != provider.ProvenanceObserved {
		t.Fatalf("run#1 must learn cooldown=6000/observed, got %+v", value)
	}

	// Run#2 (restart, retry enabled): the 2s header is beaten by the learned
	// 6s envelope, so the inter-attempt gap must be ~6s.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), retryArgs, &out, &errOut); code != exitSuccess {
		t.Fatalf("run#2 exit = %d\nstderr:\n%s", code, errOut.String())
	}
	mu.Lock()
	totalRequests, copied := requests, append([]time.Time(nil), stamps...)
	mu.Unlock()
	if totalRequests != 4 {
		t.Fatalf("total physical requests = %d, want 4 (run#1: 1; run#2: 429, retry, second turn)", totalRequests)
	}
	if gap := copied[1].Sub(copied[0]); gap < 5500*time.Millisecond || gap > 60*time.Second {
		t.Fatalf("retry gap = %v, want ~6s (learned cooldown must dominate the 2s header after restart)", gap)
	}
	// The learned value survives the restart unchanged.
	if value := loadProfileValue(t, stateDir, identity, provider.FieldCooldownMillis); value.Value != 6000 || value.Provenance != provider.ProvenanceObserved {
		t.Fatalf("restart lost the learned cooldown: %+v", value)
	}
}

// TestLearningIdentityIsolation: evidence learned under one provider
// identity never leaks to another identity.
func TestLearningIdentityIsolation(t *testing.T) {
	script := learningScript()
	wireA := &e2eRetryWire{family: provider.FamilyOpenAICompatible, responses: script, firstFails: 1, status: http.StatusTooManyRequests, retryAfter: "5"}
	serverA := httptest.NewServer(wireA.handler())
	t.Cleanup(serverA.Close)
	wireB := newE2EWire(provider.FamilyAnthropicCompatible, script...)
	serverB := newHTTPServerForWire(wireB)
	t.Cleanup(serverB.Close)

	providersFile := writeProvidersFile(t,
		e2eFamily{provider.FamilyAnthropicCompatible, `{"max_tokens":"256","anthropic_version":"2023-06-01"}`},
		map[string]string{"iso-a": serverA.URL + "/v1", "iso-b": serverB.URL + "/v1"})
	identityA := profileTighteningIdentity(t, providersFile, "iso-a")
	identityB := profileTighteningIdentity(t, providersFile, "iso-b")
	stateDir := t.TempDir()

	var out, errOut strings.Builder
	// A learns a 5s cooldown (run stops on the 429).
	if code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "iso-a"), &out, &errOut); code == exitSuccess {
		t.Fatalf("iso-a run must stop on the 429\nstderr:\n%s", errOut.String())
	}
	if value := loadProfileValue(t, stateDir, identityA, provider.FieldCooldownMillis); value.Value != 5000 || value.Provenance != provider.ProvenanceObserved {
		t.Fatalf("identity A must have learned cooldown, got %+v", value)
	}
	// B runs successfully and must carry NO observed information.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), monotonicRunArgs(t, learningWorkspace(t), stateDir, learningAcceptance(t), providersFile, "iso-b"), &out, &errOut); code != exitSuccess {
		t.Fatalf("iso-b run exit = %d\nstderr:\n%s", code, errOut.String())
	}
	// B must own no observed evidence and no cooldown (A's learned wait must
	// never leak). Configured capability rows for B's own identity would be
	// legitimate operator declaration, so the check targets evidence.
	if value := loadProfileValue(t, stateDir, identityB, provider.FieldCooldownMillis); value.Known() {
		t.Fatalf("identity B inherited the cooldown from identity A: %+v", value)
	}
	if value := loadProfileValue(t, stateDir, identityB, provider.FieldMaxRequestBytes); value.Provenance == provider.ProvenanceObserved {
		t.Fatalf("identity B inherited observed evidence from identity A: %+v", value)
	}
}

// TestLearningCorruptProfileFailsClosed: corrupt profile state makes every
// subsequent run fail closed (never executed under unknown envelope state).
// The wire serves both runs (duplicated script), so the config identity and
// therefore the profile key are identical across runs.
func TestLearningCorruptProfileFailsClosed(t *testing.T) {
	script := learningScript()
	wire := newE2EWire(provider.FamilyOpenAICompatible, append(append([]string{}, script...), script...)...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "learn-provider", server.URL+"/v1", 8000)
	identity := profileTighteningIdentity(t, providersFile, "learn-provider")
	stateDir := t.TempDir()
	workspace := learningWorkspace(t)
	acceptance := learningAcceptance(t)
	args := monotonicRunArgs(t, workspace, stateDir, acceptance, providersFile, "learn-provider")

	var out, errOut strings.Builder
	if code := run(context.Background(), args, &out, &errOut); code != exitSuccess {
		t.Fatalf("run#1 exit = %d\nstderr:\n%s", code, errOut.String())
	}
	if value := loadProfileValue(t, stateDir, identity, provider.FieldMaxRequestBytes); value.Value != 8000 || value.Provenance != provider.ProvenanceConfigured {
		t.Fatalf("run#1 must persist configured bound, got %+v", value)
	}

	// Corrupt the persisted rows directly in SQLite (zero values are invalid
	// evidence, so every loader/validator must refuse them).
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE provider_operational_profiles SET value = 0 WHERE provider_id = ?`, state.Redact(identity.ProviderID)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Run#2 with the SAME configuration MUST fail closed before any
	// execution: corrupt profile state is never executed under.
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), args, &out, &errOut); code == exitSuccess {
		t.Fatalf("run#2 must fail closed on a corrupt operational profile")
	}
	if !strings.Contains(errOut.String(), "operational profile") {
		t.Fatalf("run#2 must report the operational profile failure, got:\n%s", errOut.String())
	}
	if wire.count() != 2 {
		t.Fatalf("run#2 dispatched %d requests, want 0 additional (fail closed before execution)", wire.count()-2)
	}
}

// TestLearningCorruptProfileFailsClosedOnResume: the same fail-closed
// guarantee holds on the resume path. A crashed task owns configured
// profile state; corrupting the rows directly in SQLite must make the
// resume fail closed BEFORE any execution (zero additional requests),
// because unknown envelope state is never executed under.
func TestLearningCorruptProfileFailsClosedOnResume(t *testing.T) {
	script := learningScript()
	wire := newE2EWire(provider.FamilyOpenAICompatible, script...)
	server := newHTTPServerForWire(wire)
	t.Cleanup(server.Close)
	providersFile := writeProvidersFileWithBounds(t, provider.FamilyOpenAICompatible, "learn-provider", server.URL+"/v1", 8000)
	identity := profileTighteningIdentity(t, providersFile, "learn-provider")
	stateDir := t.TempDir()
	acceptance := learningAcceptance(t)

	// Crash the run after its first durable provider attempt: the task
	// exists for resume and the configured bound is already persisted.
	args := monotonicRunArgs(t, learningWorkspace(t), stateDir, acceptance, providersFile, "learn-provider")
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
	if value := loadProfileValue(t, stateDir, identity, provider.FieldMaxRequestBytes); value.Value != 8000 || value.Provenance != provider.ProvenanceConfigured {
		t.Fatalf("crashed run must persist configured bound, got %+v", value)
	}

	// Corrupt the persisted rows directly in SQLite (zero values are invalid
	// evidence, so every loader/validator must refuse them).
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE provider_operational_profiles SET value = 0 WHERE provider_id = ?`, state.Redact(identity.ProviderID)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume with the SAME configuration MUST fail closed before any
	// execution: corrupt profile state is never executed under.
	var out, errOut strings.Builder
	resumeArgs := []string{
		"resume", taskID,
		"--state-dir", stateDir,
		"--providers", providersFile,
		"--provider-id", "learn-provider",
		"--acceptance", acceptance,
		"--log-level", "error",
	}
	if code := run(context.Background(), resumeArgs, &out, &errOut); code == exitSuccess {
		t.Fatalf("resume must fail closed on a corrupt operational profile")
	}
	if !strings.Contains(errOut.String(), "operational profile") {
		t.Fatalf("resume must report the operational profile failure, got:\n%s", errOut.String())
	}
	// The crashed run crashes before any physical dispatch and the resume
	// fails closed, so the wire must never have served a request.
	if wire.count() != 0 {
		t.Fatalf("requests served = %d, want 0 (crashed pre-dispatch; fail closed before execution)", wire.count())
	}
}

// TestLearningObserverPersistenceFailureStopsRunConservatively is covered at
// the agent seam (TestExecutorObserverErrorStopsRetryConservatively) and the
// CLI-level fail-closed profile load is covered by
// TestLearningCorruptProfileFailsClosed.
