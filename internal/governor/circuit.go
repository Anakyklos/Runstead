package governor

import (
	"errors"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func (g *Governor) transitionCircuitLocked(state CircuitState, reason OutcomeClass, openUntil time.Time, _ string) {
	from := g.circuit.state
	g.circuit.state = state
	g.circuit.reason = reason
	g.circuit.openUntil = openUntil
	if from != state {
		g.queueEventLocked(Event{
			Kind:             EventCircuit,
			AccountPolicyID:  g.config.AccountPolicyID,
			ProviderID:       g.config.ProviderID,
			ModelPool:        g.config.ModelPool,
			Model:            g.config.Model,
			AllowanceProfile: g.config.AllowanceProfile,
			AllowanceKind:    g.config.AllowanceKind,
			CircuitFrom:      from,
			CircuitTo:        state,
			CircuitReason:    reason,
			CooldownUntil:    openUntil,
		})
	}
}

func (g *Governor) recordOutcomeLocked(now time.Time, permit *Permit, outcome Outcome) FinishResult {
	if outcome.Class == "" {
		outcome.Class = OutcomeUncertainReached
	}
	if outcome.Class == OutcomeCancelledBeforeUpstream &&
		effectiveDeliveryState(outcome.DeliveryState) != provider.DeliveryNotSent {
		outcome.Class = OutcomeUncertainReached
	}
	selected := time.Duration(0)
	if outcome.Class == OutcomeRateCapacity {
		g.expireLocked(now)
		g.circuit.rateEvents = append(g.circuit.rateEvents, now)
		sequence := len(g.circuit.rateEvents)
		base := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second}
		baseline := base[min(sequence-1, len(base)-1)]
		selected = g.jitter.Apply(baseline, sequence)
		if selected < baseline {
			selected = baseline
		}
		authoritativeUntil := time.Time{}
		if outcome.RetryAfter > 0 {
			authoritativeUntil = now.Add(outcome.RetryAfter)
		}
		if outcome.ResetAt.After(authoritativeUntil) {
			authoritativeUntil = outcome.ResetAt
		}
		if !authoritativeUntil.IsZero() {
			selected = authoritativeUntil.Sub(now)
		}
		if selected > 0 && now.Add(selected).After(g.cooldownUntil) {
			g.cooldownUntil = now.Add(selected)
		}
		if !g.circuit.lastRateReset.IsZero() && outcome.ResetAt.After(now) && now.Before(g.circuit.lastRateReset) {
			openUntil := g.circuit.lastRateReset.Add(g.config.ResetSafetyMargin)
			if outcome.ResetAt.Add(g.config.ResetSafetyMargin).After(openUntil) {
				openUntil = outcome.ResetAt.Add(g.config.ResetSafetyMargin)
			}
			g.transitionCircuitLocked(CircuitOpenUntil, outcome.Class, openUntil, "pre-reset repeated rate response")
		}
		if !outcome.ResetAt.IsZero() {
			g.circuit.lastRateReset = outcome.ResetAt
		}
		if len(g.circuit.rateEvents) >= g.config.RateResponseThreshold {
			g.transitionCircuitLocked(CircuitHumanReviewRequired, outcome.Class, time.Time{}, "rate response threshold")
		}
	} else {
		switch outcome.Class {
		case OutcomeAuthenticationExpired:
			g.circuit.refreshRequired = true
			g.transitionCircuitLocked(CircuitOpenUntil, outcome.Class, time.Time{}, "credential refresh required")
		case OutcomeAuthenticationDenied, OutcomeHTTP403, OutcomeLoginChallenge, OutcomeCAPTCHA, OutcomeSuspiciousActivity, OutcomeAccountWarning, OutcomeFeatureRestriction:
			g.transitionCircuitLocked(CircuitHumanReviewRequired, outcome.Class, time.Time{}, "security signal")
		}
	}
	result := FinishResult{
		Outcome:         outcome.Class,
		SelectedBackoff: selected,
		AttemptDebited:  1,
		Circuit:         g.circuitSnapshotLocked(),
		DeliveryState:   outcome.DeliveryState,
	}
	state := g.taskLocked(permit.request.TaskID)
	recoverable := outcome.Class == OutcomeRateCapacity || outcome.Class == OutcomeConnectionReset || outcome.Class == OutcomeTimeout || outcome.Class == OutcomeEmptyResponse || outcome.Class == OutcomeMalformedUpstream || outcome.Class == OutcomeUpstreamServerFailure
	result.RetryEligible = recoverable && state.retries < g.config.RetryBudget && g.circuit.state == CircuitClosed
	return result
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func (g *Governor) AcknowledgeHuman() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.circuit.state != CircuitHumanReviewRequired {
		return errors.New("human acknowledgement is not required")
	}
	g.circuit.rateEvents = nil
	g.circuit.lastRateReset = time.Time{}
	g.circuit.refreshRequired = false
	g.transitionCircuitLocked(CircuitClosed, "", time.Time{}, "human acknowledgement")
	return nil
}
