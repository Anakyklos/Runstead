package governor_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type fakeTimer struct {
	clock *fakeClock
	at    time.Time
	c     chan time.Time
	mu    sync.Mutex
	stop  bool
	fired bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop || t.fired {
		return false
	}
	t.stop = true
	return true
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(delay time.Duration) policy.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{clock: c, at: c.now.Add(delay), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	timers := append([]*fakeTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.mu.Lock()
		if !timer.stop && !timer.fired && !now.Before(timer.at) {
			timer.fired = true
			timer.c <- now
		}
		timer.mu.Unlock()
	}
}

type fixedJitter struct{ values []time.Duration }

func (j fixedJitter) Apply(base time.Duration, sequence int) time.Duration {
	if len(j.values) == 0 {
		return base
	}
	return j.values[(sequence-1)%len(j.values)]
}

type eventSink struct {
	mu     sync.Mutex
	events []policy.Event
}

func (s *eventSink) Emit(event policy.Event) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *eventSink) Events() []policy.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]policy.Event(nil), s.events...)
}

type snapshotSink struct {
	governor *policy.Governor
}

func (s *snapshotSink) Emit(policy.Event) {
	_ = s.governor.Snapshot()
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSink) Emit(policy.Event) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
}

type fakeTelemetry struct {
	mu       sync.Mutex
	snapshot policy.TelemetrySnapshot
	err      error
	calls    int
}

func (t *fakeTelemetry) Snapshot(context.Context) (policy.TelemetrySnapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	return t.snapshot, t.err
}

func (t *fakeTelemetry) Set(snapshot policy.TelemetrySnapshot) {
	t.mu.Lock()
	t.snapshot = snapshot
	t.mu.Unlock()
}

func instantGovernor(t *testing.T) (*policy.Governor, *fakeClock, *eventSink) {
	t.Helper()
	clock := newFakeClock()
	events := &eventSink{}
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	governor, err := policy.New(config, policy.Options{
		Clock:  clock,
		Jitter: fixedJitter{},
		Events: events,
	})
	if err != nil {
		t.Fatalf("policy.New() error = %v", err)
	}
	return governor, clock, events
}

func fastConfig(t *testing.T) policy.Config {
	t.Helper()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Rolling3h = 100
	config.Rolling1h = 90
	config.Rolling10m = 80
	config.ManualReserve = 10
	config.TaskBudget = 4
	config.RetryBudget = 2
	return config
}

func admitAndFinish(t *testing.T, governor *policy.Governor, task string, request string, retry bool, outcome policy.Outcome) policy.FinishResult {
	t.Helper()
	admission := governor.Admit(context.Background(), policy.AttemptRequest{
		TaskID:          task,
		ClientRequestID: request,
		Retry:           retry,
	})
	if !admission.Admitted() {
		t.Fatalf("Admit() = %#v, want admitted", admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatalf("Permit.Start() error = %v", err)
	}
	return admission.Permit.Finish(outcome)
}

func tryAdmitAndFinish(t *testing.T, governor *policy.Governor, task string, request string, retry bool, outcome policy.Outcome) policy.FinishResult {
	t.Helper()
	admission := governor.TryAdmit(context.Background(), policy.AttemptRequest{
		TaskID:          task,
		ClientRequestID: request,
		Retry:           retry,
	})
	if !admission.Admitted() {
		t.Fatalf("TryAdmit() = %#v, want admitted", admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatalf("Permit.Start() error = %v", err)
	}
	return admission.Permit.Finish(outcome)
}

func waitForQueue(t *testing.T, governor *policy.Governor, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if governor.Snapshot().QueueLength >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("queue length = %d, want at least %d", governor.Snapshot().QueueLength, want)
}

func TestDefaultInstantProfileAndValidation(t *testing.T) {
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultInstantConfig.Validate() error = %v", err)
	}
	wants := map[string]any{
		"profile":  config.AllowanceProfile,
		"3h":       config.Rolling3h,
		"reserve":  config.ManualReserve,
		"1h":       config.Rolling1h,
		"10m":      config.Rolling10m,
		"task":     config.TaskBudget,
		"retry":    config.RetryBudget,
		"queue":    config.QueueCapacity,
		"interval": config.MinimumStartInterval,
		"flight":   config.MaxInFlight,
	}
	if wants["profile"] != policy.AllowanceProfileInstant || wants["3h"] != 140 || wants["reserve"] != 20 || wants["1h"] != 80 || wants["10m"] != 25 || wants["task"] != 80 || wants["retry"] != 2 || wants["queue"] != 16 || wants["interval"] != 5*time.Second || wants["flight"] != 1 {
		t.Fatalf("Instant defaults = %#v", wants)
	}
	for name, mutate := range map[string]func(*policy.Config){
		"missing account policy": func(c *policy.Config) { c.AccountPolicyID = "" },
		"missing provider":       func(c *policy.Config) { c.ProviderID = "" },
		"missing model pool":     func(c *policy.Config) { c.ModelPool = "" },
		"parallel account":       func(c *policy.Config) { c.MaxInFlight = 2 },
		"unsafe route":           func(c *policy.Config) { c.RouteSafety = provider.RouteSafety{} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := config
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want fail-closed error")
			}
		})
	}
}

func TestReasoningProfileRequiresItsOwnExplicitCeilings(t *testing.T) {
	config := policy.Config{
		AccountPolicyID:       "policy-account-1",
		ProviderID:            "omniroute",
		ModelPool:             "reasoning",
		AllowanceProfile:      policy.AllowanceProfileReasoning,
		Rolling3h:             40,
		ManualReserve:         8,
		Rolling1h:             20,
		Rolling10m:            8,
		TaskBudget:            10,
		RetryBudget:           1,
		QueueCapacity:         16,
		FairnessQuantum:       1,
		MinimumStartInterval:  5 * time.Second,
		BurstCapacity:         1,
		MaxInFlight:           1,
		RequireSingleAttempt:  true,
		RateResponseThreshold: 3,
		RateResponseWindow:    time.Hour,
		ResetSafetyMargin:     5 * time.Minute,
		RouteSafety:           provider.SafeRouteSafety(),
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("reasoning Validate() error = %v", err)
	}
	if config.Rolling3h == 140 || config.ManualReserve == 20 {
		t.Fatalf("reasoning profile inherited Instant defaults: %#v", config)
	}
	config.Rolling3h = 0
	if err := config.Validate(); err == nil {
		t.Fatal("incomplete reasoning profile was accepted")
	}
}

func TestTwoTasksNeverHaveTwoAttemptsInFlight(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}
	if err := first.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	secondResult := make(chan policy.AdmissionResult, 1)
	go func() {
		secondResult <- governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
	}()
	waitForQueue(t, governor, 1)
	select {
	case result := <-secondResult:
		t.Fatalf("second task admitted while first was in flight: %#v", result)
	default:
	}
	if got := governor.Snapshot().InFlight; !got {
		t.Fatal("snapshot says no attempt is in flight")
	}
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(5 * time.Second)
	select {
	case result := <-secondResult:
		if !result.Admitted() {
			t.Fatalf("second admission = %#v", result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	case <-time.After(time.Second):
		t.Fatal("second task was not released")
	}
}

func TestQueuedAttemptWaitsForActivePermitAfterPacingInterval(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}
	if err := first.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	secondResult := make(chan policy.AdmissionResult, 1)
	go func() {
		secondResult <- governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
	}()
	waitForQueue(t, governor, 1)
	select {
	case result := <-secondResult:
		t.Fatalf("queued attempt admitted while first was in flight: %#v", result)
	default:
	}
	if result := first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess}); result.Err != nil {
		t.Fatalf("first Finish() = %#v", result)
	}
	select {
	case result := <-secondResult:
		if !result.Admitted() {
			t.Fatalf("second admission = %#v", result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	case <-time.After(time.Second):
		t.Fatal("queued attempt was not released after active permit finished")
	}
}

func TestStartToStartPacingCountsResponseLatencyAndFastResponses(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "first"})
	first.Permit.Start()
	clock.Advance(6 * time.Second)
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "second"})
	if !second.Admitted() {
		t.Fatalf("slow response caused an unnecessary delay: %#v", second)
	}
	second.Permit.Start()
	second.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})

	third := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-c", ClientRequestID: "third"})
	if third.Admitted() {
		t.Fatal("fast response created an immediate burst")
	}
	if third.Code != policy.AdmissionDelayed {
		t.Fatalf("fast response admission = %#v, want delayed", third)
	}
	clock.Advance(4 * time.Second)
	if result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-d", ClientRequestID: "fourth"}); result.Admitted() {
		t.Fatal("request admitted before five-second start interval")
	}
	clock.Advance(time.Second)
	result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-e", ClientRequestID: "fifth"})
	if !result.Admitted() {
		t.Fatalf("request after pacing interval = %#v", result)
	}
	result.Permit.Start()
	result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestRollingWindowsAreMovingAndManualReserveIsSeparate(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	config.Rolling10m = 2
	config.Rolling1h = 3
	config.Rolling3h = 5
	config.ManualReserve = 1
	config.MinimumStartInterval = time.Nanosecond
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		admitAndFinish(t, governor, "task", "request-"+string(rune('a'+i)), false, policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Minute)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "blocked"})
	if blocked.Code != policy.AdmissionDelayed {
		t.Fatalf("rolling 10m result = %#v, want delayed", blocked)
	}
	if got := governor.Snapshot().Budgets.Rolling10mUsed; got != 2 {
		t.Fatalf("rolling 10m used = %d, want 2", got)
	}
	clock.Advance(8 * time.Minute)
	if got := governor.Snapshot().Budgets.Rolling10mUsed; got != 1 {
		t.Fatalf("rolling 10m used after moving expiry = %d, want 1", got)
	}
	clock.Advance(51 * time.Minute)
	if got := governor.Snapshot().Budgets.Rolling1hUsed; got != 0 {
		t.Fatalf("rolling 1h used after moving expiry = %d, want 0", got)
	}
	if got := governor.Snapshot().Budgets.ManualReserveRemaining; got != 1 {
		t.Fatalf("manual reserve remaining = %d, want 1", got)
	}
}

func TestInstantProfileAllowsFullAutomatedThreeHourCeiling(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.TaskBudget = 200
	config.RetryBudget = 200
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}

	batches := []struct {
		advance time.Duration
		count   int
	}{
		{advance: 0, count: 20},
		{advance: 10 * time.Minute, count: 20},
		{advance: 10 * time.Minute, count: 20},
		{advance: 10 * time.Minute, count: 20},
		{advance: 30 * time.Minute, count: 20},
		{advance: 10 * time.Minute, count: 20},
		{advance: 10 * time.Minute, count: 20},
	}
	requestNumber := 0
	for _, batch := range batches {
		clock.Advance(batch.advance)
		for i := 0; i < batch.count; i++ {
			requestNumber++
			tryAdmitAndFinish(t, governor, "task", "request-"+string(rune(0x1000+requestNumber)), false, policy.Outcome{Class: policy.OutcomeSuccess})
			clock.Advance(time.Nanosecond)
		}
	}

	snapshot := governor.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 140 {
		t.Fatalf("rolling 3h usage = %d, want full automated ceiling 140", snapshot.Budgets.Rolling3hUsed)
	}
	if snapshot.Budgets.Automated3hCeiling != 140 || snapshot.Budgets.ManualReserve != 20 {
		t.Fatalf("budget snapshot = %#v, want separate 140 ceiling and 20 reserve", snapshot.Budgets)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "after-ceiling"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionRollingBudgetExhausted {
		t.Fatalf("post-ceiling admission = %#v, want rolling budget exhaustion", blocked)
	}
}

func TestTaskAndRetryBudgetsChargeEveryStartedAttempt(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	config.TaskBudget = 3
	config.RetryBudget = 1
	config.Rolling3h = 10
	config.Rolling1h = 9
	config.Rolling10m = 8
	config.ManualReserve = 2
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	admitAndFinish(t, governor, "task", "first", false, policy.Outcome{Class: policy.OutcomeConnectionReset})
	clock.Advance(time.Second)
	finish := admitAndFinish(t, governor, "task", "retry", true, policy.Outcome{Class: policy.OutcomeUncertainReached, UpstreamReached: true})
	if finish.RetryEligible {
		t.Fatal("uncertain outcome remained retry eligible")
	}
	clock.Advance(time.Second)
	third := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "third", Retry: true})
	if third.Code != policy.AdmissionRetryBudgetExhausted {
		t.Fatalf("third retry admission = %#v, want retry budget exhausted", third)
	}
	clock.Advance(time.Second)
	regular := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "third-regular"})
	if !regular.Admitted() {
		t.Fatalf("regular task attempt = %#v, want admitted within task budget", regular)
	}
	regular.Permit.Start()
	regular.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(time.Second)
	tooMuch := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "fourth"})
	if tooMuch.Code != policy.AdmissionTaskBudgetExhausted {
		t.Fatalf("task budget result = %#v, want exhausted", tooMuch)
	}
	if got := governor.Snapshot().Budgets.Rolling3hUsed; got != 3 {
		t.Fatalf("rolling budget usage = %d, want 3", got)
	}
}

func TestTelemetryOnlyTightensLocalLimitsAndFailureKeepsThem(t *testing.T) {
	clock := newFakeClock()
	telemetry := &fakeTelemetry{}
	remaining := 11
	reset := clock.Now().Add(time.Hour)
	telemetry.snapshot = policy.TelemetrySnapshot{Remaining: &remaining, ResetAt: reset}
	config := fastConfig(t)
	config.Rolling3h = 20
	config.Rolling1h = 9
	config.Rolling10m = 8
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "first"})
	if !first.Admitted() {
		t.Fatalf("telemetry remaining admitted = %#v", first)
	}
	first.Permit.Start()
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(time.Second)
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "second"})
	if second.Code != policy.AdmissionDelayed || !second.RetryAt.Equal(reset) {
		t.Fatalf("telemetry exhausted result = %#v, want delayed until reset", second)
	}
	telemetry.err = errors.New("telemetry unavailable")
	telemetry.snapshot = policy.TelemetrySnapshot{}
	clock.Advance(time.Hour)
	third := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "third"})
	if !third.Admitted() {
		t.Fatalf("telemetry failure disabled local policy: %#v", third)
	}
	third.Permit.Start()
	third.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	if telemetry.calls < 3 {
		t.Fatalf("telemetry calls = %d, want repeated snapshots", telemetry.calls)
	}
}

func TestTelemetryCannotRaiseLocalHardCeiling(t *testing.T) {
	clock := newFakeClock()
	remaining := 100
	config := fastConfig(t)
	config.Rolling3h = 4
	config.Rolling1h = 3
	config.Rolling10m = 2
	config.ManualReserve = 1
	telemetry := &fakeTelemetry{snapshot: policy.TelemetrySnapshot{Remaining: &remaining, ResetAt: clock.Now().Add(time.Hour)}}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		admission := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-" + string(rune('a'+i))})
		if !admission.Admitted() {
			t.Fatalf("admission %d = %#v", i+1, admission)
		}
		admission.Permit.Start()
		admission.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Second)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "blocked"})
	if blocked.Code != policy.AdmissionDelayed {
		t.Fatalf("telemetry expanded local ceiling: %#v", blocked)
	}
}

func TestTelemetryRemainingOnlyTightensWithoutReset(t *testing.T) {
	clock := newFakeClock()
	remaining := 100
	telemetry := &fakeTelemetry{snapshot: policy.TelemetrySnapshot{Remaining: &remaining}}
	config := fastConfig(t)
	config.Rolling3h = 20
	config.Rolling1h = 19
	config.Rolling10m = 18
	config.ManualReserve = 1
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}

	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "first"})
	if !first.Admitted() {
		t.Fatalf("first telemetry admission = %#v", first)
	}
	if err := first.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(time.Nanosecond)

	zero := 0
	telemetry.Set(policy.TelemetrySnapshot{Remaining: &zero})
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "second"})
	if second.Code != policy.AdmissionUpstreamAllowanceExhausted {
		t.Fatalf("reduced telemetry without reset = %#v, want immediate exhaustion", second)
	}
}

func TestSecondTelemetryRateResponseBeforeResetOpensCircuit(t *testing.T) {
	clock := newFakeClock()
	reset := clock.Now().Add(time.Minute)
	telemetry := &fakeTelemetry{snapshot: policy.TelemetrySnapshot{RateLimited: true, ResetAt: reset}}
	config := fastConfig(t)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "first"})
	if first.Code != policy.AdmissionDelayed || first.Reason != policy.AdmissionUpstreamAllowanceExhausted {
		t.Fatalf("first telemetry rate response = %#v", first)
	}
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "second"})
	if second.Code != policy.AdmissionDelayed || second.Reason != policy.AdmissionCircuitOpen {
		t.Fatalf("second telemetry rate response = %#v", second)
	}
	snapshot := governor.Snapshot()
	if snapshot.Circuit.State != policy.CircuitOpenUntil || !snapshot.Circuit.OpenUntil.Equal(reset.Add(5*time.Minute)) {
		t.Fatalf("circuit snapshot = %#v", snapshot.Circuit)
	}
}

func TestBackoffUsesAuthoritativeHintsAndDeterministicJitter(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	config.MinimumStartInterval = time.Nanosecond
	config.Rolling3h = 20
	config.Rolling1h = 19
	config.Rolling10m = 18
	reset := clock.Now().Add(2 * time.Minute)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{values: []time.Duration{17 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	first := admitAndFinish(t, governor, "task", "first", false, policy.Outcome{Class: policy.OutcomeRateCapacity})
	if first.SelectedBackoff != 17*time.Second {
		t.Fatalf("first backoff = %s, want deterministic jitter", first.SelectedBackoff)
	}
	clock.Advance(time.Second)
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "second"})
	if second.Code != policy.AdmissionDelayed {
		t.Fatalf("second rate admission = %#v, want delayed", second)
	}
	clock.Advance(15 * time.Second)
	if result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "second"}); result.Admitted() {
		t.Fatal("rate cooldown was ignored")
	}
	clock.Advance(time.Second)
	ready := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "third"})
	if !ready.Admitted() {
		t.Fatalf("rate cooldown result = %#v", ready)
	}
	if err := ready.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	authoritative := ready.Permit.Finish(policy.Outcome{Class: policy.OutcomeRateCapacity, RetryAfter: time.Minute, ResetAt: reset})
	if authoritative.SelectedBackoff != reset.Sub(clock.Now()) {
		t.Fatalf("authoritative backoff = %s, want %s", authoritative.SelectedBackoff, reset.Sub(clock.Now()))
	}
	if !governor.Snapshot().CooldownUntil.Equal(reset) {
		t.Fatalf("cooldown = %s, want reset %s", governor.Snapshot().CooldownUntil, reset)
	}
}

func TestRetryAfterWithoutResetIsAuthoritative(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	config := governor.Config()
	governor.Close()
	var err error
	governor, err = policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{values: []time.Duration{time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	finish := admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: policy.OutcomeRateCapacity, RetryAfter: 40 * time.Second})
	if finish.SelectedBackoff != 40*time.Second || !governor.Snapshot().CooldownUntil.Equal(clock.Now().Add(40*time.Second)) {
		t.Fatalf("Retry-After result = %#v, cooldown=%s", finish, governor.Snapshot().CooldownUntil)
	}
}

func TestSecuritySignalsOpenCircuitWithoutRetry(t *testing.T) {
	cases := []policy.OutcomeClass{
		policy.OutcomeAuthenticationDenied,
		policy.OutcomeHTTP403,
		policy.OutcomeLoginChallenge,
		policy.OutcomeCAPTCHA,
		policy.OutcomeSuspiciousActivity,
		policy.OutcomeAccountWarning,
		policy.OutcomeFeatureRestriction,
	}
	for _, outcome := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			governor, _, _ := instantGovernor(t)
			finished := admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: outcome})
			if finished.RetryEligible {
				t.Fatal("security signal was marked retry eligible")
			}
			if governor.Snapshot().Circuit.State != policy.CircuitHumanReviewRequired {
				t.Fatalf("circuit = %q, want human review", governor.Snapshot().Circuit.State)
			}
			blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "retry", Retry: true})
			if blocked.Code != policy.AdmissionHumanAcknowledgementRequired {
				t.Fatalf("blocked admission = %#v", blocked)
			}
		})
	}
}

func TestExpiredCredentialRequiresRefreshAndDoesNotRetryModel(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	finished := admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: policy.OutcomeAuthenticationExpired})
	if finished.RetryEligible {
		t.Fatal("expired credential allowed a model retry")
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "model-retry", Retry: true})
	if blocked.Code != policy.AdmissionAuthenticationRefreshRequired {
		t.Fatalf("model retry after expired credential = %#v", blocked)
	}
	refresh := governor.BeginCredentialRefresh()
	if !refresh.Admitted() {
		t.Fatalf("credential refresh = %#v", refresh)
	}
	if err := refresh.Permit.Finish(true); err != nil {
		t.Fatal(err)
	}
	if governor.Snapshot().Circuit.State != policy.CircuitClosed {
		t.Fatalf("circuit after successful refresh = %q", governor.Snapshot().Circuit.State)
	}
}

func TestThreeRateResponsesRequireHumanAcknowledgement(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	for i := 0; i < 3; i++ {
		finish := admitAndFinish(t, governor, "task", "request-"+string(rune('a'+i)), false, policy.Outcome{Class: policy.OutcomeRateCapacity})
		if i < 2 && finish.Circuit.State == policy.CircuitHumanReviewRequired {
			t.Fatalf("circuit opened too early at response %d", i+1)
		}
		clock.Advance(time.Minute)
	}
	if got := governor.Snapshot().Circuit.State; got != policy.CircuitHumanReviewRequired {
		t.Fatalf("circuit = %q, want human review", got)
	}
	if err := governor.AcknowledgeHuman(); err != nil {
		t.Fatalf("AcknowledgeHuman() error = %v", err)
	}
	if governor.Snapshot().Circuit.State != policy.CircuitClosed {
		t.Fatal("human acknowledgement did not close circuit")
	}
}

func TestEventSinkCanReenterSnapshot(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	sink := &snapshotSink{}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: sink})
	if err != nil {
		t.Fatal(err)
	}
	sink.governor = governor

	done := make(chan error, 1)
	go func() {
		admission := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"})
		if !admission.Admitted() {
			done <- errors.New("request was not admitted")
			return
		}
		if err := admission.Permit.Start(); err != nil {
			done <- err
			return
		}
		admission.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("event sink re-entry deadlocked the governor")
	}
	drainDone := make(chan int, 1)
	go func() { drainDone <- governor.DrainEvents() }()
	select {
	case drained := <-drainDone:
		if drained == 0 {
			t.Fatal("event sink drain delivered no events")
		}
	case <-time.After(time.Second):
		t.Fatal("event sink re-entry deadlocked the event drain")
	}
}

func TestBlockedEventSinkDoesNotHoldGovernorMutex(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: sink})
	if err != nil {
		t.Fatal(err)
	}

	admission := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"})
	if !admission.Admitted() {
		t.Fatalf("admission = %#v", admission)
	}
	drainDone := make(chan int, 1)
	go func() { drainDone <- governor.DrainEvents() }()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		close(sink.release)
		t.Fatal("event sink was not called by explicit drain")
	}
	snapshotDone := make(chan struct{})
	go func() {
		_ = governor.Snapshot()
		close(snapshotDone)
	}()
	select {
	case <-snapshotDone:
	case <-time.After(time.Second):
		close(sink.release)
		t.Fatal("blocked event sink held the governor mutex")
	}
	close(sink.release)
	select {
	case drained := <-drainDone:
		if drained == 0 {
			t.Fatal("event drain delivered no events")
		}
	case <-time.After(time.Second):
		t.Fatal("event drain did not complete after event sink release")
	}
}

func TestEventDrainCatchesEventsQueuedDuringDelivery(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: sink})
	if err != nil {
		t.Fatal(err)
	}
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "first"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}

	drainDone := make(chan int, 1)
	go func() { drainDone <- governor.DrainEvents() }()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		close(sink.release)
		t.Fatal("event drain did not enter sink")
	}
	delayed := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "delayed"})
	if delayed.Code != policy.AdmissionDelayed {
		t.Fatalf("concurrent admission = %#v, want delayed", delayed)
	}
	close(sink.release)
	select {
	case drained := <-drainDone:
		if drained != 2 {
			t.Fatalf("drained events = %d, want 2", drained)
		}
	case <-time.After(time.Second):
		t.Fatal("event drain did not finish")
	}
	if snapshot := governor.Snapshot(); snapshot.PendingEvents != 0 {
		t.Fatalf("events queued during delivery remained pending: %#v", snapshot)
	}
	if err := first.Permit.CancelBeforeStart(); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedEventSinkDoesNotBlockExecutionOrLane(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: sink})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.ProviderResponse{Content: "response", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}})
	executionDone := make(chan policy.ExecutionResult, 1)
	go func() {
		executionDone <- governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, client, nil)
	}()

	var execution policy.ExecutionResult
	select {
	case execution = <-executionDone:
	case <-time.After(time.Second):
		close(sink.release)
		t.Fatal("blocked event sink delayed execution")
	}
	if !execution.Admission.Admitted() || client.Attempts() != 1 {
		t.Fatalf("execution = %#v, provider attempts = %d", execution, client.Attempts())
	}

	drainDone := make(chan int, 1)
	go func() { drainDone <- governor.DrainEvents() }()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("event drain did not call the sink")
	}
	clock.Advance(5 * time.Second)
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "second", ClientRequestID: "second"})
	if !second.Admitted() {
		t.Fatalf("lane remained blocked by event sink: %#v", second)
	}
	if err := second.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	second.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	close(sink.release)
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("event drain did not finish after sink release")
	}
}

func TestUpstreamOpenCircuitRemainsBlockedAfterCooldown(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	telemetry := &fakeTelemetry{snapshot: policy.TelemetrySnapshot{
		UpstreamCircuit: policy.UpstreamCircuitOpen,
		CooldownUntil:   clock.Now().Add(-time.Second),
	}}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.ProviderResponse{Content: "response", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}})
	blocked := governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, client, nil)
	if blocked.Admission.Code != policy.AdmissionCircuitOpen {
		t.Fatalf("expired upstream cooldown admission = %#v, want circuit open", blocked.Admission)
	}
	if client.Attempts() != 0 || governor.Snapshot().InFlight {
		t.Fatalf("expired upstream cooldown side effects = attempts %d, snapshot %#v", client.Attempts(), governor.Snapshot())
	}

	telemetry.Set(policy.TelemetrySnapshot{UpstreamCircuit: policy.UpstreamCircuitClosed})
	allowed := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "closed-request"})
	if !allowed.Admitted() {
		t.Fatalf("explicit upstream close admission = %#v, want admitted", allowed)
	}
	allowed.Permit.Start()
	allowed.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestAttemptEventsPreserveTelemetryHealth(t *testing.T) {
	clock := newFakeClock()
	events := &eventSink{}
	telemetry := &fakeTelemetry{err: errors.New("telemetry unavailable")}
	config := fastConfig(t)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.ProviderResponse{Content: "response", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}})
	result := governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, client, nil)
	if !result.Admission.Admitted() || result.Err != nil {
		t.Fatalf("execution with telemetry failure = %#v", result)
	}
	governor.DrainEvents()
	for _, event := range events.Events() {
		if event.Kind == policy.EventAttemptStarted || event.Kind == policy.EventAttemptFinished {
			if event.TelemetryHealthy {
				t.Fatalf("attempt event reported healthy telemetry: %#v", event)
			}
		}
	}
}

func TestMandatoryEventsSurviveLargeBacklog(t *testing.T) {
	governor, _, events := instantGovernor(t)
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "first"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}
	if err := first.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		governor.TryAdmit(context.Background(), policy.AttemptRequest{
			TaskID:          "waiting",
			ClientRequestID: "waiting-" + strconv.Itoa(i),
		})
	}
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeHTTP403})
	snapshot := governor.Snapshot()
	const expectedEvents = 304
	if snapshot.PendingEvents != expectedEvents {
		t.Fatalf("event queue snapshot = %#v, want %d pending events", snapshot, expectedEvents)
	}
	if drained := governor.DrainEvents(); drained != expectedEvents {
		t.Fatalf("drained events = %d, want %d", drained, expectedEvents)
	}
	seenStart := false
	seenFinish := false
	seenSecurityCircuit := false
	for _, event := range events.Events() {
		switch event.Kind {
		case policy.EventAttemptStarted:
			seenStart = seenStart || event.ClientRequestID == "first"
		case policy.EventAttemptFinished:
			seenFinish = seenFinish || event.ClientRequestID == "first"
		case policy.EventCircuit:
			seenSecurityCircuit = seenSecurityCircuit || event.CircuitTo == policy.CircuitHumanReviewRequired
		}
	}
	if !seenStart || !seenFinish || !seenSecurityCircuit {
		t.Fatalf("mandatory events missing: start=%t finish=%t security_circuit=%t", seenStart, seenFinish, seenSecurityCircuit)
	}
}

func TestCircuitTransitionsEmitOneEventEach(t *testing.T) {
	cases := []struct {
		name        string
		outcome     policy.OutcomeClass
		threshold   int
		acknowledge bool
		refresh     bool
		want        int
	}{
		{name: "security", outcome: policy.OutcomeHTTP403, want: 1},
		{name: "rate threshold", outcome: policy.OutcomeRateCapacity, threshold: 1, want: 1},
		{name: "acknowledgement", outcome: policy.OutcomeHTTP403, acknowledge: true, want: 2},
		{name: "credential refresh", outcome: policy.OutcomeAuthenticationExpired, refresh: true, want: 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			events := &eventSink{}
			config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
			if testCase.threshold != 0 {
				config.RateResponseThreshold = testCase.threshold
			}
			governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
			if err != nil {
				t.Fatal(err)
			}
			admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: testCase.outcome})
			if testCase.acknowledge {
				if err := governor.AcknowledgeHuman(); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.refresh {
				refresh := governor.BeginCredentialRefresh()
				if !refresh.Admitted() {
					t.Fatalf("credential refresh = %#v", refresh)
				}
				if err := refresh.Permit.Finish(true); err != nil {
					t.Fatal(err)
				}
			}
			governor.DrainEvents()
			count := 0
			for _, event := range events.Events() {
				if event.Kind == policy.EventCircuit {
					count++
				}
			}
			if count != testCase.want {
				t.Fatalf("circuit events = %d, want %d: %#v", count, testCase.want, events.Events())
			}
		})
	}
}

func TestQueueCancellationAndFairnessQuantum(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	config := governor.Config()
	config.FairnessQuantum = 1
	governor.Close()
	var err error
	governor, err = policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "a", ClientRequestID: "a-1"})
	first.Permit.Start()
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(5 * time.Second)
	second := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "a", ClientRequestID: "a-2"})
	if !second.Admitted() {
		t.Fatalf("second a turn = %#v", second)
	}
	second.Permit.Start()
	thirdResult := make(chan policy.AdmissionResult, 1)
	go func() {
		thirdResult <- governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "b", ClientRequestID: "b-1"})
	}()
	canceledCtx, cancel := context.WithCancel(context.Background())
	canceledResult := make(chan policy.AdmissionResult, 1)
	go func() {
		canceledResult <- governor.Admit(canceledCtx, policy.AttemptRequest{TaskID: "canceled", ClientRequestID: "canceled-1"})
	}()
	waitForQueue(t, governor, 2)
	cancel()
	select {
	case result := <-canceledResult:
		if result.Code != policy.AdmissionContextCancelled {
			t.Fatalf("canceled queue result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled queue request did not leave promptly")
	}
	second.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(5 * time.Second)
	select {
	case result := <-thirdResult:
		if !result.Admitted() {
			t.Fatalf("fairness result = %#v", result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	case <-time.After(time.Second):
		t.Fatal("waiting task did not receive fairness turn")
	}
}

func TestQueueCapacityIsApplied(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "active", ClientRequestID: "active"})
	first.Permit.Start()
	deferred := make([]chan policy.AdmissionResult, 0, 16)
	cancels := make([]context.CancelFunc, 0, 16)
	for i := 0; i < 16; i++ {
		result := make(chan policy.AdmissionResult, 1)
		ctx, cancel := context.WithCancel(context.Background())
		deferred = append(deferred, result)
		cancels = append(cancels, cancel)
		go func(i int) {
			result <- governor.Admit(ctx, policy.AttemptRequest{TaskID: "task", ClientRequestID: "queued-" + string(rune(i))})
		}(i)
	}
	waitForQueue(t, governor, 16)
	full := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "overflow", ClientRequestID: "overflow"})
	if full.Code != policy.AdmissionQueueFull {
		t.Fatalf("overflow admission = %#v, want queue full", full)
	}
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	for _, cancel := range cancels {
		cancel()
	}
	for _, result := range deferred {
		select {
		case admission := <-result:
			if admission.Permit != nil {
				admission.Permit.CancelBeforeStart()
			}
		case <-time.After(time.Second):
			t.Fatal("queued request did not cancel")
		}
	}
}

func TestUnsafeProviderAmplificationFailsBeforeProviderCall(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	unsafe := &safetyClient{safety: provider.RouteSafety{}}
	result := governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, unsafe, nil)
	if result.Admission.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("unsafe execute admission = %#v", result.Admission)
	}
	if unsafe.calls != 0 {
		t.Fatalf("unsafe client calls = %d, want 0", unsafe.calls)
	}
}

type safetyClient struct {
	safety provider.RouteSafety
	calls  int
}

func (c *safetyClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	c.calls++
	return provider.Response{Text: "secret response", Metadata: provider.ResponseMetadata{StatusCode: 200}}, nil
}

func (c *safetyClient) RouteSafety() provider.RouteSafety { return c.safety }

type plainClient struct {
	calls int
}

func (c *plainClient) Complete(context.Context, provider.Request) (provider.Response, error) {
	c.calls++
	return provider.Response{Text: "private model response", Metadata: provider.ResponseMetadata{StatusCode: 200}}, nil
}

func TestExecuteRejectsProviderWithoutExplicitRouteSafety(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	client := &plainClient{}
	result := governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, client, nil)
	if result.Admission.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("unaware provider admission = %#v, want fail-closed unsafe provider", result.Admission)
	}
	if client.calls != 0 || governor.Snapshot().Budgets.Rolling3hUsed != 0 {
		t.Fatalf("unaware provider side effects = calls %d, budget %d; want zero", client.calls, governor.Snapshot().Budgets.Rolling3hUsed)
	}
}

func TestSingleAttemptExecutionChargesExactlyOnceAndSanitizesEvents(t *testing.T) {
	governor, _, events := instantGovernor(t)
	fake := provider.NewFake(provider.ProviderResponse{Content: "private model response", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}})
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, fake, nil)
	if !result.Admission.Admitted() || result.Err != nil {
		t.Fatalf("Execute() = %#v", result)
	}
	if fake.Attempts() != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("attempt accounting = provider %d, snapshot %#v", fake.Attempts(), governor.Snapshot().Budgets)
	}
	governor.DrainEvents()
	encoded, err := json.Marshal(events.Events())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private prompt", "private model response", "cookie", "token", "credential", "api key"} {
		if strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(secret)) {
			t.Fatalf("sanitized events contain %q: %s", secret, encoded)
		}
	}
}

func TestDuplicateClientRequestIDIsRejectedAfterFirstAttempt(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	client := provider.NewFake(provider.ProviderResponse{Content: "private model response", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}})
	request := policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}
	first := governor.Execute(context.Background(), request, client, nil)
	if !first.Admission.Admitted() || first.Err != nil {
		t.Fatalf("first Execute() = %#v", first)
	}
	clock.Advance(time.Nanosecond)
	second := governor.Execute(context.Background(), request, client, nil)
	if second.Admission.Code != policy.AdmissionDuplicateClientRequest {
		t.Fatalf("duplicate Execute() = %#v, want duplicate request code", second.Admission)
	}
	if client.Attempts() != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("duplicate side effects = provider %d, budget %d; want one", client.Attempts(), governor.Snapshot().Budgets.Rolling3hUsed)
	}
}

func TestJitterCannotShortenRateBackoffBaseline(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	config.RateResponseThreshold = 10
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{values: []time.Duration{0}}})
	if err != nil {
		t.Fatal(err)
	}

	wants := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, want := range wants {
		finish := admitAndFinish(t, governor, "task", "rate-"+string(rune('a'+i)), false, policy.Outcome{Class: policy.OutcomeRateCapacity})
		if finish.SelectedBackoff != want {
			t.Fatalf("rate response %d backoff = %s, want baseline %s", i+1, finish.SelectedBackoff, want)
		}
		clock.Advance(want)
	}
}

func TestUncertainOutcomeRemainsDebited(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	finish := admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: policy.OutcomeUncertainReached, UpstreamReached: true})
	if finish.AttemptDebited != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("uncertain accounting = %#v", finish)
	}
}

func TestCancellationAfterStartIsUncertainAndStillDebited(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	fake := provider.NewErrorFake(context.Canceled)
	result := governor.Execute(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"}, fake, nil)
	if fake.Attempts() != 1 {
		t.Fatalf("provider attempts = %d, want 1", fake.Attempts())
	}
	if result.Completion.Outcome != policy.OutcomeUncertainReached || result.Completion.AttemptDebited != 1 {
		t.Fatalf("cancellation completion = %#v", result.Completion)
	}
	if governor.Snapshot().Budgets.Rolling3hUsed != 1 || governor.Snapshot().InFlight {
		t.Fatalf("post-cancellation snapshot = %#v", governor.Snapshot())
	}
}

func TestRetentionExpiresCompletedRequestIDs(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	admitAndFinish(t, governor, "first-task", "request", false, policy.Outcome{Class: policy.OutcomeSuccess})
	if got := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "same-task", ClientRequestID: "request"}); got.Code != policy.AdmissionDuplicateClientRequest {
		t.Fatalf("recent request reuse = %#v, want duplicate", got)
	}
	clock.Advance(3*time.Hour + time.Nanosecond)
	admission := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "second-task", ClientRequestID: "request"})
	if !admission.Admitted() {
		t.Fatalf("request reuse after retention window = %#v, want admitted", admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	admission.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestRetentionPreservesPendingAndActiveRequests(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	active := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "active", ClientRequestID: "active-request"})
	if !active.Admitted() {
		t.Fatalf("active admission = %#v", active)
	}
	if err := active.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	queuedContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := make(chan policy.AdmissionResult, 1)
	go func() {
		queued <- governor.Admit(queuedContext, policy.AttemptRequest{TaskID: "queued", ClientRequestID: "queued-request"})
	}()
	waitForQueue(t, governor, 1)
	clock.Advance(4 * time.Hour)
	snapshot := governor.Snapshot()
	if snapshot.RetainedTaskStates == 0 {
		t.Fatalf("active task state was pruned: %#v", snapshot)
	}
	for _, request := range []policy.AttemptRequest{
		{TaskID: "active-again", ClientRequestID: "active-request"},
		{TaskID: "queued-again", ClientRequestID: "queued-request"},
	} {
		if got := governor.TryAdmit(context.Background(), request); got.Code != policy.AdmissionDuplicateClientRequest {
			t.Fatalf("request %q after retention pruning = %#v, want duplicate", request.ClientRequestID, got)
		}
	}
	cancel()
	if result := <-queued; result.Code != policy.AdmissionContextCancelled {
		t.Fatalf("queued cleanup = %#v", result)
	}
	if result := active.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess}); result.Err != nil {
		t.Fatalf("active cleanup = %#v", result)
	}
}

func TestRetentionCapsCompletedRequestsAndTasks(t *testing.T) {
	clock := newFakeClock()
	config := fastConfig(t)
	config.Rolling3h = 20000
	config.Rolling1h = 10000
	config.Rolling10m = 5000
	config.TaskBudget = 2
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4097; index++ {
		id := strconv.Itoa(index)
		admitAndFinish(t, governor, "task-"+id, "request-"+id, false, policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	snapshot := governor.Snapshot()
	if snapshot.RetainedRequestIDs > 4096 {
		t.Fatalf("retained request IDs = %d, want at most 4096", snapshot.RetainedRequestIDs)
	}
	if snapshot.RetainedTaskStates > 1024 {
		t.Fatalf("retained task states = %d, want at most 1024", snapshot.RetainedTaskStates)
	}
}

func TestRetentionExpiresInactiveTaskStates(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	admitAndFinish(t, governor, "task", "request", false, policy.Outcome{Class: policy.OutcomeSuccess})
	if got := governor.Snapshot().RetainedTaskStates; got == 0 {
		t.Fatal("completed task state was not retained immediately")
	}
	clock.Advance(3*time.Hour + time.Nanosecond)
	if got := governor.Snapshot().RetainedTaskStates; got != 0 {
		t.Fatalf("expired task states = %d, want zero", got)
	}
}

func TestCancelBeforeStartAfterStartDoesNotReleaseLane(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}
	if err := first.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := first.Permit.CancelBeforeStart(); !errors.Is(err, policy.ErrPermitStarted) {
		t.Fatalf("CancelBeforeStart after Start() = %v, want ErrPermitStarted", err)
	}
	snapshot := governor.Snapshot()
	if !snapshot.InFlight || snapshot.Budgets.Rolling3hUsed != 1 {
		t.Fatalf("started permit changed after rejected cancel: %#v", snapshot)
	}
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "other", ClientRequestID: "other"})
	if second.Code != policy.AdmissionDelayed {
		t.Fatalf("admission after rejected cancel = %#v, want delayed", second)
	}
	if result := first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess}); result.Err != nil {
		t.Fatalf("first Finish() = %#v", result)
	}
	clock.Advance(5 * time.Second)
	if got := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "other", ClientRequestID: "other"}); !got.Admitted() {
		t.Fatalf("admission after real finish = %#v", got)
	} else {
		got.Permit.Start()
		got.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	}
}

func TestNoGoroutineLeakAfterQueuedCancellation(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "active", ClientRequestID: "active"})
	first.Permit.Start()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan policy.AdmissionResult, 1)
	go func() {
		done <- governor.Admit(ctx, policy.AttemptRequest{TaskID: "queued", ClientRequestID: "queued"})
	}()
	waitForQueue(t, governor, 1)
	cancel()
	result := <-done
	if result.Code != policy.AdmissionContextCancelled {
		t.Fatalf("queued cancellation = %#v", result)
	}
	if result := first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess}); result.Err != nil {
		t.Fatalf("active permit cleanup = %#v", result)
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if runtime.NumGoroutine() > before+1 {
		t.Fatalf("goroutines remained after cancellation: before=%d after=%d", before, runtime.NumGoroutine())
	}
}

func TestTryAdmitDistinguishesTaskDeadline(t *testing.T) {
	governor, clock, _ := instantGovernor(t)
	ctx, cancel := context.WithDeadline(context.Background(), clock.Now().Add(-time.Second))
	defer cancel()
	result := governor.TryAdmit(ctx, policy.AttemptRequest{TaskID: "task", ClientRequestID: "deadline"})
	if result.Code != policy.AdmissionTaskDeadlineExceeded {
		t.Fatalf("deadline result = %#v", result)
	}
}

func TestPermitCannotBeCompletedTwice(t *testing.T) {
	governor, _, _ := instantGovernor(t)
	admission := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request"})
	if !admission.Admitted() {
		t.Fatal(admission)
	}
	if err := admission.Permit.Start(); err != nil {
		t.Fatal(err)
	}
	admission.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	if got := admission.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess}); !errors.Is(got.Err, policy.ErrPermitCompleted) {
		t.Fatalf("second Finish() = %#v, want permit completed", got)
	}
}
