package governor

import (
	"context"
	"sort"
	"time"
)

func (g *Governor) readTelemetry(ctx context.Context) (*TelemetrySnapshot, bool) {
	if g.telemetrySource == nil {
		return nil, true
	}
	snapshot, err := g.telemetrySource.Snapshot(ctx)
	if err != nil {
		return nil, false
	}
	return &snapshot, true
}

func (g *Governor) applyTelemetryLocked(snapshot TelemetrySnapshot, now time.Time) {
	previousReset := g.telemetry.resetAt
	if snapshot.RouteSafety != nil {
		if err := snapshot.RouteSafety.Validate(); err != nil || !snapshot.RouteSafety.Equal(g.config.RouteSafety) {
			g.telemetry.unsafe = true
		}
	}
	if snapshot.ResetAt.After(now) {
		if g.telemetry.resetAt.IsZero() || !g.telemetry.resetAt.Equal(snapshot.ResetAt) || !now.Before(g.telemetry.resetAt) {
			g.telemetry.available = cloneInt(snapshot.Remaining)
		} else if snapshot.Remaining != nil && (g.telemetry.available == nil || *snapshot.Remaining < *g.telemetry.available) {
			g.telemetry.available = cloneInt(snapshot.Remaining)
		}
		g.telemetry.resetAt = snapshot.ResetAt
	} else if snapshot.Remaining != nil && (g.telemetry.available == nil || *snapshot.Remaining < *g.telemetry.available) {
		g.telemetry.available = cloneInt(snapshot.Remaining)
	}
	if snapshot.CooldownUntil.After(g.telemetry.cooldownUntil) {
		g.telemetry.cooldownUntil = snapshot.CooldownUntil
	}
	if snapshot.RetryAfter > 0 {
		until := now.Add(snapshot.RetryAfter)
		if until.After(g.telemetry.cooldownUntil) {
			g.telemetry.cooldownUntil = until
		}
		if until.After(g.cooldownUntil) {
			g.cooldownUntil = until
		}
	}
	if snapshot.CooldownUntil.After(g.cooldownUntil) {
		g.cooldownUntil = snapshot.CooldownUntil
	}
	g.telemetry.rateLimited = snapshot.RateLimited
	g.telemetry.capacityExhausted = snapshot.CapacityExhausted
	if snapshot.UpstreamCircuit != UpstreamCircuitUnknown {
		g.telemetry.upstreamCircuit = snapshot.UpstreamCircuit
	}
	if (snapshot.RateLimited || snapshot.CapacityExhausted) && previousReset.After(now) && snapshot.ResetAt.After(now) {
		openUntil := previousReset
		if snapshot.ResetAt.After(openUntil) {
			openUntil = snapshot.ResetAt
		}
		g.transitionCircuitLocked(CircuitOpenUntil, OutcomeRateCapacity, openUntil.Add(g.config.ResetSafetyMargin), "telemetry repeated pre-reset rate response")
	}
	if snapshot.UpstreamCircuit == UpstreamCircuitOpen && snapshot.ResetAt.After(g.telemetry.cooldownUntil) {
		g.telemetry.cooldownUntil = snapshot.ResetAt
	}
	if snapshot.UpstreamCircuit == UpstreamCircuitOpen && snapshot.CooldownUntil.After(g.cooldownUntil) {
		g.cooldownUntil = snapshot.CooldownUntil
	}
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (g *Governor) expireLocked(now time.Time) {
	g.ledger.expire(now, 3*time.Hour)
	g.pruneRetentionLocked(now)
	cutoff := now.Add(-g.config.RateResponseWindow)
	index := sort.Search(len(g.circuit.rateEvents), func(index int) bool {
		return g.circuit.rateEvents[index].After(cutoff)
	})
	if index > 0 {
		g.circuit.rateEvents = append([]time.Time(nil), g.circuit.rateEvents[index:]...)
	}
	if !g.telemetry.resetAt.IsZero() && !now.Before(g.telemetry.resetAt) {
		g.telemetry.available = nil
		g.telemetry.resetAt = time.Time{}
		g.telemetry.rateLimited = false
		g.telemetry.capacityExhausted = false
		g.telemetry.cooldownUntil = time.Time{}
	}
}

func (g *Governor) budgetLocked(now time.Time, taskID string) BudgetSnapshot {
	g.expireLocked(now)
	taskUsed := 0
	if taskID != "" {
		taskUsed = g.taskLocked(taskID).attempts
	}
	manualReserveRemaining := g.config.ManualReserve
	if g.telemetry.available != nil && *g.telemetry.available < manualReserveRemaining {
		manualReserveRemaining = *g.telemetry.available
	}
	return BudgetSnapshot{
		Rolling3hUsed:          g.ledger.count(now, 3*time.Hour),
		Rolling3hCeiling:       g.config.Rolling3h,
		Automated3hCeiling:     g.config.Rolling3h,
		Rolling1hUsed:          g.ledger.count(now, time.Hour),
		Rolling1hCeiling:       g.config.Rolling1h,
		Rolling10mUsed:         g.ledger.count(now, 10*time.Minute),
		Rolling10mCeiling:      g.config.Rolling10m,
		TaskUsed:               taskUsed,
		TaskCeiling:            g.config.TaskBudget,
		RetriesUsed:            g.taskRetryCountLocked(taskID),
		RetryCeiling:           g.config.RetryBudget,
		ManualReserve:          g.config.ManualReserve,
		ManualReserveRemaining: manualReserveRemaining,
	}
}

func (g *Governor) taskRetryCountLocked(taskID string) int {
	if taskID == "" {
		return 0
	}
	return g.taskLocked(taskID).retries
}

func (g *Governor) telemetrySummaryLocked() TelemetrySummary {
	return TelemetrySummary{
		Available:         g.telemetrySource != nil,
		Remaining:         cloneInt(g.telemetry.available),
		ResetAt:           g.telemetry.resetAt,
		CooldownUntil:     g.telemetry.cooldownUntil,
		RateLimited:       g.telemetry.rateLimited,
		CapacityExhausted: g.telemetry.capacityExhausted,
		UpstreamCircuit:   g.telemetry.upstreamCircuit,
	}
}

func (g *Governor) circuitSnapshotLocked() CircuitSnapshot {
	return CircuitSnapshot{State: g.circuit.state, Reason: g.circuit.reason, OpenUntil: g.circuit.openUntil, RefreshRequired: g.circuit.refreshRequired}
}

func (g *Governor) emitAdmissionLocked(request AttemptRequest, result AdmissionResult, telemetryHealthy bool) {
	if g.events == nil {
		return
	}
	budgets := g.budgetLocked(g.clock.Now(), request.TaskID)
	event := Event{
		Kind:             EventAdmission,
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		Model:            g.config.Model,
		AllowanceProfile: g.config.AllowanceProfile,
		TaskID:           request.TaskID,
		ClientRequestID:  request.ClientRequestID,
		Admission:        result.Code,
		Reason:           result.Reason,
		Delay:            result.Delay,
		RetryAt:          result.RetryAt,
		BudgetsBefore:    budgets,
		BudgetsAfter:     budgets,
		Telemetry:        g.telemetrySummaryLocked(),
		TelemetryHealthy: telemetryHealthy,
	}
	g.queueEventLocked(event)
}
