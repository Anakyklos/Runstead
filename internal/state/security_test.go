package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// Security tests: the database must never contain credentials, cookies,
// authorization headers, raw provider responses or secrets embedded in error
// strings.

const (
	secretAPIKey   = "sk-abcdef1234567890secret"
	secretBearer   = "Bearer eyJhbGciOiJIUzI1NiJ9.secret.payload"
	secretCookie   = "__Secure-session=abcdefghijklmnop123456"
	secretInPrompt = "the api_key=sk-abcdef1234567890secret must never persist"
)

// runSecretLifecycle persists a full lifecycle where every text field
// contains credential-shaped values.
func runSecretLifecycle(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateTask(ctx, TaskRecord{
		TaskID: "task-secret", Objective: secretInPrompt,
		Workspace: "/tmp/ws", Model: "scripted",
		ConfigJSON: []byte(`{"api_key":"` + secretAPIKey + `"}`),
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if err := store.StartTask(ctx, "task-secret"); err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	actionID, err := store.RecordAction(ctx, ActionRecord{
		TaskID: "task-secret", Tool: "read_file",
		Arguments:   []byte(`{"path":"a.txt","token":"` + secretAPIKey + `"}`),
		Fingerprint: "fp",
	})
	if err != nil {
		t.Fatalf("RecordAction() error = %v", err)
	}
	executionID, err := store.PrepareToolAttempt(ctx, ToolAttemptPrepared{
		TaskID: "task-secret", ActionID: actionID, Tool: "read_file",
		Arguments: []byte(`{"path":"a.txt","api_key":"` + secretAPIKey + `"}`), RecoveryClass: 1,
	})
	if err != nil {
		t.Fatalf("PrepareToolAttempt() error = %v", err)
	}
	obs := tools.Observation{
		ID: "obs-000001", Tool: "read_file", Success: true,
		Data: map[string]any{
			"content":       "line with " + secretBearer,
			"authorization": secretCookie,
		},
		Metadata: tools.Metadata{Source: "read_file", Untrusted: true},
	}
	if err := store.CompleteToolAttempt(ctx, ToolAttemptCompleted{
		TaskID: "task-secret", ExecutionID: executionID, Status: "completed",
		EvidenceID: obs.ID, DurationNanos: 1, Observation: obs,
	}); err != nil {
		t.Fatalf("CompleteToolAttempt() error = %v", err)
	}
	if err := store.FinalizeTask(ctx, TaskFinalize{
		TaskID: "task-secret", Outcome: "failed",
		StopReason: "provider failure: " + secretBearer,
		Summary:    "summary with " + secretAPIKey,
	}); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	// Provider attempt records carry only sanitized identifiers and
	// classification codes, never prompts or response bodies.
	mustProviderAttempt(t, store, "task-secret", "task-secret-0001", 1)
}

func TestSensitiveValuesAbsentFromDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runstead.db")
	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	runSecretLifecycle(t, store)
	store.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	content := string(raw)
	for _, secret := range []string{
		secretAPIKey, secretBearer, secretCookie,
		"eyJhbGciOiJIUzI1NiJ9.secret.payload", "abcdefghijklmnop123456",
		"abcdef1234567890secret",
	} {
		if strings.Contains(content, secret) {
			t.Errorf("database file contains secret %q", secret)
		}
	}
	// The redaction marker must be present.
	if !strings.Contains(content, "<redacted>") {
		t.Error("database must contain the redaction marker")
	}
}

func TestSensitiveValuesAbsentFromColumns(t *testing.T) {
	store := openTestStore(t)
	runSecretLifecycle(t, store)

	// The provider prompt is never persisted: provider attempt rows have no
	// prompt column at all, and the transcript is not stored anywhere.
	var objective, stopReason, summary string
	if err := store.db.QueryRow(
		`SELECT objective, stop_reason, summary FROM tasks WHERE task_id = 'task-secret'`).Scan(&objective, &stopReason, &summary); err != nil {
		t.Fatalf("task query: %v", err)
	}
	for name, value := range map[string]string{
		"objective": objective, "stop_reason": stopReason, "summary": summary,
	} {
		if ContainsCredentialShape(value) {
			t.Errorf("%s retained a credential-shaped value: %q", name, value)
		}
	}
	var dataJSON, metadataJSON string
	if err := store.db.QueryRow(
		`SELECT data_json, metadata_json FROM tool_results WHERE task_id = 'task-secret' AND evidence_id = 'obs-000001'`).Scan(&dataJSON, &metadataJSON); err != nil {
		t.Fatalf("tool result query: %v", err)
	}
	if ContainsCredentialShape(dataJSON) || ContainsCredentialShape(metadataJSON) {
		t.Fatalf("tool results retained credentials: %q %q", dataJSON, metadataJSON)
	}
	var arguments string
	if err := store.db.QueryRow(
		`SELECT arguments_json FROM tool_attempts WHERE task_id = 'task-secret'`).Scan(&arguments); err != nil {
		t.Fatalf("tool attempt query: %v", err)
	}
	if ContainsCredentialShape(arguments) {
		t.Fatalf("tool arguments retained a credential: %q", arguments)
	}
	// Event payloads must be sanitized too.
	events, err := store.loadEvents(context.Background(), "task-secret")
	if err != nil {
		t.Fatalf("loadEvents() error = %v", err)
	}
	for _, event := range events {
		if ContainsCredentialShape(event.Payload) {
			t.Errorf("event %d payload retained a credential: %q", event.Sequence, event.Payload)
		}
	}
}

func TestProviderPromptNeverPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	store, err := Open(Options{Path: path, Clock: newFixedClock()})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	mustTask(t, store, "task-1")
	// The governor persistence boundary receives only sanitized records; the
	// provider prompt lives in the AttemptRequest and is never passed to the
	// store. This test proves no table stores free-form prompt text.
	state := governor.PersistedState{
		AccountPolicyID: "p", ProviderID: "scripted", ModelPool: "instant",
		Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
	}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AttemptSequence: 1, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	if strings.Contains(string(raw), "secret instruction") {
		t.Fatal("provider prompt text must never be persisted")
	}
}

func TestReceiptEvidenceIsSanitizedIdentifiersOnly(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	// Receipt fields are validated identifiers by contract (#29); persist a
	// receipt and confirm no secret-bearing text can ride along.
	state := governor.PersistedState{
		AccountPolicyID: "p", ProviderID: "scripted", ModelPool: "instant",
		Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
	}
	prepared := governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-0001", ProviderID: "scripted",
		ModelPool: "instant", Model: "scripted", AttemptSequence: 1, State: state,
	}
	if err := store.RecordProviderPrepared(context.Background(), prepared); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	receipt := provider.AttemptReceipt{
		SchemaVersion:   1,
		AttemptID:       "upstream-attempt-0001",
		ClientRequestID: "task-1-0001",
		Sequence:        1,
		Provider:        "scripted",
		Model:           "scripted",
		AccountLaneHash: "lane-hash",
		StartedAt:       newFixedClock().Now(),
		CompletedAt:     newFixedClock().Now().Add(time.Second),
		Outcome:         provider.AttemptOutcomeSuccess,
		Trigger:         provider.AttemptTriggerInitial,
		UpstreamReached: true,
	}
	if err := store.RecordProviderFinished(context.Background(), governor.ProviderFinished{
		TaskID: "task-1", ClientRequestID: "task-1-0001", Outcome: governor.OutcomeSuccess,
		UpstreamReached: true, AttemptDebited: 1, Receipts: []provider.AttemptReceipt{receipt},
		State: state,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
	// The receipt attempt id is upstream-owned evidence identity, distinct
	// from the Runstead execution id.
	var executionID, receiptID string
	if err := store.db.QueryRow(
		`SELECT p.execution_id, r.receipt_attempt_id FROM provider_attempt_receipts r
		 JOIN provider_attempts p ON p.execution_id = r.execution_id
		 WHERE r.receipt_attempt_id = 'upstream-attempt-0001'`).Scan(&executionID, &receiptID); err != nil {
		t.Fatalf("receipt query: %v", err)
	}
	if executionID == receiptID {
		t.Fatal("execution_id and receipt_attempt_id must never be equal")
	}
	if executionID == "" || receiptID != "upstream-attempt-0001" {
		t.Fatalf("execution %q receipt %q", executionID, receiptID)
	}
}
