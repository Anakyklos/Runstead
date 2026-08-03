package policy

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type taskState struct {
	attempts int
	retries  int
}

type waiter struct {
	request  AttemptRequest
	notify   chan struct{}
	checking bool
	removed  bool
}

type circuitData struct {
	state           CircuitState
	reason          OutcomeClass
	openUntil       time.Time
	refreshRequired bool
	refreshInFlight bool
	lastRateReset   time.Time
	rateEvents      []time.Time
}

type telemetryState struct {
	available         *int
	resetAt           time.Time
	cooldownUntil     time.Time
	rateLimited       bool
	capacityExhausted bool
	upstreamCircuit   UpstreamCircuitState
	unsafe            bool
}

type Governor struct {
	mu              sync.Mutex
	config          Config
	clock           Clock
	jitter          Jitter
	telemetrySource TelemetrySource
	events          EventSink

	closed           bool
	inFlight         bool
	queue            []*waiter
	currentTask      string
	consecutiveTurns int
	lastStart        time.Time
	nextAttempt      int
	ledger           rollingLedger
	tasks            map[string]*taskState
	seenRequestIDs   map[string]struct{}
	circuit          circuitData
	cooldownUntil    time.Time
	telemetry        telemetryState
}

func New(config Config, options Options) (*Governor, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = realClock{}
	}
	jitter := options.Jitter
	if jitter == nil {
		jitter = &randomJitter{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	}
	return &Governor{
		config:          config,
		clock:           clock,
		jitter:          jitter,
		telemetrySource: options.Telemetry,
		events:          options.Events,
		nextAttempt:     1,
		tasks:           make(map[string]*taskState),
		seenRequestIDs:  make(map[string]struct{}),
		circuit:         circuitData{state: CircuitClosed},
	}, nil
}

func (g *Governor) Config() Config {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.config
}

func (g *Governor) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	for _, entry := range g.queue {
		entry.removed = true
		g.signalLocked(entry)
	}
	g.queue = nil
	g.mu.Unlock()
}

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
		AccountPolicyID:  g.config.AccountPolicyID,
		ProviderID:       g.config.ProviderID,
		ModelPool:        g.config.ModelPool,
		AllowanceProfile: g.config.AllowanceProfile,
		InFlight:         g.inFlight,
		QueueLength:      len(g.queue),
		NextAttempt:      g.nextAttempt,
		LastStart:        g.lastStart,
		CooldownUntil:    g.cooldownUntil,
		Budgets:          g.budgetLocked(now, ""),
		Circuit:          g.circuitSnapshotLocked(),
		Telemetry:        g.telemetrySummaryLocked(),
		Tasks:            tasks,
	}
}

func (g *Governor) Admit(ctx context.Context, request AttemptRequest) AdmissionResult {
	return g.admit(ctx, request, true)
}

// TryAdmit returns Delayed instead of waiting when pacing, a cooldown, a
// rolling window or an occupied lane would defer the request.
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
	return &Permit{governor: g, request: request}
}

func (g *Governor) requestSeenLocked(requestID string) bool {
	_, seen := g.seenRequestIDs[requestID]
	return seen
}

func (g *Governor) reserveRequestLocked(requestID string) {
	g.seenRequestIDs[requestID] = struct{}{}
}

func (g *Governor) releaseRequestLocked(requestID string) {
	delete(g.seenRequestIDs, requestID)
}

func (g *Governor) taskLocked(taskID string) *taskState {
	state := g.tasks[taskID]
	if state == nil {
		state = &taskState{}
		g.tasks[taskID] = state
	}
	return state
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
	g.telemetry.upstreamCircuit = snapshot.UpstreamCircuit
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
		g.telemetry.upstreamCircuit = UpstreamCircuitUnknown
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
	g.events.Emit(event)
}

func (g *Governor) transitionCircuitLocked(state CircuitState, reason OutcomeClass, openUntil time.Time, _ string) {
	from := g.circuit.state
	g.circuit.state = state
	g.circuit.reason = reason
	g.circuit.openUntil = openUntil
	if g.events != nil && from != state {
		g.events.Emit(Event{
			Kind:             EventCircuit,
			AccountPolicyID:  g.config.AccountPolicyID,
			ProviderID:       g.config.ProviderID,
			ModelPool:        g.config.ModelPool,
			AllowanceProfile: g.config.AllowanceProfile,
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
	if outcome.Class == OutcomeCancelledBeforeUpstream {
		outcome.Class = OutcomeUncertainReached
	}
	beforeCircuit := g.circuitSnapshotLocked()
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
	}
	state := g.taskLocked(permit.request.TaskID)
	recoverable := outcome.Class == OutcomeRateCapacity || outcome.Class == OutcomeConnectionReset || outcome.Class == OutcomeTimeout || outcome.Class == OutcomeEmptyResponse || outcome.Class == OutcomeMalformedUpstream || outcome.Class == OutcomeUpstreamServerFailure
	result.RetryEligible = recoverable && state.retries < g.config.RetryBudget && g.circuit.state == CircuitClosed
	if beforeCircuit.State != result.Circuit.State && g.events != nil {
		g.events.Emit(Event{
			Kind:             EventCircuit,
			AccountPolicyID:  g.config.AccountPolicyID,
			ProviderID:       g.config.ProviderID,
			ModelPool:        g.config.ModelPool,
			AllowanceProfile: g.config.AllowanceProfile,
			TaskID:           permit.request.TaskID,
			ClientRequestID:  permit.request.ClientRequestID,
			AttemptSequence:  permit.attemptSequence,
			Outcome:          outcome.Class,
			CircuitFrom:      beforeCircuit.State,
			CircuitTo:        result.Circuit.State,
			CircuitReason:    outcome.Class,
			CooldownUntil:    g.cooldownUntil,
			SelectedBackoff:  selected,
		})
	}
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

func (g *Governor) Execute(ctx context.Context, request AttemptRequest, client provider.Client, classifier OutcomeClassifier) ExecutionResult {
	if client == nil {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("provider client is required"))}
	}
	aware, ok := client.(provider.SafetyAware)
	if !ok {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeProviderAmplification, AdmissionUnsafeProviderAmplification, time.Time{}, provider.ErrUnsafeRoute)}
	}
	safety := aware.RouteSafety()
	if err := safety.Validate(); err != nil || !safety.Equal(g.config.RouteSafety) {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeProviderAmplification, AdmissionUnsafeProviderAmplification, time.Time{}, provider.ErrUnsafeRoute)}
	}
	admission := g.Admit(ctx, request)
	if !admission.Admitted() {
		return ExecutionResult{Admission: admission, Err: admission.Err}
	}
	if err := ctx.Err(); err != nil {
		admission.Permit.CancelBeforeStart()
		admission.Code = contextAdmissionCode(ctx)
		admission.Err = &AdmissionError{Code: admission.Code, Cause: err}
		return ExecutionResult{Admission: admission, Err: admission.Err}
	}
	if err := admission.Permit.Start(); err != nil {
		return ExecutionResult{Admission: admission, Err: err}
	}
	response, callErr := client.Complete(ctx, request.ProviderRequest)
	if classifier == nil {
		classifier = defaultOutcome
	}
	outcome := classifier(response, callErr)
	if outcome.Class == OutcomeCancelledBeforeUpstream {
		outcome.Class = OutcomeUncertainReached
	}
	completion := admission.Permit.Finish(outcome)
	return ExecutionResult{Admission: admission, Response: response, Completion: completion, Err: callErr}
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
	p.attemptSequence = g.nextAttempt
	g.nextAttempt++
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
	if g.events != nil {
		g.events.Emit(Event{
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
	defer g.mu.Unlock()
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
	g.inFlight = false
	g.signalAllLocked()
	if g.events != nil {
		g.events.Emit(Event{
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
	}
	return result
}
