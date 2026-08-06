package governor

import (
	"errors"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
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
	defer g.mu.Unlock()
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
	governor         *Governor
	request          AttemptRequest
	telemetryHealthy bool
	started          bool
	completed        bool
	attemptSequence  int
	receiptAware     bool
}

func (p *Permit) Start() error {
	return p.start(false)
}

func (p *Permit) StartReceiptAware() error {
	return p.start(true)
}

func (p *Permit) start(receiptAware bool) error {
	if p == nil || p.governor == nil {
		return ErrPermitCompleted
	}
	g := p.governor
	g.mu.Lock()
	defer g.mu.Unlock()
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
	p.receiptAware = receiptAware
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
	if !receiptAware {
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
			TelemetryHealthy: p.telemetryHealthy,
		})
	}
	return nil
}

func (p *Permit) CancelBeforeStart() error {
	if p == nil || p.governor == nil {
		return ErrPermitCompleted
	}
	g := p.governor
	g.mu.Lock()
	defer g.mu.Unlock()
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
	g.emitAdmissionLocked(p.request, AdmissionResult{Code: AdmissionContextCancelled, Reason: AdmissionContextCancelled, TelemetryHealthy: p.telemetryHealthy}, p.telemetryHealthy)
	g.signalAllLocked()
	return nil
}

func (p *Permit) Finish(outcome Outcome) FinishResult {
	if p == nil || p.governor == nil {
		return FinishResult{Err: ErrPermitCompleted}
	}
	g := p.governor
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.completed {
		return FinishResult{Err: ErrPermitCompleted}
	}
	if !p.started {
		return FinishResult{Err: ErrPermitNotStarted}
	}
	if p.receiptAware {
		return FinishResult{Err: ErrAttemptReceiptsRequired}
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
		TelemetryHealthy: p.telemetryHealthy,
	})
	return result
}

func (p *Permit) FinishWithAttemptReceipts(outcome Outcome, set *provider.AttemptReceiptSet) FinishResult {
	if p == nil || p.governor == nil {
		return FinishResult{Err: ErrPermitCompleted}
	}
	g := p.governor
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.completed {
		return FinishResult{Err: ErrPermitCompleted}
	}
	if !p.started {
		return FinishResult{Err: ErrPermitNotStarted}
	}
	if !p.receiptAware {
		return FinishResult{Err: ErrAttemptReceiptsRequired}
	}
	expectedProvider := g.config.AttemptProviderID
	if expectedProvider == "" {
		expectedProvider = g.config.ProviderID
	}
	expectedModel := p.request.ModelPool
	if expectedModel == "" {
		expectedModel = g.config.ModelPool
	}
	var validationErr error
	if set == nil {
		validationErr = provider.ErrInvalidAttemptReceipts
	} else {
		validationErr = provider.ValidateAttemptReceiptSet(*set, provider.AttemptReceiptExpectation{
			ClientRequestID: p.request.ClientRequestID,
			Provider:        expectedProvider,
			Model:           expectedModel,
			AccountLaneHash: g.config.AccountLaneHash,
			Now:             g.clock.Now(),
		})
	}
	if validationErr != nil {
		return p.finishReceiptFailureLocked(validationErr)
	}
	now := g.clock.Now()
	before := g.budgetLocked(now, p.request.TaskID)
	state := g.taskLocked(p.request.TaskID)
	for index, receipt := range set.Receipts {
		g.ledger.add(receipt.StartedAt, p.request.TaskID)
		state.attempts++
		if index > 0 || p.request.Retry {
			state.retries++
		}
		if index > 0 {
			g.nextAttempt++
		}
		if g.telemetry.available != nil {
			value := *g.telemetry.available
			if value > 0 {
				value--
			}
			g.telemetry.available = &value
		}
		g.queueEventLocked(Event{
			Kind:                  EventUpstreamAttempt,
			AccountPolicyID:       g.config.AccountPolicyID,
			ProviderID:            g.config.ProviderID,
			ModelPool:             g.config.ModelPool,
			AllowanceProfile:      g.config.AllowanceProfile,
			TaskID:                p.request.TaskID,
			ClientRequestID:       p.request.ClientRequestID,
			AttemptSequence:       receipt.Sequence,
			AttemptID:             receipt.AttemptID,
			AttemptTrigger:        receipt.Trigger,
			AttemptReceiptOutcome: receipt.Outcome,
			UpstreamReached:       receipt.UpstreamReached,
			Outcome:               receiptOutcome(receipt.Outcome),
			TelemetryHealthy:      p.telemetryHealthy,
		})
	}
	if set.ContainsUncertain() {
		outcome.Class = OutcomeUncertainReached
		outcome.UpstreamReached = true
	}
	result := g.recordOutcomeLocked(now, p, outcome)
	result.AttemptDebited = len(set.Receipts)
	p.completed = true
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
		TelemetryHealthy: p.telemetryHealthy,
	})
	return result
}

func (p *Permit) finishReceiptFailureLocked(err error) FinishResult {
	g := p.governor
	p.completed = true
	g.telemetry.unsafe = true
	now := g.clock.Now()
	result := FinishResult{Outcome: OutcomeUncertainReached, Err: err, Circuit: g.circuitSnapshotLocked()}
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
		CircuitTo:        result.Circuit.State,
		TelemetryHealthy: p.telemetryHealthy,
	})
	return result
}

func receiptOutcome(outcome provider.AttemptOutcome) OutcomeClass {
	if outcome == provider.AttemptOutcomeSuccess {
		return OutcomeSuccess
	}
	if outcome == provider.AttemptOutcomeTimeout {
		return OutcomeTimeout
	}
	if outcome == provider.AttemptOutcomeCancelled {
		return OutcomeUncertainReached
	}
	if outcome == provider.AttemptOutcomeUncertain {
		return OutcomeUncertainReached
	}
	return OutcomeClass(outcome)
}
