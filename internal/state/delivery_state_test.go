package state

import (
	"context"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestProviderAttemptsHaveDeliveryStateColumn(t *testing.T) {
	store := openTestStore(t)
	rows, err := store.db.Query(`PRAGMA table_info(provider_attempts)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "delivery_state" {
			found = true
			if columnType != "TEXT" || notNull != 1 || primaryKey != 0 {
				t.Fatalf("delivery_state schema = type %q notNull %d pk %d", columnType, notNull, primaryKey)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("provider_attempts.delivery_state is missing")
	}
}

func TestProviderAttemptPersistsObservedAndUnobservedDeliveryState(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")

	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-1", 1)
	assertProviderDeliveryState(t, store, "task-1", "request-1", "")
	finishProviderAttemptForDelivery(t, store, "task-1", "request-1", provider.DeliveryCompleted)
	assertProviderDeliveryState(t, store, "task-1", "request-1", "completed")

	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-2", 2)
	finishProviderAttemptForDelivery(t, store, "task-1", "request-2", provider.DeliveryState(0))
	assertProviderDeliveryState(t, store, "task-1", "request-2", "")

	var payload string
	if err := store.db.QueryRow(`
		SELECT payload_json FROM events
		WHERE task_id = ? ORDER BY sequence DESC LIMIT 1`, "task-1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"delivery_state":"unobserved"`) {
		t.Fatalf("last provider event missing sanitized unobserved delivery state: %s", payload)
	}
	for _, forbidden := range []string{"prompt", "authorization", "response_body", "raw_error"} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Fatalf("provider event leaks %q: %s", forbidden, payload)
		}
	}
}

func TestProviderTx1CrashLeavesPreparedDeliveryUnobserved(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-1", 1)
	assertProviderDeliveryState(t, store, "task-1", "request-1", "")
}

func TestProviderDeliveryStateIsRenderedAndLoadedForRecovery(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-1", 1)
	finishProviderAttemptForDelivery(t, store, "task-1", "request-1", provider.DeliveryCompleted)
	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-2", 2)

	output := renderForTest(t, store, "task-1")
	if !strings.Contains(output, "delivery_state=completed") {
		t.Fatalf("inspect output missing completed delivery state:\n%s", output)
	}
	if !strings.Contains(output, "delivery_state=unobserved") {
		t.Fatalf("inspect output missing prepared unobserved delivery state:\n%s", output)
	}

	snapshot, err := store.LoadRecoverySnapshot(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ProviderAttempts) != 2 {
		t.Fatalf("provider attempts = %d, want 2", len(snapshot.ProviderAttempts))
	}
	if snapshot.ProviderAttempts[0].DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("completed recovery delivery state = %v, want completed", snapshot.ProviderAttempts[0].DeliveryState)
	}
	if snapshot.ProviderAttempts[1].DeliveryState != (provider.DeliveryState(0)) {
		t.Fatalf("prepared recovery delivery state = %v, want unobserved zero", snapshot.ProviderAttempts[1].DeliveryState)
	}
}

func TestProviderDeliveryStateConstraintRejectsUnknownValue(t *testing.T) {
	store := openTestStore(t)
	mustTask(t, store, "task-1")
	mustProviderAttemptPreparedOnly(t, store, "task-1", "request-1", 1)
	if _, err := store.db.Exec(`UPDATE provider_attempts SET delivery_state = 'invented' WHERE task_id = ?`, "task-1"); err == nil {
		t.Fatal("delivery_state constraint accepted an unknown value")
	}
}

func assertProviderDeliveryState(t *testing.T, store *Store, taskID, requestID, want string) {
	t.Helper()
	var delivery string
	if err := store.db.QueryRow(`
		SELECT delivery_state FROM provider_attempts
		WHERE task_id = ? AND client_request_id = ?`, taskID, requestID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if delivery != want {
		t.Fatalf("provider delivery_state = %q, want %q", delivery, want)
	}
}

func finishProviderAttemptForDelivery(t *testing.T, store *Store, taskID, requestID string, delivery provider.DeliveryState) {
	t.Helper()
	now := newFixedClock().Now()
	if err := store.RecordProviderFinished(context.Background(), governor.ProviderFinished{
		TaskID: taskID, ClientRequestID: requestID, Outcome: governor.OutcomeSuccess,
		UpstreamReached: true, DeliveryState: delivery, AttemptDebited: 1,
		Circuit: governor.CircuitSnapshot{State: governor.CircuitClosed},
		State: governor.PersistedState{
			AccountPolicyID: "runstead-cli", ProviderID: "scripted", ModelPool: "instant", Model: "scripted",
			AllowanceProfile: governor.ProfileInstant, NextAttempt: 3,
			Circuit:       governor.CircuitSnapshot{State: governor.CircuitClosed},
			Ceilings:      governor.BudgetCeilings{Rolling3h: 140, Rolling1h: 80, Rolling10m: 25, TaskBudget: 80, RetryBudget: 2},
			RollingEvents: []governor.LedgerEvent{{At: now, TaskID: taskID}},
			TaskStates:    []governor.TaskStateRecord{{TaskID: taskID, Attempts: 3, LastTouched: now}},
		},
	}); err != nil {
		t.Fatalf("RecordProviderFinished() error = %v", err)
	}
}
