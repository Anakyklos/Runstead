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
	startedAt        time.Time
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
	p.startedAt = now
	p.attemptSequence = g.nextAttempt
	g.nextAttempt++
	g.markRequestActiveLocked(p.request.ClientRequestID)
	if !receiptAware {
		g.lastStart = now
	}
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
			Model:            g.config.Model,
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
		Model:            g.config.Model,
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
	now := g.clock.Now()
	expectedProvider := g.config.AttemptProviderID
	if expectedProvider == "" {
		expectedProvider = g.config.ProviderID
	}
	var validationErr error
	if set == nil {
		validationErr = provider.ErrInvalidAttemptReceipts
	} else {
		validationErr = provider.ValidateAttemptReceiptSet(*set, provider.AttemptReceiptExpectation{
			ClientRequestID:    p.request.ClientRequestID,
			Provider:           expectedProvider,
			Model:              g.config.Model,
			AccountLaneHash:    g.config.AccountLaneHash,
			RequestStartedAt:   p.startedAt,
			RequestCompletedAt: now,
			SingleAttempt:      true,
			Now:                now,
		})
	}
	if validationErr != nil {
		return p.finishReceiptFailureLocked(validationErr, !errors.Is(validationErr, ErrAttemptReceiptReplayed))
	}
	g.pruneAttemptIDsLocked(now)
	replayed := false
	newAttemptIDs := 0
	for _, receipt := range set.Receipts {
		if _, seen := g.attemptIDs[receipt.AttemptID]; seen {
			replayed = true
			continue
		}
		newAttemptIDs++
	}
	if replayed {
		for _, receipt := range set.Receipts {
			if _, seen := g.attemptIDs[receipt.AttemptID]; !seen {
				g.attemptIDs[receipt.AttemptID] = now
			}
		}
		return p.finishReceiptFailureLocked(ErrAttemptReceiptReplayed, newAttemptIDs > 0)
	}
	for _, receipt := range set.Receipts {
		g.attemptIDs[receipt.AttemptID] = now
	}
	before := g.budgetLocked(now, p.request.TaskID)
	state := g.taskLocked(p.request.TaskID)
	receiptOutcomes := make([]Outcome, len(set.Receipts))
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
			Model:                 receipt.Model,
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
		receiptOutcomes[index] = Outcome{Class: receiptOutcome(receipt.Outcome), UpstreamReached: receipt.UpstreamReached}
	}
	if outcome.Class == OutcomeCancelledBeforeUpstream {
		outcome.Class = OutcomeUncertainReached
	}
	result := FinishResult{}
	aggregate := outcome.Class
	if aggregate == "" {
		aggregate = OutcomeSuccess
	}
	var selectedBackoff time.Duration
	for index, receipt := range set.Receipts {
		receiptOutcomeValue := receiptOutcomes[index]
		if index == len(set.Receipts)-1 && outcome.Class != "" && outcome.Class != OutcomeSuccess && outcome.Class == receiptOutcomeValue.Class {
			receiptOutcomeValue.RetryAfter = outcome.RetryAfter
			receiptOutcomeValue.ResetAt = outcome.ResetAt
		}
		record := g.recordOutcomeLocked(receipt.CompletedAt, p, receiptOutcomeValue)
		if record.SelectedBackoff > selectedBackoff {
			selectedBackoff = record.SelectedBackoff
		}
		if index == len(set.Receipts)-1 {
			result = record
		}
		aggregate = strongerOutcome(aggregate, receiptOutcomeValue.Class)
	}
	if outcome.Class != "" && outcome.Class != OutcomeSuccess && outcome.Class != receiptOutcomes[len(receiptOutcomes)-1].Class {
		record := g.recordOutcomeLocked(now, p, outcome)
		if record.SelectedBackoff > selectedBackoff {
			selectedBackoff = record.SelectedBackoff
		}
		result = record
	}
	if set.ContainsUncertain() {
		aggregate = strongerOutcome(aggregate, OutcomeUncertainReached)
	}
	result.Outcome = strongerOutcome(OutcomeSuccess, aggregate)
	result.AttemptDebited = len(set.Receipts)
	result.SelectedBackoff = selectedBackoff
	result.Circuit = g.circuitSnapshotLocked()
	result.RetryEligible = isRecoverableOutcome(result.Outcome) && state.retries < g.config.RetryBudget && g.circuit.state == CircuitClosed
	latestStart := set.Receipts[len(set.Receipts)-1].StartedAt
	if latestStart.After(g.lastStart) {
		g.lastStart = latestStart
	}
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
		Model:            g.config.Model,
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

func (p *Permit) finishReceiptFailureLocked(err error, debitPossibleAttempt bool) FinishResult {
	g := p.governor
	p.completed = true
	g.telemetry.unsafe = true
	now := g.clock.Now()
	before := g.budgetLocked(now, p.request.TaskID)
	result := FinishResult{Outcome: OutcomeUncertainReached, Err: err, Circuit: g.circuitSnapshotLocked()}
	if debitPossibleAttempt {
		state := g.taskLocked(p.request.TaskID)
		state.attempts++
		if p.request.Retry {
			state.retries++
		}
		debitAt := p.startedAt
		if debitAt.IsZero() {
			debitAt = now
		}
		g.ledger.add(debitAt, p.request.TaskID)
		if g.telemetry.available != nil {
			value := *g.telemetry.available
			if value > 0 {
				value--
			}
			g.telemetry.available = &value
		}
		g.lastStart = debitAt
		result.AttemptDebited = 1
		g.queueEventLocked(Event{
			Kind:             EventUncertainAttempt,
			AccountPolicyID:  g.config.AccountPolicyID,
			ProviderID:       g.config.ProviderID,
			ModelPool:        g.config.ModelPool,
			Model:            g.config.Model,
			AllowanceProfile: g.config.AllowanceProfile,
			TaskID:           p.request.TaskID,
			ClientRequestID:  p.request.ClientRequestID,
			AttemptSequence:  p.attemptSequence,
			Outcome:          OutcomeUncertainReached,
			UpstreamReached:  true,
			TelemetryHealthy: p.telemetryHealthy,
		})
	}
	g.completeRequestLocked(p.request.ClientRequestID, now)
	g.inFlight = false
	g.activeTaskID = ""
	g.signalAllLocked()
	g.queueEventLocked(Event{
		Kind:             EventAttemptFinished,
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		Model:            g.config.Model,
		AllowanceProfile: g.config.AllowanceProfile,
		TaskID:           p.request.TaskID,
		ClientRequestID:  p.request.ClientRequestID,
		AttemptSequence:  p.attemptSequence,
		Outcome:          result.Outcome,
		BudgetsBefore:    before,
		BudgetsAfter:     g.budgetLocked(now, p.request.TaskID),
		Telemetry:        g.telemetrySummaryLocked(),
		CircuitTo:        result.Circuit.State,
		TelemetryHealthy: p.telemetryHealthy,
	})
	return result
}

func receiptOutcome(outcome provider.AttemptOutcome) OutcomeClass {
	switch outcome {
	case provider.AttemptOutcomeSuccess:
		return OutcomeSuccess
	case provider.AttemptOutcomeRateCapacity:
		return OutcomeRateCapacity
	case provider.AttemptOutcomeAuthenticationExpired:
		return OutcomeAuthenticationExpired
	case provider.AttemptOutcomeAuthenticationDenied:
		return OutcomeAuthenticationDenied
	case provider.AttemptOutcomeHTTP403:
		return OutcomeHTTP403
	case provider.AttemptOutcomeLoginChallenge:
		return OutcomeLoginChallenge
	case provider.AttemptOutcomeCAPTCHA:
		return OutcomeCAPTCHA
	case provider.AttemptOutcomeSuspiciousActivity:
		return OutcomeSuspiciousActivity
	case provider.AttemptOutcomeAccountWarning:
		return OutcomeAccountWarning
	case provider.AttemptOutcomeFeatureRestriction:
		return OutcomeFeatureRestriction
	case provider.AttemptOutcomeConnectionReset:
		return OutcomeConnectionReset
	case provider.AttemptOutcomeTimeout:
		return OutcomeTimeout
	case provider.AttemptOutcomeEmptyResponse:
		return OutcomeEmptyResponse
	case provider.AttemptOutcomeMalformedUpstream:
		return OutcomeMalformedUpstream
	case provider.AttemptOutcomeUpstreamServerFailure:
		return OutcomeUpstreamServerFailure
	case provider.AttemptOutcomeCancelled, provider.AttemptOutcomeUncertain,
		provider.AttemptOutcomeError, provider.AttemptOutcomeHTTPError, provider.AttemptOutcomeTransportError:
		return OutcomeUncertainReached
	default:
		return OutcomeUncertainReached
	}
}

func strongerOutcome(current, candidate OutcomeClass) OutcomeClass {
	if outcomePriority(candidate) > outcomePriority(current) {
		return candidate
	}
	return current
}

func outcomePriority(outcome OutcomeClass) int {
	switch outcome {
	case OutcomeAuthenticationDenied, OutcomeHTTP403, OutcomeLoginChallenge, OutcomeCAPTCHA,
		OutcomeSuspiciousActivity, OutcomeAccountWarning, OutcomeFeatureRestriction:
		return 100
	case OutcomeAuthenticationExpired:
		return 95
	case OutcomeRateCapacity:
		return 90
	case OutcomeUncertainReached:
		return 80
	case OutcomeConnectionReset, OutcomeTimeout:
		return 70
	case OutcomeMalformedUpstream, OutcomeEmptyResponse, OutcomeUpstreamServerFailure:
		return 60
	case OutcomeSuccess:
		return 0
	default:
		return 50
	}
}

func isRecoverableOutcome(outcome OutcomeClass) bool {
	switch outcome {
	case OutcomeRateCapacity, OutcomeConnectionReset, OutcomeTimeout, OutcomeEmptyResponse, OutcomeMalformedUpstream, OutcomeUpstreamServerFailure:
		return true
	default:
		return false
	}
}
