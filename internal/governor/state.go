package governor

import (
	"time"
)

// PersistedState returns the serializable account-protection projection.
// Callers use it to persist governor state atomically with provider attempt
// transitions; the CLI feeds it back through Options.Restore so a restart
// does not reset account protection.
func (g *Governor) PersistedState() PersistedState {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	g.expireLocked(now)
	return g.persistedStateLocked()
}

func (g *Governor) persistedStateLocked() PersistedState {
	telemetry := g.telemetrySummaryLocked()
	state := PersistedState{
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		Model:            g.config.Model,
		AllowanceProfile: g.config.AllowanceProfile,
		NextAttempt:      g.nextAttempt,
		LastStart:        g.lastStart,
		CooldownUntil:    g.cooldownUntil,
		Circuit:          g.circuitSnapshotLocked(),
		RateEvents:       append([]time.Time(nil), g.circuit.rateEvents...),
		LastRateReset:    g.circuit.lastRateReset,
		Telemetry: PersistedTelemetry{
			Available:         telemetry.Remaining,
			ResetAt:           telemetry.ResetAt,
			CooldownUntil:     telemetry.CooldownUntil,
			RateLimited:       telemetry.RateLimited,
			CapacityExhausted: telemetry.CapacityExhausted,
			UpstreamCircuit:   telemetry.UpstreamCircuit,
			Unsafe:            g.telemetry.unsafe,
		},
		RollingEvents: persistedLedgerEvents(g.ledger.copyEvents()),
		AttemptIDs:    attemptIDRecords(g.attemptIDs),
		Ceilings: BudgetCeilings{
			Rolling3h:   g.config.Rolling3h,
			Rolling1h:   g.config.Rolling1h,
			Rolling10m:  g.config.Rolling10m,
			TaskBudget:  g.config.TaskBudget,
			RetryBudget: g.config.RetryBudget,
		},
	}
	for taskID, record := range g.tasks {
		state.TaskStates = append(state.TaskStates, TaskStateRecord{
			TaskID:      taskID,
			Attempts:    record.attempts,
			Retries:     record.retries,
			LastTouched: record.lastTouched,
		})
	}
	for requestID, record := range g.requestIDs {
		state.RequestRecords = append(state.RequestRecords, RequestRecordState{
			RequestID:   requestID,
			State:       requestStateName(record.state),
			CompletedAt: record.completedAt,
		})
	}
	return state
}

func attemptIDRecords(attemptIDs map[string]time.Time) []AttemptIDRecord {
	records := make([]AttemptIDRecord, 0, len(attemptIDs))
	for attemptID, seenAt := range attemptIDs {
		records = append(records, AttemptIDRecord{AttemptID: attemptID, SeenAt: seenAt})
	}
	return records
}

func persistedLedgerEvents(events []ledgerEvent) []LedgerEvent {
	records := make([]LedgerEvent, 0, len(events))
	for _, event := range events {
		records = append(records, LedgerEvent{At: event.at, TaskID: event.taskID})
	}
	return records
}

func requestStateName(state requestState) string {
	switch state {
	case requestPending:
		return "pending"
	case requestActive:
		return "active"
	case requestCompleted:
		return "completed"
	default:
		return ""
	}
}

// RestorePersistedState applies a previously persisted protection projection
// to the governor. Restored state must obey the existing governor invariants:
// usage counts are additive with the restored ledger, cooldown and circuit
// continue to gate admission, and expired circuit windows are normalized to
// closed. In-flight and queue state are never restored.
func (g *Governor) RestorePersistedState(state PersistedState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	if state.NextAttempt > g.nextAttempt {
		g.nextAttempt = state.NextAttempt
	}
	g.lastStart = state.LastStart
	g.cooldownUntil = state.CooldownUntil
	if state.Circuit.State != "" {
		g.circuit.state = state.Circuit.State
		g.circuit.reason = state.Circuit.Reason
		g.circuit.openUntil = state.Circuit.OpenUntil
		g.circuit.refreshRequired = state.Circuit.RefreshRequired
	}
	g.circuit.rateEvents = append([]time.Time(nil), state.RateEvents...)
	g.circuit.lastRateReset = state.LastRateReset
	if state.Telemetry.Available != nil {
		value := *state.Telemetry.Available
		g.telemetry.available = &value
	}
	g.telemetry.resetAt = state.Telemetry.ResetAt
	g.telemetry.cooldownUntil = state.Telemetry.CooldownUntil
	g.telemetry.rateLimited = state.Telemetry.RateLimited
	g.telemetry.capacityExhausted = state.Telemetry.CapacityExhausted
	g.telemetry.upstreamCircuit = state.Telemetry.UpstreamCircuit
	g.telemetry.unsafe = state.Telemetry.Unsafe
	for _, event := range state.RollingEvents {
		if !event.At.IsZero() {
			g.ledger.add(event.At, event.TaskID)
		}
	}
	for _, record := range state.TaskStates {
		g.tasks[record.TaskID] = &taskState{
			attempts:    record.Attempts,
			retries:     record.Retries,
			lastTouched: record.LastTouched,
		}
	}
	for _, record := range state.RequestRecords {
		g.requestIDs[record.RequestID] = requestRecord{
			state:       requestStateFromName(record.State),
			completedAt: record.CompletedAt,
		}
	}
	for _, record := range state.AttemptIDs {
		if !record.SeenAt.IsZero() {
			g.attemptIDs[record.AttemptID] = record.SeenAt
		}
	}
	// Normalize expired circuit windows: an open_until in the past means the
	// circuit would auto-close on the next admission; restore it closed so
	// the persisted projection matches the observable behavior.
	if g.circuit.state == CircuitOpenUntil && !g.circuit.openUntil.IsZero() && !now.Before(g.circuit.openUntil) {
		g.circuit.state = CircuitClosed
		g.circuit.reason = ""
		g.circuit.openUntil = time.Time{}
	}
	g.pruneRetentionLocked(now)
}

func requestStateFromName(name string) requestState {
	switch name {
	case "pending":
		return requestPending
	case "active":
		return requestActive
	case "completed":
		return requestCompleted
	default:
		return 0
	}
}
