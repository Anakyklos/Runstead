package governor

import (
	"context"
	"errors"
	"time"
)

func (g *Governor) Admit(ctx context.Context, request AttemptRequest) AdmissionResult {
	return g.admit(ctx, request, true)
}

// TryAdmit returns Delayed instead of waiting when pacing, a rolling window,
// cooldown or an occupied lane would defer the request.
func (g *Governor) TryAdmit(ctx context.Context, request AttemptRequest) AdmissionResult {
	return g.admit(ctx, request, false)
}

func (g *Governor) admit(ctx context.Context, request AttemptRequest, wait bool) AdmissionResult {
	if result := g.validateAdmissionContext(ctx, request); result.Code != "" {
		return result
	}
	if request.ModelPool == "" {
		request.ModelPool = g.config.ModelPool
	}
	if request.ModelPool != g.config.ModelPool {
		return g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("request model pool differs from account policy"))
	}

	if !wait {
		return g.tryAdmit(ctx, request)
	}
	return g.queueAdmit(ctx, request)
}

func (g *Governor) validateAdmissionContext(ctx context.Context, request AttemptRequest) AdmissionResult {
	if request.TaskID == "" || request.ClientRequestID == "" {
		return g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("task and client request identifiers are required"))
	}
	if ctx == nil {
		return g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return g.result(contextAdmissionCode(ctx), contextAdmissionCode(ctx), time.Time{}, err)
	}
	if deadline, ok := ctx.Deadline(); ok && !g.clock.Now().Before(deadline) {
		return g.result(AdmissionTaskDeadlineExceeded, AdmissionTaskDeadlineExceeded, deadline, context.DeadlineExceeded)
	}
	return AdmissionResult{}
}

func contextAdmissionCode(ctx context.Context) AdmissionCode {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return AdmissionTaskDeadlineExceeded
	}
	return AdmissionContextCancelled
}

func (g *Governor) tryAdmit(ctx context.Context, request AttemptRequest) AdmissionResult {
	defer g.flushEvents()
	g.mu.Lock()
	if g.closed {
		result := g.resultLocked(AdmissionGovernorClosed, AdmissionGovernorClosed, time.Time{}, ErrGovernorClosed)
		g.mu.Unlock()
		return result
	}
	if g.requestSeenLocked(request.ClientRequestID) {
		result := g.resultLocked(AdmissionDuplicateClientRequest, AdmissionDuplicateClientRequest, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, true)
		g.mu.Unlock()
		return result
	}
	if len(g.queue) >= g.config.QueueCapacity {
		result := g.resultLocked(AdmissionQueueFull, AdmissionQueueFull, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, true)
		g.mu.Unlock()
		return result
	}
	if len(g.queue) != 0 || g.inFlight {
		result := g.delayedLocked(request, AdmissionDelayed, g.nextAvailableLocked(g.clock.Now()), false)
		g.mu.Unlock()
		return result
	}
	g.mu.Unlock()
	telemetry, healthy := g.readTelemetry(ctx)
	g.mu.Lock()
	if g.closed {
		result := g.resultLocked(AdmissionGovernorClosed, AdmissionGovernorClosed, time.Time{}, ErrGovernorClosed)
		g.mu.Unlock()
		return result
	}
	if g.requestSeenLocked(request.ClientRequestID) {
		result := g.resultLocked(AdmissionDuplicateClientRequest, AdmissionDuplicateClientRequest, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, healthy)
		g.mu.Unlock()
		return result
	}
	if len(g.queue) >= g.config.QueueCapacity {
		result := g.resultLocked(AdmissionQueueFull, AdmissionQueueFull, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, healthy)
		g.mu.Unlock()
		return result
	}
	if len(g.queue) != 0 || g.inFlight {
		result := g.delayedLocked(request, AdmissionDelayed, g.nextAvailableLocked(g.clock.Now()), healthy)
		g.mu.Unlock()
		return result
	}
	if err := ctx.Err(); err != nil {
		result := g.resultLocked(contextAdmissionCode(ctx), contextAdmissionCode(ctx), time.Time{}, err)
		g.emitAdmissionLocked(request, result, healthy)
		g.mu.Unlock()
		return result
	}
	if telemetry != nil {
		g.applyTelemetryLocked(*telemetry, g.clock.Now())
	}
	if check := g.checkLocked(g.clock.Now(), request); check.code != "" {
		if check.delayed {
			result := g.delayedLocked(request, check.code, check.until, healthy)
			g.mu.Unlock()
			return result
		}
		result := g.resultLocked(check.code, check.code, check.until, nil)
		g.emitAdmissionLocked(request, result, healthy)
		g.mu.Unlock()
		return result
	}
	g.reserveRequestLocked(request.ClientRequestID)
	permit := g.grantLocked(request)
	g.emitAdmissionLocked(request, AdmissionResult{Code: AdmissionAdmitted, Permit: permit}, healthy)
	g.mu.Unlock()
	return AdmissionResult{Code: AdmissionAdmitted, Permit: permit}
}

func (g *Governor) queueAdmit(ctx context.Context, request AttemptRequest) AdmissionResult {
	defer g.flushEvents()
	g.mu.Lock()
	if g.closed {
		result := g.resultLocked(AdmissionGovernorClosed, AdmissionGovernorClosed, time.Time{}, ErrGovernorClosed)
		g.mu.Unlock()
		return result
	}
	if g.requestSeenLocked(request.ClientRequestID) {
		result := g.resultLocked(AdmissionDuplicateClientRequest, AdmissionDuplicateClientRequest, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, true)
		g.mu.Unlock()
		return result
	}
	if len(g.queue) >= g.config.QueueCapacity {
		result := g.resultLocked(AdmissionQueueFull, AdmissionQueueFull, time.Time{}, nil)
		g.emitAdmissionLocked(request, result, true)
		g.mu.Unlock()
		return result
	}
	waiter := &waiter{request: request, notify: make(chan struct{})}
	g.reserveRequestLocked(request.ClientRequestID)
	g.queue = append(g.queue, waiter)
	g.mu.Unlock()

	for {
		if result := g.validateAdmissionContext(ctx, request); result.Code != "" {
			g.mu.Lock()
			g.removeWaiterLocked(waiter)
			g.emitAdmissionLocked(request, result, true)
			g.mu.Unlock()
			return result
		}

		g.mu.Lock()
		if waiter.removed {
			code := AdmissionContextCancelled
			if g.closed {
				code = AdmissionGovernorClosed
			}
			result := g.resultLocked(code, code, time.Time{}, ctx.Err())
			g.mu.Unlock()
			return result
		}
		if g.closed {
			g.removeWaiterLocked(waiter)
			result := g.resultLocked(AdmissionGovernorClosed, AdmissionGovernorClosed, time.Time{}, ErrGovernorClosed)
			g.mu.Unlock()
			return result
		}
		if g.eligibleIndexLocked() != g.indexOfLocked(waiter) || waiter.checking {
			channel := waiter.notify
			g.mu.Unlock()
			if !g.waitForChange(ctx, channel, time.Time{}) {
				g.mu.Lock()
				g.removeWaiterLocked(waiter)
				result := g.contextResult(ctx)
				g.emitAdmissionLocked(request, result, true)
				g.mu.Unlock()
				return result
			}
			continue
		}
		waiter.checking = true
		g.mu.Unlock()

		telemetry, healthy := g.readTelemetry(ctx)
		now := g.clock.Now()
		g.mu.Lock()
		waiter.checking = false
		if waiter.removed {
			g.mu.Unlock()
			return g.result(AdmissionGovernorClosed, AdmissionGovernorClosed, time.Time{}, ErrGovernorClosed)
		}
		if telemetry != nil {
			g.applyTelemetryLocked(*telemetry, now)
		}
		if err := ctx.Err(); err != nil {
			g.removeWaiterLocked(waiter)
			result := g.contextResult(ctx)
			g.emitAdmissionLocked(request, result, healthy)
			g.mu.Unlock()
			return result
		}
		check := g.checkLocked(now, request)
		if check.code != "" {
			if check.delayed {
				channel := waiter.notify
				result := g.delayedLocked(request, check.code, check.until, healthy)
				g.mu.Unlock()
				if !g.waitForChange(ctx, channel, check.until) {
					g.mu.Lock()
					g.removeWaiterLocked(waiter)
					result := g.contextResult(ctx)
					g.emitAdmissionLocked(request, result, healthy)
					g.mu.Unlock()
					return result
				}
				_ = result
				continue
			}
			g.removeWaiterLocked(waiter)
			result := g.resultLocked(check.code, check.code, check.until, nil)
			g.emitAdmissionLocked(request, result, healthy)
			g.mu.Unlock()
			return result
		}
		g.removeWaiterLocked(waiter)
		g.reserveRequestLocked(request.ClientRequestID)
		permit := g.grantLocked(request)
		result := AdmissionResult{Code: AdmissionAdmitted, Permit: permit}
		g.emitAdmissionLocked(request, result, healthy)
		g.mu.Unlock()
		return result
	}
}

func (g *Governor) waitForChange(ctx context.Context, changed <-chan struct{}, until time.Time) bool {
	var timer Timer
	if !until.IsZero() {
		delay := until.Sub(g.clock.Now())
		if delay <= 0 {
			return true
		}
		timer = g.clock.NewTimer(delay)
		defer timer.Stop()
	}
	if timer == nil {
		select {
		case <-changed:
			return true
		case <-ctx.Done():
			return false
		}
	}
	select {
	case <-changed:
		return true
	case <-timer.C():
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *Governor) contextResult(ctx context.Context) AdmissionResult {
	if ctx.Err() == nil {
		return g.result(AdmissionContextCancelled, AdmissionContextCancelled, time.Time{}, context.Canceled)
	}
	code := contextAdmissionCode(ctx)
	return g.result(code, code, time.Time{}, ctx.Err())
}

type admissionCheck struct {
	code    AdmissionCode
	until   time.Time
	delayed bool
}

func (g *Governor) checkLocked(now time.Time, request AttemptRequest) admissionCheck {
	g.expireLocked(now)
	if g.inFlight {
		return admissionCheck{code: AdmissionDelayed, delayed: true}
	}
	if g.telemetry.unsafe {
		return admissionCheck{code: AdmissionUnsafeProviderAmplification}
	}
	if g.circuit.refreshRequired {
		return admissionCheck{code: AdmissionAuthenticationRefreshRequired}
	}
	if g.circuit.state == CircuitHumanReviewRequired {
		return admissionCheck{code: AdmissionHumanAcknowledgementRequired}
	}
	if g.circuit.state == CircuitOpenUntil {
		if g.circuit.openUntil.IsZero() {
			return admissionCheck{code: AdmissionCircuitOpen}
		}
		if now.Before(g.circuit.openUntil) {
			return admissionCheck{code: AdmissionCircuitOpen, until: g.circuit.openUntil, delayed: true}
		}
		g.transitionCircuitLocked(CircuitClosed, "", time.Time{}, "")
	}
	if now.Before(g.cooldownUntil) {
		return admissionCheck{code: AdmissionCooldownActive, until: g.cooldownUntil, delayed: true}
	}
	if g.telemetry.upstreamCircuit == UpstreamCircuitOpen {
		if g.telemetry.cooldownUntil.After(now) {
			return admissionCheck{code: AdmissionCircuitOpen, until: g.telemetry.cooldownUntil, delayed: true}
		}
		if g.telemetry.cooldownUntil.IsZero() {
			return admissionCheck{code: AdmissionCircuitOpen}
		}
	}
	if g.telemetry.rateLimited || g.telemetry.capacityExhausted {
		if g.telemetry.resetAt.After(now) {
			return admissionCheck{code: AdmissionUpstreamAllowanceExhausted, until: g.telemetry.resetAt, delayed: true}
		}
		return admissionCheck{code: AdmissionUpstreamAllowanceExhausted}
	}
	if g.telemetry.available != nil {
		available := *g.telemetry.available - g.config.ManualReserve
		if available <= 0 {
			if g.telemetry.resetAt.After(now) {
				return admissionCheck{code: AdmissionUpstreamAllowanceExhausted, until: g.telemetry.resetAt, delayed: true}
			}
			return admissionCheck{code: AdmissionUpstreamAllowanceExhausted}
		}
	}
	state := g.taskLocked(request.TaskID)
	if state.attempts >= g.config.TaskBudget {
		return admissionCheck{code: AdmissionTaskBudgetExhausted}
	}
	if request.Retry && state.retries >= g.config.RetryBudget {
		return admissionCheck{code: AdmissionRetryBudgetExhausted}
	}
	if next := g.ledger.next(now, 3*time.Hour, g.config.Rolling3h); !next.IsZero() {
		return admissionCheck{code: AdmissionRollingBudgetExhausted, until: next, delayed: true}
	}
	if next := g.ledger.next(now, time.Hour, g.config.Rolling1h); !next.IsZero() {
		return admissionCheck{code: AdmissionRollingBudgetExhausted, until: next, delayed: true}
	}
	if next := g.ledger.next(now, 10*time.Minute, g.config.Rolling10m); !next.IsZero() {
		return admissionCheck{code: AdmissionRollingBudgetExhausted, until: next, delayed: true}
	}
	if !g.lastStart.IsZero() {
		next := g.lastStart.Add(g.config.MinimumStartInterval)
		if now.Before(next) {
			return admissionCheck{code: AdmissionDelayed, until: next, delayed: true}
		}
	}
	return admissionCheck{}
}

func (g *Governor) nextAvailableLocked(now time.Time) time.Time {
	if !g.lastStart.IsZero() {
		return g.lastStart.Add(g.config.MinimumStartInterval)
	}
	return now
}

func (g *Governor) eligibleIndexLocked() int {
	if len(g.queue) == 0 {
		return -1
	}
	if g.currentTask != "" && g.consecutiveTurns >= g.config.FairnessQuantum {
		for index, entry := range g.queue {
			if entry.request.TaskID != g.currentTask {
				return index
			}
		}
	}
	return 0
}

func (g *Governor) indexOfLocked(target *waiter) int {
	for index, entry := range g.queue {
		if entry == target {
			return index
		}
	}
	return -1
}

func (g *Governor) grantLocked(request AttemptRequest) *Permit {
	g.inFlight = true
	g.activeTaskID = request.TaskID
	return &Permit{governor: g, request: request}
}

func (g *Governor) removeWaiterLocked(target *waiter) {
	if target.removed {
		return
	}
	index := g.indexOfLocked(target)
	if index < 0 {
		target.removed = true
		g.releaseRequestLocked(target.request.ClientRequestID)
		g.signalLocked(target)
		return
	}
	target.removed = true
	g.releaseRequestLocked(target.request.ClientRequestID)
	g.queue = append(g.queue[:index], g.queue[index+1:]...)
	g.signalLocked(target)
	g.signalAllLocked()
}

func (g *Governor) signalLocked(target *waiter) {
	select {
	case <-target.notify:
	default:
		close(target.notify)
	}
	target.notify = make(chan struct{})
}

func (g *Governor) signalAllLocked() {
	for _, entry := range g.queue {
		g.signalLocked(entry)
	}
}

func (g *Governor) result(code, reason AdmissionCode, retryAt time.Time, err error) AdmissionResult {
	if err == nil && code != AdmissionAdmitted && code != AdmissionDelayed {
		err = &AdmissionError{Code: code}
	} else if err != nil {
		var admissionErr *AdmissionError
		if !errors.As(err, &admissionErr) {
			err = &AdmissionError{Code: code, Cause: err}
		}
	}
	return AdmissionResult{Code: code, Reason: reason, RetryAt: retryAt, Err: err}
}

func (g *Governor) resultLocked(code, reason AdmissionCode, retryAt time.Time, err error) AdmissionResult {
	return g.result(code, reason, retryAt, err)
}

func (g *Governor) delayedLocked(request AttemptRequest, reason AdmissionCode, retryAt time.Time, healthy bool) AdmissionResult {
	now := g.clock.Now()
	delay := time.Duration(0)
	if !retryAt.IsZero() && retryAt.After(now) {
		delay = retryAt.Sub(now)
	}
	result := AdmissionResult{Code: AdmissionDelayed, Reason: reason, RetryAt: retryAt, Delay: delay}
	g.emitAdmissionLocked(request, result, healthy)
	return result
}
