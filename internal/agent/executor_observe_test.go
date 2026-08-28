package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// recordingObserver records every observed attempt and optionally fails on a
// specific observation (1-based).
type recordingObserver struct {
	mu           sync.Mutex
	errOn        int
	observations []governor.AttemptRequest
	results      []governor.ExecutionResult
}

func (o *recordingObserver) ObserveAttempt(_ context.Context, request governor.AttemptRequest, result governor.ExecutionResult) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observations = append(o.observations, request)
	o.results = append(o.results, result)
	if o.errOn > 0 && len(o.observations) == o.errOn {
		return errors.New("observer persistence failed")
	}
	return nil
}

func (o *recordingObserver) observed() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.observations)
}

// TestExecutorObserverCalledExactlyOncePerAdmittedAttempt: with retry
// enabled, a rate-limited attempt followed by success produces two physical
// attempts and exactly two observations, each with its own request identity.
func TestExecutorObserverCalledExactlyOncePerAdmittedAttempt(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	observer := &recordingObserver{}
	_, clock, executor := newRetryHarness(t, nil, client, rateThenSuccessClassifier(client), ExecutorOptions{EnableRetry: true, Observer: observer})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.Advance(2 * time.Second)
	result := <-done

	if result.Completion.Outcome != governor.OutcomeSuccess {
		t.Fatalf("final outcome = %q, want success (err=%v)", result.Completion.Outcome, result.Err)
	}
	if got := observer.observed(); got != 2 {
		t.Fatalf("observations = %d, want exactly 2 (one per admitted attempt)", got)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	first, second := observer.observations[0], observer.observations[1]
	if first.Retry || !second.Retry {
		t.Fatalf("retry flags = %v/%v, want false/true", first.Retry, second.Retry)
	}
	if first.ClientRequestID == second.ClientRequestID {
		t.Fatalf("each observed attempt must carry its own client request id: %q", first.ClientRequestID)
	}
	if second.ClientRequestID != retryRequest().ClientRequestID+"-r1" {
		t.Fatalf("second attempt client request id = %q, want the retry suffix", second.ClientRequestID)
	}
}

// TestExecutorObserverRunsWithRetryDisabled: learning is independent of
// retry; an admitted rate-limited attempt is observed once even when retry
// orchestration is disabled.
func TestExecutorObserverRunsWithRetryDisabled(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	observer := &recordingObserver{}
	_, _, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{Observer: observer})
	result := executor.Execute(context.Background(), retryRequest())
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("outcome = %q, want rate_or_capacity", result.Completion.Outcome)
	}
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (retry disabled)", calls)
	}
	if got := observer.observed(); got != 1 {
		t.Fatalf("observations = %d, want 1 (observation independent of retry)", got)
	}
}

// TestExecutorObserverErrorStopsRetryConservatively: when the observer
// cannot safely persist what it learned, no further physical attempt may
// start even though the outcome would have been retry-eligible.
func TestExecutorObserverErrorStopsRetryConservatively(t *testing.T) {
	client := newScriptedClient(
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate-1")},
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate-2")},
	)
	observer := &recordingObserver{errOn: 1}
	_, clock, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true, Observer: observer})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.Advance(10 * time.Second)
	result := <-done

	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (observer failure must stop retry)", calls)
	}
	if !errors.Is(result.Err, ErrAttemptObservation) {
		t.Fatalf("result error must wrap ErrAttemptObservation, got %v", result.Err)
	}
}

// TestExecutorObserverNotCalledOnDeniedAdmission: admission denials are not
// physical attempts and are never observed.
func TestExecutorObserverNotCalledOnDeniedAdmission(t *testing.T) {
	client := newScriptedClient(
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: nil},
	)
	observer := &recordingObserver{}
	// A long minimum start interval denies any admission issued immediately
	// after the first one on the same lane.
	_, _, executor := newRetryHarness(t, func(config *governor.Config) { config.MinimumStartInterval = time.Hour }, client,
		planClassifier(governor.OutcomeSuccess), ExecutorOptions{Observer: observer, EnableRetry: true})

	first := executor.Execute(context.Background(), retryRequest())
	if !first.Admission.Admitted() {
		t.Fatalf("first admission must be admitted, got %+v", first.Admission)
	}
	second := executor.Execute(context.Background(), retryRequest())
	if second.Admission.Admitted() {
		t.Fatalf("second admission must be denied by the minimum start interval, got %+v", second.Admission)
	}
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (denied admission never dispatches)", calls)
	}
	if got := observer.observed(); got != 1 {
		t.Fatalf("observations = %d, want 1 (only the admitted attempt)", got)
	}
}

// TestExecutorObserverCancelDuringBackoffObservesOnce: a cancel during the
// retry backoff stops the loop after the first attempt; exactly one
// observation happened (the never-started attempt is not observed).
func TestExecutorObserverCancelDuringBackoffObservesOnce(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	observer := &recordingObserver{}
	_, clock, executor := newRetryHarness(t, nil, client, planClassifier(governor.OutcomeRateCapacity), ExecutorOptions{EnableRetry: true, Observer: observer})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	cancel()
	clock.Advance(10 * time.Second)
	result := <-done

	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (cancelled retry never starts)", calls)
	}
	if got := observer.observed(); got != 1 {
		t.Fatalf("observations = %d, want exactly 1", got)
	}
	if result.Completion.Outcome != governor.OutcomeRateCapacity {
		t.Fatalf("outcome = %q, want the original attempt outcome", result.Completion.Outcome)
	}
}

// TestExecutorObserverErrorPreservesPersistenceSentinel: when the observer
// itself fails on a control-plane persistence sentinel (TX2 store error),
// the terminal error must keep both markers: the observation stopped the
// loop conservatively AND the underlying persistence cause stays
// detectable, so the CLI mapping can still classify it as a control-plane
// failure that must never learn.
func TestExecutorObserverErrorPreservesPersistenceSentinel(t *testing.T) {
	client := newScriptedClient(rateClientResults()...)
	observer := &recordingObserver{errOn: 1}
	_, clock, executor := newRetryHarnessWithPersistence(t, nil, failFinishedPersistence{}, client,
		rateThenSuccessClassifier(client), ExecutorOptions{EnableRetry: true, Observer: observer})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.Advance(2 * time.Second)
	result := <-done

	if !errors.Is(result.Err, ErrAttemptObservation) {
		t.Fatalf("result error must wrap ErrAttemptObservation, got %v", result.Err)
	}
	if !errors.Is(result.Err, governor.ErrProviderOutcomePersist) {
		t.Fatalf("result error must preserve ErrProviderOutcomePersist, got %v", result.Err)
	}
	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (observer error stops retry)", calls)
	}
	if got := observer.observed(); got != 1 {
		t.Fatalf("observations = %d, want exactly 1", got)
	}
}

// TestExecutorObserverOnFailedDurableFinishStillNoRetry: a failed TX2
// durable finish (issue #92 sentinel) with an observer attached must still
// stop retrying; the admitted attempt is observed exactly once (the
// observer cannot know it was control-plane ambiguous, so the CLI mapping
// treats the non-adapter error as ambiguous and learns nothing).
func TestExecutorObserverOnFailedDurableFinishStillNoRetry(t *testing.T) {
	client := newScriptedClient(
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: errors.New("rate-1")},
		clientResult{response: provider.Response{Metadata: provider.ResponseMetadata{DeliveryState: provider.DeliveryCompleted}}, err: nil},
	)
	observer := &recordingObserver{}
	_, clock, executor := newRetryHarnessWithPersistence(t, nil, failFinishedPersistence{}, client,
		planClassifier(governor.OutcomeRateCapacity, governor.OutcomeSuccess), ExecutorOptions{EnableRetry: true, Observer: observer})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan governor.ExecutionResult, 1)
	go func() { done <- executor.Execute(ctx, retryRequest()) }()
	waitForCalls(t, client, 1)
	clock.Advance(10 * time.Second)
	result := <-done

	if calls := client.callCount(); calls != 1 {
		t.Fatalf("physical calls = %d, want 1 (failed durable finish must stop retry)", calls)
	}
	if !errors.Is(result.Err, governor.ErrProviderOutcomePersist) {
		t.Fatalf("result error must preserve ErrProviderOutcomePersist, got %v", result.Err)
	}
	if got := observer.observed(); got != 1 {
		t.Fatalf("observations = %d, want exactly 1 (the admitted attempt)", got)
	}
}
