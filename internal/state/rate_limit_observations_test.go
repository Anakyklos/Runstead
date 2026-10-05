package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type observedRateLimitClient struct{ response provider.Response }

func (c observedRateLimitClient) RouteSafety() provider.RouteSafety {
	return provider.SafeRouteSafety()
}

func (c observedRateLimitClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	return c.response, errors.New("PRIVATE_HEADER_SECRET PRIVATE_BODY_SECRET")
}

func TestPersistRateLimitObservationDropsInvalidValuesIndependently(t *testing.T) {
	maxAllowed := int64(1_000_000_000)
	zero, negative := int64(0), int64(-1)
	maxPlusOne := maxAllowed + 1
	recordedAt := time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
	got := persistRateLimitObservation(governor.ProviderFinished{
		StatusCode:      700,
		ObservedResetAt: time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		RateLimitObservation: provider.RateLimitObservation{
			ObservedRetryAfter: 30*24*time.Hour + time.Nanosecond,
			LimitRequests:      &maxAllowed,
			RemainingRequests:  &negative,
			ResetRequests:      31 * 24 * time.Hour,
			LimitTokens:        &maxPlusOne,
			RemainingTokens:    &zero,
			ResetTokens:        30 * 24 * time.Hour,
		},
	}, recordedAt)
	if got.statusCode != nil || got.observedRetryAfterNS != nil || got.remainingRequests != nil ||
		got.resetRequestsNS != nil || got.limitTokens != nil || got.observedResetAt != nil {
		t.Fatalf("invalid status/request values survived persistence validation: %#v", got)
	}
	if got.limitRequests != maxAllowed || got.remainingTokens != zero || got.resetTokensNS != int64(30*24*time.Hour) {
		t.Fatalf("valid counter/duration values were lost or altered: %#v", got)
	}
}

func TestPersistRateLimitObservationKeepsResetAtWithinThirtyDays(t *testing.T) {
	recordedAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"upper bound", recordedAt.Add(30 * 24 * time.Hour), "2026-01-31T00:00:00Z"},
		{"lower bound", recordedAt.Add(-30 * 24 * time.Hour), "2025-12-02T00:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := persistRateLimitObservation(governor.ProviderFinished{ObservedResetAt: test.at}, recordedAt)
			if got.observedResetAt != test.want {
				t.Fatalf("bounded reset timestamp = %#v, want %q", got.observedResetAt, test.want)
			}
		})
	}
	tooFar := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	got := persistRateLimitObservation(governor.ProviderFinished{ObservedResetAt: tooFar}, recordedAt)
	if got.observedResetAt != nil {
		t.Fatalf("year-9999 reset timestamp survived the persistence bound: %#v", got.observedResetAt)
	}
}

func TestRateLimitObservationPersistsAndRendersSeparatelyFromGovernorDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	mustGovernorTask(t, store)
	limitRequests, remainingRequests := int64(1200), int64(73)
	limitTokens, remainingTokens := int64(250000), int64(89000)
	resetAt := time.Now().UTC().Add(2 * time.Minute)
	response := provider.Response{
		Text: "PRIVATE_BODY_SECRET",
		Metadata: provider.ResponseMetadata{
			StatusCode:    429,
			RetryAfter:    29 * time.Second,
			ResetAt:       resetAt,
			DeliveryState: provider.DeliveryCompleted,
			RateLimitObservation: provider.RateLimitObservation{
				ObservedRetryAfter: 29 * time.Second,
				LimitRequests:      &limitRequests,
				RemainingRequests:  &remainingRequests,
				ResetRequests:      29 * time.Second,
				LimitTokens:        &limitTokens,
				RemainingTokens:    &remainingTokens,
				ResetTokens:        58 * time.Second,
			},
		},
	}
	accountGovernor := newGovernor(t, store, nil, 80)
	result := accountGovernor.Execute(context.Background(), governor.AttemptRequest{
		TaskID: "task-1", ClientRequestID: "task-1-observed-rate",
		ProviderRequest: provider.Request{Prompt: "prompt", Model: "scripted"},
	}, observedRateLimitClient{response: response}, func(response provider.Response, _ error) governor.Outcome {
		return governor.Outcome{
			Class: governor.OutcomeRateCapacity, RetryAfter: response.Metadata.RetryAfter,
			ResetAt: response.Metadata.ResetAt, UpstreamReached: true,
			DeliveryState:        response.Metadata.DeliveryState,
			ProviderFailureClass: provider.FailureRateCapacity,
		}
	})
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("completion outcome = %q, want rate_or_capacity", result.Completion.Outcome)
	}
	if delta := 2*time.Minute - result.Completion.SelectedBackoff; delta < -time.Second || delta > time.Second {
		t.Fatalf("selected backoff = %s, want approximately 2m from the observed absolute reset", result.Completion.SelectedBackoff)
	}
	selectedBackoffNS := int64(result.Completion.SelectedBackoff)

	var statusCode, selectedBackoff, observedRetryAfter, limitReq, remainingReq, resetReq, limitTok, remainingTok, resetTok sql.NullInt64
	var observedReset sql.NullString
	var outcome, failureClass, receiptError string
	if err := store.db.QueryRow(`SELECT status_code, observed_reset_at, observed_retry_after_ns,
		limit_requests, remaining_requests, reset_requests_ns, limit_tokens, remaining_tokens, reset_tokens_ns,
		outcome, selected_backoff_ns, provider_failure_class, error_class FROM provider_attempts
		WHERE task_id = 'task-1' AND client_request_id = 'task-1-observed-rate'`).Scan(
		&statusCode, &observedReset, &observedRetryAfter, &limitReq, &remainingReq, &resetReq, &limitTok,
		&remainingTok, &resetTok, &outcome, &selectedBackoff, &failureClass, &receiptError); err != nil {
		t.Fatalf("read persisted provider outcome: %v", err)
	}
	if !statusCode.Valid || statusCode.Int64 != 429 || !observedReset.Valid || !observedRetryAfter.Valid || observedRetryAfter.Int64 != int64(29*time.Second) {
		t.Fatalf("status/reset/retry observations = %#v %#v %#v", statusCode, observedReset, observedRetryAfter)
	}
	if !limitReq.Valid || limitReq.Int64 != limitRequests || !remainingReq.Valid || remainingReq.Int64 != remainingRequests ||
		!resetReq.Valid || resetReq.Int64 != int64(29*time.Second) || !limitTok.Valid || limitTok.Int64 != limitTokens ||
		!remainingTok.Valid || remainingTok.Int64 != remainingTokens || !resetTok.Valid || resetTok.Int64 != int64(58*time.Second) {
		t.Fatalf("persisted request/token observations are incomplete: req=%#v/%#v/%#v tokens=%#v/%#v/%#v",
			limitReq, remainingReq, resetReq, limitTok, remainingTok, resetTok)
	}
	if outcome != string(governor.OutcomeRateCapacity) || !selectedBackoff.Valid || selectedBackoff.Int64 != selectedBackoffNS ||
		failureClass != string(provider.FailureRateCapacity) || receiptError != "" {
		t.Fatalf("governor outcome and failure classifications merged: outcome=%q backoff=%#v failure=%q receipt_error=%q",
			outcome, selectedBackoff, failureClass, receiptError)
	}
	var payload string
	if err := store.db.QueryRow(`SELECT payload_json FROM events WHERE task_id = 'task-1' AND kind = 'provider_attempt_failed'`).Scan(&payload); err != nil {
		t.Fatalf("read TX2 event: %v", err)
	}
	for _, want := range []string{`"status_code":429`, `"observed_retry_after_ns":29000000000`, fmt.Sprintf(`"selected_backoff":%d`, selectedBackoffNS), `"remaining_tokens":89000`} {
		if !strings.Contains(payload, want) {
			t.Errorf("TX2 event missing %s: %s", want, payload)
		}
	}
	for _, secret := range []string{"PRIVATE_HEADER_SECRET", "PRIVATE_BODY_SECRET"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("TX2 event contains raw provider text %q", secret)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	var rendered strings.Builder
	if err := reopened.RenderInspect(context.Background(), &rendered, "task-1"); err != nil {
		t.Fatalf("RenderInspect() after reopen: %v", err)
	}
	text := rendered.String()
	for _, want := range []string{
		"status=failed", "delivery_state=completed", "outcome=rate_or_capacity", "provider_failure_class=rate_or_capacity", "selected_backoff=" + result.Completion.SelectedBackoff.String(),
		"rate_limit_observation: status_code=429", "reset_at=" + formatTime(resetAt), "observed_retry_after=29s", "limit_requests=1200", "remaining_requests=73",
		"reset_requests=29s", "limit_tokens=250000", "remaining_tokens=89000", "reset_tokens=58s",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reopened inspect missing %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"PRIVATE_HEADER_SECRET", "PRIVATE_BODY_SECRET"} {
		if strings.Contains(text, secret) {
			t.Fatalf("inspect contains raw provider text %q", secret)
		}
	}
}

func TestPersistenceNullsOutOfBoundRateLimitValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstead.db")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	mustGovernorTask(t, store)
	state := governor.PersistedState{AccountPolicyID: "policy-test", ProviderID: "scripted"}
	if err := store.RecordProviderPrepared(context.Background(), governor.ProviderPrepared{
		TaskID: "task-1", ClientRequestID: "task-1-out-of-bounds", ProviderID: "scripted",
		ModelPool: "pool", Model: "scripted", AttemptSequence: 1, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderPrepared() error = %v", err)
	}
	const maxAllowed int64 = 1_000_000_000
	maxPlusOne, maxInt := maxAllowed+1, int64(1<<63-1)
	zero := int64(0)
	if err := store.RecordProviderFinished(context.Background(), governor.ProviderFinished{
		TaskID: "task-1", ClientRequestID: "task-1-out-of-bounds", Outcome: governor.OutcomeRateCapacity,
		ObservedResetAt: time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		RateLimitObservation: provider.RateLimitObservation{
			LimitRequests:     &maxPlusOne,
			RemainingRequests: &zero,
			LimitTokens:       &maxInt,
		},
		AttemptDebited: 1, SelectedBackoff: 29 * time.Second, State: state,
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
	var resetAt sql.NullString
	var limitRequests, remainingRequests, limitTokens, selectedBackoff sql.NullInt64
	if err := store.db.QueryRow(`SELECT observed_reset_at, limit_requests, remaining_requests, limit_tokens, selected_backoff_ns
		FROM provider_attempts WHERE task_id = 'task-1' AND client_request_id = 'task-1-out-of-bounds'`).Scan(
		&resetAt, &limitRequests, &remainingRequests, &limitTokens, &selectedBackoff); err != nil {
		t.Fatalf("read persisted out-of-bounds observations: %v", err)
	}
	if resetAt.Valid || limitRequests.Valid || limitTokens.Valid {
		t.Fatalf("out-of-bounds provider evidence was persisted instead of NULL: reset=%#v limits=%#v/%#v", resetAt, limitRequests, limitTokens)
	}
	if !remainingRequests.Valid || remainingRequests.Int64 != 0 {
		t.Fatalf("observed remaining=0 did not remain valid: %#v", remainingRequests)
	}
	if !selectedBackoff.Valid || selectedBackoff.Int64 != int64(29*time.Second) {
		t.Fatalf("outcome accounting/backoff changed while dropping diagnostics: %#v", selectedBackoff)
	}
}

// TestRateLimitObservationMigrationPreservesLegacyAttempts exercises a real
// schema-17 to current-schema upgrade with an existing provider attempt. The
// new evidence columns are nullable so old attempts remain explicitly absent.
func TestRateLimitObservationMigrationPreservesLegacyAttempts(t *testing.T) {
	legacyEntries := make(map[int]string)
	for version := 1; version <= 17; version++ {
		matches, err := fs.Glob(migrationFS, "migrations/*.sql")
		if err != nil {
			t.Fatalf("list embedded migrations: %v", err)
		}
		var migration string
		for _, name := range matches {
			got, parseErr := migrationVersion(name)
			if parseErr != nil {
				t.Fatalf("parse migration %q: %v", name, parseErr)
			}
			if got == version {
				data, readErr := fs.ReadFile(migrationFS, name)
				if readErr != nil {
					t.Fatalf("read migration %q: %v", name, readErr)
				}
				migration = string(data)
				break
			}
		}
		if migration == "" {
			t.Fatalf("legacy migration %d not found", version)
		}
		legacyEntries[version] = migration
	}

	legacyFiles := testMigrations(t, legacyEntries)
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if err := migrateFS(db, legacyFiles); err != nil {
		t.Fatalf("migrate legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks
		(task_id, objective, status, workspace, model, config_json, created_at, started_at)
		VALUES ('task-legacy', 'legacy', 'running', '/tmp', 'model', '{}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy task: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO provider_attempts
		(execution_id, task_id, client_request_id, status, outcome, created_at, prepared_at, completed_at)
		VALUES ('exec-legacy', 'task-legacy', 'req-legacy', 'completed', 'rate_or_capacity', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z')`); err != nil {
		t.Fatalf("insert legacy provider attempt: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(Options{Path: legacyPath})
	if err != nil {
		t.Fatalf("upgrade legacy database: %v", err)
	}
	defer store.Close()
	var nullCount int
	if err := store.db.QueryRow(`SELECT
		(status_code IS NULL) + (observed_reset_at IS NULL) + (observed_retry_after_ns IS NULL) +
		(limit_requests IS NULL) + (remaining_requests IS NULL) + (reset_requests_ns IS NULL) +
		(limit_tokens IS NULL) + (remaining_tokens IS NULL) + (reset_tokens_ns IS NULL)
		FROM provider_attempts WHERE execution_id = 'exec-legacy'`).Scan(&nullCount); err != nil {
		t.Fatalf("read legacy observation columns: %v", err)
	}
	if nullCount != 9 {
		t.Fatalf("legacy observation columns null count = %d, want 9", nullCount)
	}
	var rendered strings.Builder
	if err := store.RenderInspect(context.Background(), &rendered, "task-legacy"); err != nil {
		t.Fatalf("RenderInspect() error = %v", err)
	}
	if strings.Contains(rendered.String(), "rate_limit_observation=") || strings.Contains(rendered.String(), "status_code=") {
		t.Fatalf("legacy attempt must render observation as absent/unknown, got:\n%s", rendered.String())
	}
}
