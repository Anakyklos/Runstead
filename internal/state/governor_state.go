package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

// saveGovernorStateInTx upserts the account protection projection inside the
// caller's transaction so projection rows and journal events commit
// atomically. The task_id attributes the event scope for the journal.
func (s *Store) saveGovernorStateInTx(ctx context.Context, tx *sql.Tx, state governor.PersistedState, taskID string, updatedAt string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO governor_state
		 (id, account_policy_id, provider_id, model_pool, model, allowance_profile, next_attempt, last_start,
		  cooldown_until, circuit_state, circuit_reason, circuit_open_until, circuit_refresh_required,
		  circuit_last_rate_reset, telemetry_available, telemetry_reset_at, telemetry_cooldown_until,
		  telemetry_rate_limited, telemetry_capacity_exhausted, telemetry_upstream_circuit, telemetry_unsafe,
		  rolling_3h_ceiling, rolling_1h_ceiling, rolling_10m_ceiling, task_budget_ceiling, retry_budget_ceiling, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   account_policy_id = excluded.account_policy_id,
		   provider_id = excluded.provider_id,
		   model_pool = excluded.model_pool,
		   model = excluded.model,
		   allowance_profile = excluded.allowance_profile,
		   next_attempt = excluded.next_attempt,
		   last_start = excluded.last_start,
		   cooldown_until = excluded.cooldown_until,
		   circuit_state = excluded.circuit_state,
		   circuit_reason = excluded.circuit_reason,
		   circuit_open_until = excluded.circuit_open_until,
		   circuit_refresh_required = excluded.circuit_refresh_required,
		   circuit_last_rate_reset = excluded.circuit_last_rate_reset,
		   telemetry_available = excluded.telemetry_available,
		   telemetry_reset_at = excluded.telemetry_reset_at,
		   telemetry_cooldown_until = excluded.telemetry_cooldown_until,
		   telemetry_rate_limited = excluded.telemetry_rate_limited,
		   telemetry_capacity_exhausted = excluded.telemetry_capacity_exhausted,
		   telemetry_upstream_circuit = excluded.telemetry_upstream_circuit,
		   telemetry_unsafe = excluded.telemetry_unsafe,
		   rolling_3h_ceiling = excluded.rolling_3h_ceiling,
		   rolling_1h_ceiling = excluded.rolling_1h_ceiling,
		   rolling_10m_ceiling = excluded.rolling_10m_ceiling,
		   task_budget_ceiling = excluded.task_budget_ceiling,
		   retry_budget_ceiling = excluded.retry_budget_ceiling,
		   updated_at = excluded.updated_at`,
		state.AccountPolicyID, state.ProviderID, state.ModelPool, state.Model,
		string(state.AllowanceProfile), state.NextAttempt, formatTime(state.LastStart), formatTime(state.CooldownUntil),
		string(state.Circuit.State), string(state.Circuit.Reason), formatTime(state.Circuit.OpenUntil),
		boolInt(state.Circuit.RefreshRequired), formatTime(state.LastRateReset),
		nullableInt(state.Telemetry.Available), formatTime(state.Telemetry.ResetAt), formatTime(state.Telemetry.CooldownUntil),
		boolInt(state.Telemetry.RateLimited), boolInt(state.Telemetry.CapacityExhausted),
		string(state.Telemetry.UpstreamCircuit), boolInt(state.Telemetry.Unsafe),
		state.Ceilings.Rolling3h, state.Ceilings.Rolling1h, state.Ceilings.Rolling10m,
		state.Ceilings.TaskBudget, state.Ceilings.RetryBudget, updatedAt); err != nil {
		return fmt.Errorf("save governor state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM governor_ledger`); err != nil {
		return fmt.Errorf("reset governor ledger: %w", err)
	}
	for _, event := range state.RollingEvents {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO governor_ledger (at, task_id) VALUES (?, ?)`,
			formatTime(event.At), event.TaskID); err != nil {
			return fmt.Errorf("save governor ledger entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM governor_task_states`); err != nil {
		return fmt.Errorf("reset governor task states: %w", err)
	}
	for _, record := range state.TaskStates {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO governor_task_states (task_id, attempts, retries, last_touched) VALUES (?, ?, ?, ?)`,
			record.TaskID, record.Attempts, record.Retries, formatTime(record.LastTouched)); err != nil {
			return fmt.Errorf("save governor task state: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM governor_request_records`); err != nil {
		return fmt.Errorf("reset governor request records: %w", err)
	}
	for _, record := range state.RequestRecords {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO governor_request_records (request_id, state, completed_at) VALUES (?, ?, ?)`,
			record.RequestID, record.State, formatTime(record.CompletedAt)); err != nil {
			return fmt.Errorf("save governor request record: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM governor_attempt_ids`); err != nil {
		return fmt.Errorf("reset governor attempt ids: %w", err)
	}
	for _, record := range state.AttemptIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO governor_attempt_ids (attempt_id, seen_at) VALUES (?, ?)`,
			record.AttemptID, formatTime(record.SeenAt)); err != nil {
			return fmt.Errorf("save governor attempt id: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM governor_rate_events`); err != nil {
		return fmt.Errorf("reset governor rate events: %w", err)
	}
	for _, at := range state.RateEvents {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO governor_rate_events (at) VALUES (?)`,
			formatTime(at)); err != nil {
			return fmt.Errorf("save governor rate event: %w", err)
		}
	}
	return nil
}

// queryer abstracts *sql.DB and *sql.Tx so governor projections can be loaded
// either directly (restart restore) or inside a transaction (recovery
// reconciliation keeps the projection and journal atomic).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// GovernorState loads the persisted account protection projection, if any.
func (s *Store) GovernorState(ctx context.Context) (governor.PersistedState, bool, error) {
	return loadGovernorState(ctx, s.db)
}

func loadGovernorState(ctx context.Context, q queryer) (governor.PersistedState, bool, error) {
	var (
		state              governor.PersistedState
		circuitState       string
		circuitReason      string
		allowanceProfile   string
		upstreamCircuit    string
		telemetryAvailable sql.NullInt64
		lastStart          string
		cooldownUntil      string
		circuitOpenUntil   string
		lastRateReset      string
		telemetryResetAt   string
		telemetryCooldown  string
	)
	err := q.QueryRowContext(ctx,
		`SELECT account_policy_id, provider_id, model_pool, model, allowance_profile, next_attempt,
		        last_start, cooldown_until, circuit_state, circuit_reason, circuit_open_until,
		        circuit_refresh_required, circuit_last_rate_reset, telemetry_available,
		        telemetry_reset_at, telemetry_cooldown_until, telemetry_rate_limited,
		        telemetry_capacity_exhausted, telemetry_upstream_circuit, telemetry_unsafe,
		        rolling_3h_ceiling, rolling_1h_ceiling, rolling_10m_ceiling, task_budget_ceiling, retry_budget_ceiling
		 FROM governor_state WHERE id = 1`).Scan(
		&state.AccountPolicyID, &state.ProviderID, &state.ModelPool, &state.Model, &allowanceProfile,
		&state.NextAttempt, &lastStart, &cooldownUntil, &circuitState, &circuitReason, &circuitOpenUntil,
		&state.Circuit.RefreshRequired, &lastRateReset, &telemetryAvailable,
		&telemetryResetAt, &telemetryCooldown, &state.Telemetry.RateLimited,
		&state.Telemetry.CapacityExhausted, &upstreamCircuit, &state.Telemetry.Unsafe,
		&state.Ceilings.Rolling3h, &state.Ceilings.Rolling1h, &state.Ceilings.Rolling10m,
		&state.Ceilings.TaskBudget, &state.Ceilings.RetryBudget)
	if err == sql.ErrNoRows {
		return governor.PersistedState{}, false, nil
	}
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor state: %w", err)
	}
	state.AllowanceProfile = governor.AllowanceProfile(allowanceProfile)
	state.Circuit.State = governor.CircuitState(circuitState)
	state.Circuit.Reason = governor.OutcomeClass(circuitReason)
	state.Circuit.OpenUntil = parseTime(circuitOpenUntil)
	state.LastStart = parseTime(lastStart)
	state.CooldownUntil = parseTime(cooldownUntil)
	state.LastRateReset = parseTime(lastRateReset)
	state.Telemetry.ResetAt = parseTime(telemetryResetAt)
	state.Telemetry.CooldownUntil = parseTime(telemetryCooldown)
	state.Telemetry.UpstreamCircuit = governor.UpstreamCircuitState(upstreamCircuit)
	if telemetryAvailable.Valid {
		value := int(telemetryAvailable.Int64)
		state.Telemetry.Available = &value
	}

	rows, err := q.QueryContext(ctx,
		`SELECT at, task_id FROM governor_ledger ORDER BY id`)
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor ledger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event governor.LedgerEvent
		var at string
		if err := rows.Scan(&at, &event.TaskID); err != nil {
			return governor.PersistedState{}, false, fmt.Errorf("scan governor ledger: %w", err)
		}
		event.At = parseTime(at)
		state.RollingEvents = append(state.RollingEvents, event)
	}
	if err := rows.Err(); err != nil {
		return governor.PersistedState{}, false, err
	}

	rows, err = q.QueryContext(ctx,
		`SELECT task_id, attempts, retries, last_touched FROM governor_task_states ORDER BY task_id`)
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor task states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var record governor.TaskStateRecord
		var lastTouched string
		if err := rows.Scan(&record.TaskID, &record.Attempts, &record.Retries, &lastTouched); err != nil {
			return governor.PersistedState{}, false, fmt.Errorf("scan governor task state: %w", err)
		}
		record.LastTouched = parseTime(lastTouched)
		state.TaskStates = append(state.TaskStates, record)
	}
	if err := rows.Err(); err != nil {
		return governor.PersistedState{}, false, err
	}

	rows, err = q.QueryContext(ctx,
		`SELECT request_id, state, completed_at FROM governor_request_records ORDER BY request_id`)
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor request records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var record governor.RequestRecordState
		var completedAt string
		if err := rows.Scan(&record.RequestID, &record.State, &completedAt); err != nil {
			return governor.PersistedState{}, false, fmt.Errorf("scan governor request record: %w", err)
		}
		record.CompletedAt = parseTime(completedAt)
		state.RequestRecords = append(state.RequestRecords, record)
	}
	if err := rows.Err(); err != nil {
		return governor.PersistedState{}, false, err
	}

	rows, err = q.QueryContext(ctx,
		`SELECT attempt_id, seen_at FROM governor_attempt_ids ORDER BY attempt_id`)
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor attempt ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var record governor.AttemptIDRecord
		var seenAt string
		if err := rows.Scan(&record.AttemptID, &seenAt); err != nil {
			return governor.PersistedState{}, false, fmt.Errorf("scan governor attempt id: %w", err)
		}
		record.SeenAt = parseTime(seenAt)
		state.AttemptIDs = append(state.AttemptIDs, record)
	}
	if err := rows.Err(); err != nil {
		return governor.PersistedState{}, false, err
	}

	rows, err = q.QueryContext(ctx,
		`SELECT at FROM governor_rate_events ORDER BY id`)
	if err != nil {
		return governor.PersistedState{}, false, fmt.Errorf("load governor rate events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return governor.PersistedState{}, false, fmt.Errorf("scan governor rate event: %w", err)
		}
		state.RateEvents = append(state.RateEvents, parseTime(at))
	}
	if err := rows.Err(); err != nil {
		return governor.PersistedState{}, false, err
	}
	return state, true, nil
}

// governorEventPayload renders the protection projection into a compact
// journal payload. The authoritative counts live in the governor_state
// projection; the payload is informational context for inspect.
func governorEventPayload(state governor.PersistedState) map[string]any {
	now := time.Now().UTC()
	taskUsed := 0
	for _, record := range state.TaskStates {
		taskUsed += record.Attempts
	}
	return map[string]any{
		"next_attempt":   state.NextAttempt,
		"rolling_3h":     len(state.RollingEvents),
		"rolling_1h":     rollingCount(state.RollingEvents, now, time.Hour),
		"rolling_10m":    rollingCount(state.RollingEvents, now, 10*time.Minute),
		"task_used":      taskUsed,
		"cooldown_until": formatTime(state.CooldownUntil),
		"circuit":        state.Circuit.State,
		"circuit_until":  formatTime(state.Circuit.OpenUntil),
	}
}

func rollingCount(events []governor.LedgerEvent, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	count := 0
	for _, event := range events {
		if event.At.After(cutoff) {
			count++
		}
	}
	return count
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
