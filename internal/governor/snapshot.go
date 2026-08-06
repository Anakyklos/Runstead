package governor

func (g *Governor) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	g.expireLocked(now)
	tasks := make(map[string]TaskSnapshot, len(g.tasks))
	for taskID, state := range g.tasks {
		tasks[taskID] = TaskSnapshot{TaskID: taskID, Attempts: state.attempts, Retries: state.retries}
	}
	return Snapshot{
		AccountPolicyID:    g.config.AccountPolicyID,
		ProviderID:         g.config.ProviderID,
		ModelPool:          g.config.ModelPool,
		Model:              g.config.Model,
		AllowanceProfile:   g.config.AllowanceProfile,
		InFlight:           g.inFlight,
		QueueLength:        len(g.queue),
		NextAttempt:        g.nextAttempt,
		LastStart:          g.lastStart,
		CooldownUntil:      g.cooldownUntil,
		Budgets:            g.budgetLocked(now, ""),
		Circuit:            g.circuitSnapshotLocked(),
		Telemetry:          g.telemetrySummaryLocked(),
		PendingEvents:      len(g.pendingEvents),
		Tasks:              tasks,
		RetainedRequestIDs: len(g.requestIDs),
		RetainedAttemptIDs: len(g.attemptIDs),
		RetainedTaskStates: len(g.tasks),
	}
}
