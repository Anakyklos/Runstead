package agent

// Issue #92 deterministic retry orchestration tests: a fake clock shared by
// the governor and the executor drives waits deterministically, the counting
// client proves one physical call per admission, and budgets/circuit/
// delivery/cancellation gate retries.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// ------------------------------ fakes ------------------------------

type fakeTimer struct {
	ch     chan time.Time
	once   sync.Once
	fired  atomic.Bool
	fireAt time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

// Stop may run concurrently with trigger (the executor's deferred Stop races
// with the clock goroutine firing the timer when context and timer become due
// at the same moment); fired must therefore be atomic, not a plain bool.
func (t *fakeTimer) Stop() bool { return !t.fired.Load() }

func (t *fakeTimer) trigger() {
	t.once.Do(func() { t.fired.Store(true); t.ch <- t.fireAt })
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// registrations signals EVERY NewTimer registration (buffered,
	// non-blocking sender): a test waiting on awaitTimerRegistered learns
	// that a timer was armed BEFORE any Advance, closing the
	// waitForCalls -> Advance race (issue #115). The signal is an
	// observable harness event, never a wall-clock substitute.
	registrations chan struct{}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(delay time.Duration) governor.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{ch: make(chan time.Time, 1), fireAt: c.now.Add(delay)}
	c.timers = append(c.timers, timer)
	select {
	case c.registrations <- struct{}{}:
	default:
		// No waiter at this instant: the event is consumed by the next
		// await. The executor never blocks on the signal.
	}
	return timer
}

// awaitTimerRegistered blocks until a timer has been registered with the
// clock and returns it. This is the deterministic synchronization for the
// retry tests: after it returns, the caller KNOWS the backoff timer was
// armed at the clock time BEFORE any Advance, so advancing the clock fires
// it. Ordering guarantee in this harness: the first (and, until the retry
// admission, only) timer registered after the first physical call is the
// executor's retry-backoff timer; governor admission pacing timers can only
// appear at the RETRY admission, which happens after the backoff
// registration. The wall-clock select below is a FAILURE GUARD ONLY: it
// never participates in a passing test's synchronization.
func (c *fakeClock) awaitTimerRegistered(t *testing.T) *fakeTimer {
	t.Helper()
	select {
	case <-c.registrations:
	case <-time.After(5 * time.Second):
		t.Fatal("fake clock: no timer was registered within the failure-guard window")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		t.Fatal("fake clock: registration signaled but no timer recorded")
	}
	return c.timers[len(c.timers)-1]
}

func (c *fakeClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	due := make([]*fakeTimer, 0)
	for _, timer := range c.timers {
		if !timer.fired.Load() && !timer.fireAt.After(c.now) {
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.trigger()
	}
}

type fixedJitter struct{}

func (fixedJitter) Apply(base time.Duration, _ int) time.Duration { return base }

type scriptedClient struct {
	mu       sync.Mutex
	results  []clientResult
	calls    int
	requests []provider.Request
}

type clientResult struct {
	response provider.Response
	err      error
}

func newScriptedClient(results ...clientResult) *scriptedClient {
	return &scriptedClient{results: results}
}

func (c *scriptedClient) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.requests = append(c.requests, request)
	if len(c.results) == 0 {
		return provider.Response{}, errors.New("no scripted result")
	}
	index := c.calls - 1
	if index >= len(c.results) {
		index = len(c.results) - 1
	}
	return c.results[index].response, c.results[index].err
}

func (c *scriptedClient) RouteSafety() provider.RouteSafety { return provider.SafeRouteSafety() }

func (c *scriptedClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *scriptedClient) requestIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.requests))
	for _, request := range c.requests {
		ids = append(ids, request.ClientRequestID)
	}
	return ids
}

func planClassifier(classes ...governor.OutcomeClass) governor.OutcomeClassifier {
	cursor := 0
	return func(response provider.Response, err error) governor.Outcome {
		class := classes[cursor]
		if cursor < len(classes)-1 {
			cursor++
		}
		return governor.Outcome{Class: class, DeliveryState: response.Metadata.DeliveryState, UpstreamReached: true}
	}
}

// newRetryHarness builds a governor + executor sharing one fake clock.
func newRetryHarness(t *testing.T, mutate func(*governor.Config), client provider.Client, classifier governor.OutcomeClassifier, options ExecutorOptions) (*governor.Governor, *fakeClock, *Executor) {
	t.Helper()
	return newRetryHarnessWithPersistence(t, mutate, nil, client, classifier, options)
}

// newRetryHarnessWithPersistence builds the same harness with an explicit
// governor persistence boundary (nil disables durable state).
func newRetryHarnessWithPersistence(t *testing.T, mutate func(*governor.Config), persistence governor.Persistence, client provider.Client, classifier governor.OutcomeClassifier, options ExecutorOptions) (*governor.Governor, *fakeClock, *Executor) {
	t.Helper()
	config := governor.DefaultInstantConfig("policy-test", "provider-a", "instant", provider.SafeRouteSafety())
	config.RetryBudget = 2
	config.MinimumStartInterval = time.Millisecond
	if mutate != nil {
		mutate(&config)
	}
	clock := &fakeClock{now: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), registrations: make(chan struct{}, 16)}
	gov, err := governor.New(config, governor.Options{Clock: clock, Jitter: fixedJitter{}, Persistence: persistence})
	if err != nil {
		t.Fatal(err)
	}
	options.Clock = clock
	executor, err := NewExecutor(gov, client, classifier, options)
	if err != nil {
		t.Fatal(err)
	}
	return gov, clock, executor
}

// failFinishedPersistence commits TX 1 (prepared) but fails TX 2 (finished):
// the physical effect was observed, but its classified outcome was never
// durably recorded, leaving the attempt 'prepared'/ambiguous in the store.
type failFinishedPersistence struct{}

func (failFinishedPersistence) RecordProviderPrepared(_ context.Context, _ governor.ProviderPrepared) error {
	return nil
}

func (failFinishedPersistence) RecordProviderFinished(_ context.Context, _ governor.ProviderFinished) error {
	return errors.New("tx2 write failed")
}

func retryRequest() governor.AttemptRequest {
	return governor.AttemptRequest{
		TaskID:          "task-retry",
		ClientRequestID: "task-retry-0001",
		ModelPool:       "instant",
		ProviderRequest: provider.Request{Model: "model-a", Prompt: "prompt"},
	}
}

func rateClientResults() []clientResult {
	return []clientResult{
		{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate")},
		{response: provider.Response{Text: "ok", Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}},
	}
}

// rateThenSuccessClassifier returns rate_or_capacity (with Retry-After) for
// the first physical call and success afterwards.
func rateThenSuccessClassifier(client *scriptedClient) governor.OutcomeClassifier {
	return func(response provider.Response, err error) governor.Outcome {
		if client.callCount() == 1 {
			return governor.Outcome{Class: governor.OutcomeRateCapacity, RetryAfter: time.Second, DeliveryState: response.Metadata.DeliveryState, UpstreamReached: true}
		}
		return governor.Outcome{Class: governor.OutcomeSuccess, DeliveryState: response.Metadata.DeliveryState, UpstreamReached: true}
	}
}

func waitForCalls(t *testing.T, client *scriptedClient, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if client.callCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d physical calls (got %d)", want, client.callCount())
}

// ------------------------------ tests ------------------------------

// TestExecutorRetryUsesNewAdmissionAndAccounting: 429 + Retry-After leads to
// exactly ONE retry; the retry is a SECOND physical call under a SECOND
// client request id (new admission) and both attempts are debited.
func TestExecutorRetryUsesNewAdmissionAndAccounting(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, clock, executor := newRetryHarness(t, nil, client, rateThenSuccessClassifier(client), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.awaitTimerRegistered(t)
	clock.Advance(2 * time.Second)

	result := <-done
	if result.Completion.Outcome != governor.OutcomeSuccess {
		t.Fatalf("final outcome = %q, want success (err=%v)", result.Completion.Outcome, result.Err)
	}
	if calls := client.callCount(); calls != 2 {
		t.Fatalf("physical calls = %d, want exactly 2 (one per admission)", calls)
	}
	ids := client.requestIDs()
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("client request ids must be distinct per attempt: %v", ids)
	}
}

// TestExecutorRetryBudgetExhaustion: RetryBudget=1 allows exactly one retry;
// exhaustion terminates deterministically.
func TestExecutorRetryBudgetExhaustion(t *testing.T) {
	client := newScriptedClient(
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate-1")},
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate-2")},
	)
	_, clock, executor := newRetryHarness(t, func(config *governor.Config) { config.RetryBudget = 1 }, client,
		planClassifier(governor.OutcomeRateCapacity, governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.awaitTimerRegistered(t)
	clock.Advance(20 * time.Second)

	result := <-done
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("final outcome = %q, want rate_or_capacity after budget exhaustion", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 2 {
		t.Fatalf("physical calls = %d, want 2 (budget=1)", calls)
	}
}

// TestExecutorRetryDisabledByDefault: without EnableRetry the historical
// behavior is preserved — a rate-limited attempt is returned, never retried.
func TestExecutorRetryDisabledByDefault(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, _, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{})
	result := executor.Execute(context.Background(), retryRequest())
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("outcome = %q, want rate_or_capacity", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (retry disabled by default)", calls)
	}
}

// TestExecutorNeverRetriesNonRetryableClasses covers auth, permission,
// unknown classification and delivery-unsafe outcomes: one physical call.
func TestExecutorNeverRetriesNonRetryableClasses(t *testing.T) {
	cases := []struct {
		name     string
		class    governor.OutcomeClass
		delivery provider.DeliveryState
	}{
		{"authentication denied", governor.OutcomeAuthenticationDenied, provider.DeliveryCompleted},
		{"http 403 permission", governor.OutcomeHTTP403, provider.DeliveryCompleted},
		{"unknown classification", governor.OutcomeUncertainReached, provider.DeliveryCompleted},
		{"timeout after possible dispatch", governor.OutcomeTimeout, provider.DeliverySentUnconfirmed},
		{"connection reset after dispatch", governor.OutcomeConnectionReset, provider.DeliverySentUnconfirmed},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			client := newScriptedClient(clientResult{
				response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: testCase.delivery}},
				err:      errors.New("fail"),
			})
			_, _, executor := newRetryHarness(t, nil, client, planClassifier(testCase.class), ExecutorOptions{EnableRetry: true})
			result := executor.Execute(context.Background(), retryRequest())
			// Delivery-unsafe classes must downgrade to uncertain (never
			// retryable); other non-retryable classes pass through.
			if calls := client.callCount(); calls != 1 {
				t.Fatalf("physical calls = %d, want 1 (never retry)", calls)
			}
			_ = result
		})
	}
}

// TestExecutorCancelDuringBackoffNoDispatchNoDebit: cancelling during the
// backoff prevents the next attempt; the never-started attempt is not
// debited, and advancing the clock afterwards produces no dispatch.
func TestExecutorCancelDuringBackoffNoDispatchNoDebit(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	gov, clock, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	// The backoff timer is provably registered before the cancel is issued,
	// so the test exercises the intended race: a registered timer whose
	// context is cancelled before it fires.
	clock.awaitTimerRegistered(t)

	cancel()
	clock.Advance(10 * time.Second)

	result := <-done
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (cancelled attempt never started)", calls)
	}
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("returned outcome = %q, want the original attempt outcome", result.Completion.Outcome)
	}
	tasks := gov.Snapshot().Tasks["task-retry"]
	if tasks.Attempts != 1 || tasks.Retries != 0 {
		t.Fatalf("task accounting = attempts %d retries %d, want 1/0 (cancelled retry never debited)", tasks.Attempts, tasks.Retries)
	}
}

// TestExecutorProfileCooldownInput: the durable OperationalProfile cooldown
// input (#91) extends the backoff beyond the Retry-After.
func TestExecutorProfileCooldownInput(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, clock, executor := newRetryHarness(t, nil, client, rateThenSuccessClassifier(client), ExecutorOptions{
		EnableRetry:          true,
		RetryProfileCooldown: func() time.Duration { return 10 * time.Second },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.awaitTimerRegistered(t)
	clock.Advance(2 * time.Second)
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls after 2s = %d, want 1 (profile cooldown still pending)", calls)
	}
	clock.Advance(8 * time.Second)

	result := <-done
	if result.Completion.Outcome != governor.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success after profile cooldown", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 2 {
		t.Fatalf("physical calls = %d, want 2", calls)
	}
}

// TestExecutorCircuitOpenNeverRetries: an open circuit makes the completion
// retry-ineligible; exactly one physical call happens.
func TestExecutorCircuitOpenNeverRetries(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, _, executor := newRetryHarness(t, func(config *governor.Config) {
		config.RateResponseThreshold = 1
		config.RateResponseWindow = time.Minute
	}, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true})
	result := executor.Execute(context.Background(), retryRequest())
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("outcome = %q", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (circuit open blocks retry)", calls)
	}
}

// TestExecutorContextDeadlineDuringBackoff: a real deadline stops the wait
// even though the fake-clock timer never fires.
func TestExecutorContextDeadlineDuringBackoff(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, _, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	select {
	case result := <-done:
		if calls := client.callCount(); calls != 1 {
			t.Fatalf("physical calls = %d, want 1 (deadline stopped the retry)", calls)
		}
		if result.Completion.Outcome != governor.OutcomeRateCapacity {
			t.Fatalf("outcome = %q", result.Completion.Outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Execute did not return after deadline")
	}
}

// TestExecutorTimerFiresOnceNoDuplicateDispatch: the backoff timer fires at
// most once (Stop + single-fire), so advancing the fake clock repeatedly
// after the wake-up can never issue another physical dispatch from the same
// admission.
func TestExecutorTimerFiresOnceNoDuplicateDispatch(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	_, clock, executor := newRetryHarness(t, nil, client, rateThenSuccessClassifier(client), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.awaitTimerRegistered(t)
	clock.Advance(time.Second)
	if result := <-done; result.Completion.Outcome != governor.OutcomeSuccess {
		t.Fatalf("outcome = %q", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 2 {
		t.Fatalf("physical calls = %d, want 2", calls)
	}
	// Advancing again must not fire a second timer or produce a third call.
	clock.Advance(5 * time.Second)
	if calls := client.callCount(); calls != 2 {
		t.Fatalf("physical calls after extra clock advance = %d, want still 2", calls)
	}
}

// TestExecutorRetryStopsWhenFinishedPersistFails: TX 1 (prepared) commits but
// TX 2 (finished) fails on an otherwise retryable outcome. The physical
// attempt stays 'prepared'/ambiguous in the store, so automatic retry must
// stop even though the computed completion was still retry-eligible: exactly
// one provider call, no second admission/dispatch, no retry debit, and the
// original ambiguous result (ErrProviderOutcomePersist) is returned.
func TestExecutorRetryStopsWhenFinishedPersistFails(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	gov, clock, executor := newRetryHarnessWithPersistence(t, nil, failFinishedPersistence{}, client,
		rateThenSuccessClassifier(client), ExecutorOptions{EnableRetry: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := executor.Execute(ctx, retryRequest())
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (TX2 failure must stop retry despite retry-eligible completion)", calls)
	}
	if !errors.Is(result.Err, governor.ErrProviderOutcomePersist) {
		t.Fatalf("result.Err = %v, want ErrProviderOutcomePersist", result.Err)
	}
	if !result.Completion.RetryEligible {
		t.Fatalf("completion must still be retry-eligible so this regression proves the persistence guard is what stops the retry")
	}
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("outcome = %q, want the original rate outcome", result.Completion.Outcome)
	}
	tasks := gov.Snapshot().Tasks["task-retry"]
	if tasks.Attempts != 1 || tasks.Retries != 0 {
		t.Fatalf("task accounting = attempts %d retries %d, want 1/0 (no retry debit)", tasks.Attempts, tasks.Retries)
	}
	// Advancing the clock after the stop must never awaken a retry dispatch.
	clock.Advance(30 * time.Second)
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls after clock advance = %d, want still 1", calls)
	}
}
