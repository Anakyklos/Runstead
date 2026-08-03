package governor

import (
	"errors"
	"time"
)

type RefreshAdmission struct {
	Code   AdmissionCode
	Permit *RefreshPermit
	Err    error
}

func (r RefreshAdmission) Admitted() bool { return r.Code == AdmissionAdmitted && r.Permit != nil }

type RefreshPermit struct {
	governor *Governor
	finished bool
}

func (g *Governor) BeginCredentialRefresh() RefreshAdmission {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.circuit.refreshRequired {
		return RefreshAdmission{Code: AdmissionCircuitOpen, Err: &AdmissionError{Code: AdmissionCircuitOpen}}
	}
	if g.circuit.refreshInFlight || g.inFlight || len(g.queue) != 0 {
		return RefreshAdmission{Code: AdmissionDelayed, Err: &AdmissionError{Code: AdmissionDelayed}}
	}
	g.circuit.refreshInFlight = true
	return RefreshAdmission{Code: AdmissionAdmitted, Permit: &RefreshPermit{governor: g}}
}

func (p *RefreshPermit) Finish(success bool) error {
	if p == nil || p.governor == nil {
		return ErrPermitCompleted
	}
	g := p.governor
	g.mu.Lock()
	defer func() {
		g.mu.Unlock()
		g.flushEvents()
	}()
	if p.finished {
		return ErrPermitCompleted
	}
	p.finished = true
	g.circuit.refreshInFlight = false
	if success {
		g.circuit.refreshRequired = false
		g.transitionCircuitLocked(CircuitClosed, "", time.Time{}, "credential refresh")
	} else {
		g.transitionCircuitLocked(CircuitHumanReviewRequired, OutcomeAuthenticationExpired, time.Time{}, "credential refresh failed")
	}
	return nil
}

type Permit struct {
	governor        *Governor
	request         AttemptRequest
	started         bool
	completed       bool
	attemptSequence int
}

func (p *Permit) Start() error {
	if p == nil || p.governor == nil {
		return ErrPermitCompleted
	}
	g := p.governor
	g.mu.Lock()
	defer func() {
		g.mu.Unlock()
		g.flushEvents()
	}()
	if p.completed {
		return ErrPermitCompleted
	}
	if p.started {
		return ErrPermitStarted
	}
	if !g.inFlight {
		return errors.New("permit is not held by the governor")
	}
	now := g.clock.Now()
	before := g.budgetLocked(now, p.request.TaskID)
	p.started = true
	p.attemptSequence = g.nextAttempt
	g.nextAttempt++
	g.markRequestActiveLocked(p.request.ClientRequestID)
	g.lastStart = now
	if g.currentTask == p.request.TaskID {
		g.consecutiveTurns++
	} else {
		g.currentTask = p.request.TaskID
		g.consecutiveTurns = 1
	}
	state := g.taskLocked(p.request.TaskID)
	state.attempts++
	if p.request.Retry {
		state.retries++
	}
	g.ledger.add(now, p.request.TaskID)
	if g.telemetry.available != nil {
		value := *g.telemetry.available
		if value > 0 {
			value--
		}
		g.telemetry.available = &value
	}
	g.queueEventLocked(Event{
		Kind:             EventAttemptStarted,
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		AllowanceProfile: g.config.AllowanceProfile,
		TaskID:           p.request.TaskID,
		ClientRequestID:  p.request.ClientRequestID,
		AttemptSequence:  p.attemptSequence,
		Admission:        AdmissionAdmitted,
		BudgetsBefore:    before,
		BudgetsAfter:     g.budgetLocked(now, p.request.TaskID),
		Telemetry:        g.telemetrySummaryLocked(),
		TelemetryHealthy: true,
	})
	return nil
}

func (p *Permit) CancelBeforeStart() error {
	if p == nil || p.governor == nil {
		return ErrPermitCompleted
	}
	g := p.governor
	g.mu.Lock()
	defer func() {
		g.mu.Unlock()
		g.flushEvents()
	}()
	if p.completed {
		return ErrPermitCompleted
	}
	if p.started {
		return ErrPermitStarted
	}
	p.completed = true
	g.releaseRequestLocked(p.request.ClientRequestID)
	g.inFlight = false
	g.activeTaskID = ""
	g.emitAdmissionLocked(p.request, AdmissionResult{Code: AdmissionContextCancelled, Reason: AdmissionContextCancelled}, true)
	g.signalAllLocked()
	return nil
}

func (p *Permit) Finish(outcome Outcome) FinishResult {
	if p == nil || p.governor == nil {
		return FinishResult{Err: ErrPermitCompleted}
	}
	g := p.governor
	g.mu.Lock()
	defer func() {
		g.mu.Unlock()
		g.flushEvents()
	}()
	if p.completed {
		return FinishResult{Err: ErrPermitCompleted}
	}
	if !p.started {
		return FinishResult{Err: ErrPermitNotStarted}
	}
	p.completed = true
	now := g.clock.Now()
	before := g.budgetLocked(now, p.request.TaskID)
	result := g.recordOutcomeLocked(now, p, outcome)
	g.completeRequestLocked(p.request.ClientRequestID, now)
	g.inFlight = false
	g.activeTaskID = ""
	g.signalAllLocked()
	g.queueEventLocked(Event{
		Kind:             EventAttemptFinished,
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		AllowanceProfile: g.config.AllowanceProfile,
		TaskID:           p.request.TaskID,
		ClientRequestID:  p.request.ClientRequestID,
		AttemptSequence:  p.attemptSequence,
		Outcome:          result.Outcome,
		CooldownUntil:    g.cooldownUntil,
		SelectedBackoff:  result.SelectedBackoff,
		CircuitTo:        result.Circuit.State,
		BudgetsBefore:    before,
		BudgetsAfter:     g.budgetLocked(g.clock.Now(), p.request.TaskID),
		Telemetry:        g.telemetrySummaryLocked(),
		TelemetryHealthy: true,
	})
	return result
}
