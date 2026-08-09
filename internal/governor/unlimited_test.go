package governor_test

import (
	"context"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Issue #58: explicit unlimited-text allowance semantics. "Unlimited text"
// means no known numeric upstream text-message quota; it never means
// ungoverned Runstead execution. These tests use the fake clock and fixed
// jitter from governor_test.go and are fully deterministic.

func unlimitedGovernor(t *testing.T, configure func(*policy.Config)) (*policy.Governor, *fakeClock, *eventSink) {
	t.Helper()
	clock := newFakeClock()
	events := &eventSink{}
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	if configure != nil {
		configure(&config)
	}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
	if err != nil {
		t.Fatalf("unlimited governor: %v", err)
	}
	return governor, clock, events
}

func unknownGovernor(t *testing.T, configure func(*policy.Config)) (*policy.Governor, *fakeClock, *eventSink) {
	t.Helper()
	clock := newFakeClock()
	events := &eventSink{}
	config := policy.DefaultUnknownConfig("policy-unknown", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	if configure != nil {
		configure(&config)
	}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
	if err != nil {
		t.Fatalf("unknown governor: %v", err)
	}
	return governor, clock, events
}

func TestUnlimitedTextProfileValidationIsExplicit(t *testing.T) {
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety())
	if err := config.Validate(); err != nil {
		t.Fatalf("luna default Validate() error = %v", err)
	}
	if config.AllowanceKind != policy.AllowanceKindUnlimitedText || config.AllowanceProfile != policy.ProfileLunaUnlimitedText {
		t.Fatalf("luna defaults = profile %q kind %q", config.AllowanceProfile, config.AllowanceKind)
	}
	if config.Rolling3h != 0 || config.Rolling1h != 0 || config.Rolling10m != 0 || config.ManualReserve != 0 {
		t.Fatalf("luna defaults fabricate numeric allowance state: %#v", config)
	}
	if config.MaxInFlight != 1 || config.TaskBudget != 80 || config.RetryBudget != 2 ||
		config.QueueCapacity != 16 || config.MinimumStartInterval != 5*time.Second {
		t.Fatalf("luna defaults dropped local workload controls: %#v", config)
	}

	for name, mutate := range map[string]func(*policy.Config){
		"fabricated 3h ceiling":  func(c *policy.Config) { c.Rolling3h = 140 },
		"fabricated 1h ceiling":  func(c *policy.Config) { c.Rolling1h = 80 },
		"fabricated 10m ceiling": func(c *policy.Config) { c.Rolling10m = 25 },
		"inherited reserve":      func(c *policy.Config) { c.ManualReserve = 20 },
		"parallel account":       func(c *policy.Config) { c.MaxInFlight = 2 },
		"no pacing":              func(c *policy.Config) { c.MinimumStartInterval = 0 },
		"kind mismatch":          func(c *policy.Config) { c.AllowanceKind = policy.AllowanceKindPublishedQuota },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := config
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("Validate() error = nil for %s, want fail-closed error", name)
			}
		})
	}

	unknown := policy.DefaultUnknownConfig("policy-unknown", "omniroute", "instant", provider.SafeRouteSafety())
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unknown default Validate() error = %v", err)
	}
	if unknown.AllowanceKind != policy.AllowanceKindUnknown {
		t.Fatalf("unknown defaults = %#v", unknown)
	}
	// The upstream allowance is unknown, so the conservative local layer
	// stays mandatory (#21 contract, #58 review): explicit positive local
	// ceilings and a local manual-use reserve are required and enforced.
	if unknown.Rolling3h <= 0 || unknown.Rolling1h <= 0 || unknown.Rolling10m <= 0 || unknown.ManualReserve <= 0 {
		t.Fatalf("unknown defaults dropped the conservative local layer: %#v", unknown)
	}
	unknown.Rolling3h = 0
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown profile accepted zero local rolling ceilings")
	}
	unknown.Rolling3h = 40
	unknown.Rolling1h = 20
	unknown.Rolling10m = 8
	unknown.ManualReserve = 40
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown profile accepted a manual reserve above the 3h ceiling")
	}
	// A zero reserve is legal exactly as in the #21 contract (0 <= reserve <
	// 3h); what is mandatory is the explicit conservative local layer.
	unknown.ManualReserve = 0
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unknown profile with zero reserve failed validation: %v", err)
	}
}

func TestUnlimitedTextAdmitsSequentialRequestsWithoutRollingQuota(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, nil)
	for i := 1; i <= 12; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		if !result.Admitted() {
			t.Fatalf("unlimited sequential admission %d = %#v, want admitted without a fabricated rolling quota", i, result)
		}
		if err := result.Permit.Start(); err != nil {
			t.Fatal(err)
		}
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	snapshot := governor.Snapshot()
	if snapshot.AllowanceKind != policy.AllowanceKindUnlimitedText {
		t.Fatalf("snapshot kind = %q, want unlimited_text", snapshot.AllowanceKind)
	}
	budgets := snapshot.Budgets
	if budgets.Rolling3hCeiling != 0 || budgets.Rolling1hCeiling != 0 || budgets.Rolling10mCeiling != 0 ||
		budgets.Automated3hCeiling != 0 || budgets.ManualReserve != 0 || budgets.ManualReserveRemaining != 0 {
		t.Fatalf("unlimited budgets fabricate ceilings/reserve: %#v", budgets)
	}
	if budgets.Rolling3hUsed != 12 || budgets.Rolling1hUsed != 12 || budgets.Rolling10mUsed != 12 {
		t.Fatalf("unlimited ledger usage = %#v, want all 12 attempts accounted", budgets)
	}
}

func TestUnlimitedTextTaskBudgetStillTerminatesRunawayTask(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, func(config *policy.Config) { config.TaskBudget = 3 })
	for i := 1; i <= 3; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		if !result.Admitted() {
			t.Fatalf("attempt %d = %#v, want admitted", i, result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-4"})
	if blocked.Code != policy.AdmissionTaskBudgetExhausted {
		t.Fatalf("runaway admission = %#v, want task_budget_exhausted", blocked)
	}
}

func TestUnlimitedTextMaxInFlightRemainsExactlyOne(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, nil)
	first := governor.Admit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
	if !first.Admitted() {
		t.Fatalf("first admission = %#v", first)
	}
	first.Permit.Start()
	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
	if second.Code != policy.AdmissionDelayed {
		t.Fatalf("second in-flight admission = %#v, want delayed", second)
	}
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	clock.Advance(time.Nanosecond)
	third := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-c"})
	if !third.Admitted() {
		t.Fatalf("post-flight admission = %#v, want admitted", third)
	}
	third.Permit.Start()
	third.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestUnlimitedTextPacingPreventsBurstsAndFastFailuresDoNotBypassIt(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, func(config *policy.Config) {
		config.MinimumStartInterval = 5 * time.Second
	})
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
	first.Permit.Start()
	clock.Advance(6 * time.Second)
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})

	second := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
	if !second.Admitted() {
		t.Fatalf("slow response caused an unnecessary delay: %#v", second)
	}
	second.Permit.Start()
	clock.Advance(2 * time.Second)
	// A fast non-rate failure (connection reset) must not bypass pacing and
	// must not arm a cooldown that would mask the pacing assertion.
	second.Permit.Finish(policy.Outcome{Class: policy.OutcomeConnectionReset, UpstreamReached: true})

	burst := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-c", ClientRequestID: "request-c"})
	if burst.Code != policy.AdmissionDelayed {
		t.Fatalf("fast failure admission = %#v, want pacing delay", burst)
	}
	clock.Advance(2 * time.Second)
	if result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-d", ClientRequestID: "request-d"}); result.Code != policy.AdmissionDelayed {
		t.Fatalf("admission before the start interval = %#v, want delayed", result)
	}
	clock.Advance(time.Second + time.Nanosecond)
	final := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-e", ClientRequestID: "request-e"})
	if !final.Admitted() {
		t.Fatalf("admission after the start interval = %#v, want admitted", final)
	}
	final.Permit.Start()
	final.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestUnlimitedTextRateCapacitySignalsStillRestrictAdmission(t *testing.T) {
	clock := newFakeClock()
	telemetry := &fakeTelemetry{}
	telemetry.Set(policy.TelemetrySnapshot{
		RateLimited:     true,
		ResetAt:         clock.Now().Add(10 * time.Minute),
		UpstreamCircuit: policy.UpstreamCircuitUnknown,
	})
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	rateGovernor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	blocked := rateGovernor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-1"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionUpstreamAllowanceExhausted || blocked.RetryAt.IsZero() {
		t.Fatalf("rate-limited admission = %#v, want delayed upstream_allowance_exhausted until reset", blocked)
	}
	// The rate signal remains authoritative even though the allowance is
	// unlimited text: it is an upstream restriction, not a quota counter.
	telemetry.Set(policy.TelemetrySnapshot{UpstreamCircuit: policy.UpstreamCircuitUnknown})
	clock.Advance(11 * time.Minute)
	if result := rateGovernor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-2"}); !result.Admitted() {
		t.Fatalf("admission after the rate reset = %#v, want admitted", result)
	}
}

func TestUnlimitedTextCapacityExhaustedStillRestrictsAdmission(t *testing.T) {
	clock := newFakeClock()
	telemetry := &fakeTelemetry{}
	telemetry.Set(policy.TelemetrySnapshot{
		CapacityExhausted: true,
		ResetAt:           clock.Now().Add(5 * time.Minute),
		UpstreamCircuit:   policy.UpstreamCircuitUnknown,
	})
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	capacityGovernor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	blocked := capacityGovernor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-1"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionUpstreamAllowanceExhausted || blocked.RetryAt.IsZero() {
		t.Fatalf("capacity-exhausted admission = %#v, want delayed upstream_allowance_exhausted", blocked)
	}
}

func TestUnlimitedTextRetryAfterAndCooldownRemainAuthoritative(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, nil)
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
	first.Permit.Start()
	completion := first.Permit.Finish(policy.Outcome{Class: policy.OutcomeRateCapacity, RetryAfter: 30 * time.Second, UpstreamReached: true})
	if completion.SelectedBackoff != 30*time.Second {
		t.Fatalf("selected backoff = %s, want authoritative 30s Retry-After", completion.SelectedBackoff)
	}
	if governor.Snapshot().CooldownUntil.IsZero() {
		t.Fatal("cooldown must be armed under unlimited text")
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionCooldownActive {
		t.Fatalf("cooldown admission = %#v, want delayed cooldown_active", blocked)
	}
	clock.Advance(31 * time.Second)
	admitted := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-c", ClientRequestID: "request-c"})
	if !admitted.Admitted() {
		t.Fatalf("admission after Retry-After = %#v, want admitted", admitted)
	}
	admitted.Permit.Start()
	admitted.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
}

func TestUnlimitedTextSevereSignalsStillOpenHumanReviewCircuit(t *testing.T) {
	for _, signal := range []policy.OutcomeClass{
		policy.OutcomeAuthenticationDenied,
		policy.OutcomeCAPTCHA,
		policy.OutcomeSuspiciousActivity,
		policy.OutcomeFeatureRestriction,
	} {
		t.Run(string(signal), func(t *testing.T) {
			governor, clock, _ := unlimitedGovernor(t, nil)
			first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-a", ClientRequestID: "request-a"})
			first.Permit.Start()
			completion := first.Permit.Finish(policy.Outcome{Class: signal, UpstreamReached: true})
			if completion.Circuit.State != policy.CircuitHumanReviewRequired {
				t.Fatalf("circuit after %s = %s, want human_review_required", signal, completion.Circuit.State)
			}
			blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-b", ClientRequestID: "request-b"})
			if blocked.Code != policy.AdmissionHumanAcknowledgementRequired {
				t.Fatalf("admission after %s = %#v, want human acknowledgement gate", signal, blocked)
			}
			if err := governor.AcknowledgeHuman(); err != nil {
				t.Fatalf("AcknowledgeHuman() error = %v", err)
			}
			clock.Advance(time.Nanosecond)
			if result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-c", ClientRequestID: "request-c"}); !result.Admitted() {
				t.Fatalf("admission after acknowledgement = %#v, want admitted", result)
			}
		})
	}
}

func TestUnlimitedTextNeverSubtractsPlusGoManualReserve(t *testing.T) {
	clock := newFakeClock()
	events := &eventSink{}
	telemetry := &fakeTelemetry{}
	// Even an exhausted observed remaining counter is not a text-allowance
	// signal under unlimited text and must not gate text admission.
	remaining := 0
	telemetry.Set(policy.TelemetrySnapshot{Remaining: &remaining, UpstreamCircuit: policy.UpstreamCircuitUnknown})
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	unlimited, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	result := unlimited.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-1"})
	if !result.Admitted() {
		t.Fatalf("unlimited admission with remaining=0 = %#v, want admitted (no reserve, no numeric text allowance)", result)
	}
	result.Permit.Start()
	result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	budgets := unlimited.Snapshot().Budgets
	if budgets.ManualReserve != 0 || budgets.ManualReserveRemaining != 0 {
		t.Fatalf("unlimited budgets report a reserve: %#v", budgets)
	}
}

func TestPlusGoInstantRetainsNumericWindowsAndReserveSemantics(t *testing.T) {
	clock := newFakeClock()
	telemetry := &fakeTelemetry{}
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	if governor.Config().EffectiveAllowanceKind() != policy.AllowanceKindPublishedQuota {
		t.Fatal("instant profile must be a published-quota allowance")
	}
	// Remaining 30 with reserve 20 leaves 10 automated headroom: admitted.
	remaining := 30
	telemetry.Set(policy.TelemetrySnapshot{Remaining: &remaining, ResetAt: clock.Now().Add(time.Hour), UpstreamCircuit: policy.UpstreamCircuitUnknown})
	first := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-1"})
	if !first.Admitted() {
		t.Fatalf("instant admission with headroom = %#v, want admitted", first)
	}
	first.Permit.Start()
	first.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
	// Remaining 20 equals the reserve: the automated headroom is zero and the
	// reserve must never be consumed automatically.
	remaining = 20
	telemetry.Set(policy.TelemetrySnapshot{Remaining: &remaining, ResetAt: clock.Now().Add(time.Hour), UpstreamCircuit: policy.UpstreamCircuitUnknown})
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-2"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionUpstreamAllowanceExhausted {
		t.Fatalf("instant admission at the reserve = %#v, want delayed upstream_allowance_exhausted", blocked)
	}
	budgets := governor.Snapshot().Budgets
	if budgets.ManualReserve != 20 || budgets.ManualReserveRemaining != 20 ||
		budgets.Rolling3hCeiling != 140 || budgets.Rolling1hCeiling != 80 || budgets.Rolling10mCeiling != 25 {
		t.Fatalf("instant budgets = %#v, want reserve 20 and 140/80/25 ceilings", budgets)
	}
}

func TestUnknownNeverBecomesUnlimitedFromRepeatedSuccess(t *testing.T) {
	governor, clock, _ := unknownGovernor(t, func(config *policy.Config) { config.TaskBudget = 100 })
	for i := 1; i <= 30; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		if !result.Admitted() {
			t.Fatalf("unknown admission %d = %#v, want admitted under local controls", i, result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		// Spread the attempts across the local windows so the conservative
		// local ceilings (10m=25, 1h=80, 3h=140) do not exhaust; the point of
		// this test is that success never promotes the allowance semantic.
		clock.Advance(time.Minute)
	}
	snapshot := governor.Snapshot()
	if snapshot.AllowanceKind != policy.AllowanceKindUnknown {
		t.Fatalf("unknown kind after repeated success = %q, want unknown (success never upgrades the allowance)", snapshot.AllowanceKind)
	}
	// The conservative local layer stays in force under unknown: explicit
	// local ceilings and a manual-use reserve are rendered and enforced.
	if snapshot.Budgets.Rolling3hCeiling <= 0 || snapshot.Budgets.ManualReserve <= 0 {
		t.Fatalf("unknown budgets dropped the conservative local layer: %#v", snapshot.Budgets)
	}
}

// TestUnknownLocalRollingCeilingsStillEnforced proves the #21 conservative
// local ceilings remain admission gates under the unknown allowance: the
// upstream allowance is unknown, the Runstead local layer is not.
func TestUnknownLocalRollingCeilingsStillEnforced(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultUnknownConfig("policy-unknown", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Rolling10m = 2
	config.Rolling1h = 3
	config.Rolling3h = 5
	config.ManualReserve = 1
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		if !result.Admitted() {
			t.Fatalf("unknown admission %d = %#v", i, result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-3"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionRollingBudgetExhausted {
		t.Fatalf("unknown admission past the local 10m ceiling = %#v, want delayed rolling_budget_exhausted", blocked)
	}
	snapshot := governor.Snapshot()
	if snapshot.AllowanceKind != policy.AllowanceKindUnknown || snapshot.Budgets.Rolling10mCeiling != 2 {
		t.Fatalf("unknown snapshot = %#v, want kind unknown with local ceiling 2", snapshot)
	}
}

// TestLegacyConfigWithoutAllowanceKindNormalizesToPublishedQuota is the #58
// review regression: a legacy config that predates the typed kind (empty
// AllowanceKind on a plus_go_instant profile) must validate, normalize to
// published_quota at construction, enforce the numeric rolling policy and
// emit the kind in snapshots and events. Skipping the rolling gates on an
// empty kind would be fail-open.
func TestLegacyConfigWithoutAllowanceKindNormalizesToPublishedQuota(t *testing.T) {
	events := &eventSink{}
	clock := newFakeClock()
	config := policy.Config{
		AccountPolicyID:       "policy-account-1",
		ProviderID:            "omniroute",
		ModelPool:             "instant",
		AllowanceProfile:      policy.AllowanceProfileInstant,
		Rolling3h:             5,
		ManualReserve:         1,
		Rolling1h:             4,
		Rolling10m:            2,
		TaskBudget:            80,
		RetryBudget:           2,
		QueueCapacity:         16,
		FairnessQuantum:       1,
		MinimumStartInterval:  time.Nanosecond,
		BurstCapacity:         1,
		MaxInFlight:           1,
		RequireSingleAttempt:  true,
		RateResponseThreshold: 3,
		RateResponseWindow:    time.Hour,
		ResetSafetyMargin:     5 * time.Minute,
		RouteSafety:           provider.SafeRouteSafety(),
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("legacy config Validate() error = %v", err)
	}
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
	if err != nil {
		t.Fatalf("legacy config New() error = %v", err)
	}
	snapshot := governor.Snapshot()
	if snapshot.AllowanceKind != policy.AllowanceKindPublishedQuota {
		t.Fatalf("normalized kind = %q, want published_quota", snapshot.AllowanceKind)
	}
	// The numeric rolling policy must be enforced: exhaust the local 10m
	// ceiling of 2 and the third attempt must be rolling-budget blocked.
	for i := 1; i <= 2; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		if !result.Admitted() {
			t.Fatalf("legacy admission %d = %#v", i, result)
		}
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-3"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionRollingBudgetExhausted {
		t.Fatalf("legacy rolling gate = %#v, want delayed rolling_budget_exhausted", blocked)
	}
	// Events must carry the normalized kind, not an empty string.
	governor.DrainEvents()
	for _, event := range events.Events() {
		if event.AllowanceKind != policy.AllowanceKindPublishedQuota {
			t.Fatalf("event allowance kind = %q, want published_quota", event.AllowanceKind)
		}
	}
}

func TestUnknownObservedRemainingStillRestrictsAdmission(t *testing.T) {
	clock := newFakeClock()
	telemetry := &fakeTelemetry{}
	config := policy.DefaultUnknownConfig("policy-unknown", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Telemetry: telemetry})
	if err != nil {
		t.Fatal(err)
	}
	// An observed remaining counter is legitimate evidence under an unknown
	// allowance: it can only restrict admission, never expand it.
	remaining := 0
	telemetry.Set(policy.TelemetrySnapshot{Remaining: &remaining, ResetAt: clock.Now().Add(5 * time.Minute), UpstreamCircuit: policy.UpstreamCircuitUnknown})
	blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-1"})
	if blocked.Code != policy.AdmissionDelayed || blocked.Reason != policy.AdmissionUpstreamAllowanceExhausted || blocked.RetryAt.IsZero() {
		t.Fatalf("unknown admission with observed remaining=0 = %#v, want delayed upstream_allowance_exhausted", blocked)
	}
}

func TestUnlimitedPersistedStateSurvivesRestart(t *testing.T) {
	governor, clock, _ := unlimitedGovernor(t, func(config *policy.Config) { config.TaskBudget = 10 })
	for i := 1; i <= 3; i++ {
		request := "request-" + string(rune('0'+i))
		result := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	state := governor.PersistedState()
	if state.AllowanceKind != policy.AllowanceKindUnlimitedText || state.AllowanceProfile != policy.ProfileLunaUnlimitedText {
		t.Fatalf("persisted allowance = profile %q kind %q", state.AllowanceProfile, state.AllowanceKind)
	}
	restored, err := policy.New(
		policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "omniroute", "instant", provider.SafeRouteSafety()),
		policy.Options{Clock: newFakeClock(), Jitter: fixedJitter{}, Restore: &state})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restored.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 3 || snapshot.Budgets.Rolling10mUsed != 3 || snapshot.NextAttempt != 4 {
		t.Fatalf("restored unlimited protection = %#v, want usage 3/3/3 and next attempt 4", snapshot)
	}
	if task := snapshot.Tasks["task"]; task.Attempts != 3 {
		t.Fatalf("restored task attempts = %#v, want 3", task)
	}
}

// TestAllowanceKindTransitionDoesNotResetDurableState proves that moving
// between published_quota, unlimited_text and unknown never creates a fresh
// allowance ledger: the rolling usage, task attempts, cooldown and circuit
// state all carry over.
func TestAllowanceKindTransitionDoesNotResetDurableState(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	source, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		request := "request-" + string(rune('0'+i))
		result := source.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	rate := source.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: "request-rate"})
	rate.Permit.Start()
	rate.Permit.Finish(policy.Outcome{Class: policy.OutcomeRateCapacity, RetryAfter: 15 * time.Second, UpstreamReached: true})
	state := source.PersistedState()
	if len(state.RollingEvents) != 4 {
		t.Fatalf("persisted ledger = %d events, want 4", len(state.RollingEvents))
	}

	transitions := []struct {
		name   string
		config policy.Config
	}{
		{"published_quota to unlimited_text", policy.DefaultLunaUnlimitedTextConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())},
		{"published_quota to unknown", policy.DefaultUnknownConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			transition.config.MinimumStartInterval = time.Nanosecond
			restored, err := policy.New(transition.config, policy.Options{Clock: newFakeClock(), Jitter: fixedJitter{}, Restore: &state})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := restored.Snapshot()
			if snapshot.Budgets.Rolling3hUsed != 4 || snapshot.Budgets.Rolling10mUsed != 4 {
				t.Fatalf("transition reset the ledger: %#v", snapshot.Budgets)
			}
			if task := snapshot.Tasks["task"]; task.Attempts != 4 {
				t.Fatalf("transition reset task attempts: %#v", task)
			}
			if snapshot.CooldownUntil.IsZero() {
				t.Fatal("transition reset the cooldown state")
			}
			if restored.PersistedState().AllowanceKind != transition.config.AllowanceKind {
				t.Fatalf("restored kind = %q, want %q", restored.PersistedState().AllowanceKind, transition.config.AllowanceKind)
			}
		})
	}

	t.Run("unlimited_text to published_quota", func(t *testing.T) {
		unlimitedState := policy.PersistedState{
			AccountPolicyID:  "policy-account-1",
			AllowanceProfile: policy.ProfileLunaUnlimitedText,
			NextAttempt:      5,
			LastStart:        state.LastStart,
			CooldownUntil:    state.CooldownUntil,
			Circuit:          state.Circuit,
			RollingEvents:    state.RollingEvents,
			TaskStates:       state.TaskStates,
			Ceilings:         state.Ceilings,
		}
		instant := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
		instant.MinimumStartInterval = time.Nanosecond
		restored, err := policy.New(instant, policy.Options{Clock: newFakeClock(), Jitter: fixedJitter{}, Restore: &unlimitedState})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := restored.Snapshot()
		if snapshot.Budgets.Rolling3hUsed != 4 || snapshot.Budgets.Rolling10mUsed != 4 {
			t.Fatalf("transition reset the ledger: %#v", snapshot.Budgets)
		}
		if restored.Snapshot().AllowanceKind != policy.AllowanceKindPublishedQuota {
			t.Fatal("configured published-quota policy must stay authoritative")
		}
	})
}

// TestModelIdentityChangeDoesNotCreateFreshAllowanceLedger proves that model
// naming (or any provider/session metadata change) cannot silently create a
// fresh governor allowance ledger for the same protected account.
func TestModelIdentityChangeDoesNotCreateFreshAllowanceLedger(t *testing.T) {
	clock := newFakeClock()
	config := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Model = "gpt-5.6-legacy"
	source, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		request := "request-" + string(rune('0'+i))
		result := source.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task", ClientRequestID: request})
		result.Permit.Start()
		result.Permit.Finish(policy.Outcome{Class: policy.OutcomeSuccess})
		clock.Advance(time.Nanosecond)
	}
	state := source.PersistedState()
	state.Model = "gpt-5.6-luna" // a model-name change alone is never allowance evidence
	reconfigured := policy.DefaultInstantConfig("policy-account-1", "omniroute", "instant", provider.SafeRouteSafety())
	reconfigured.MinimumStartInterval = time.Nanosecond
	reconfigured.Model = "gpt-5.6-luna"
	restored, err := policy.New(reconfigured, policy.Options{Clock: newFakeClock(), Jitter: fixedJitter{}, Restore: &state})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restored.Snapshot()
	if snapshot.Budgets.Rolling3hUsed != 3 || snapshot.NextAttempt != 4 {
		t.Fatalf("model identity change reset the ledger: %#v", snapshot)
	}
}

// TestUnlimitedTextReceiptAccountingUnchanged proves the receipt-aware path
// still debits every authoritative receipt, rejects replay and blocks the
// lane on hidden amplification under the unlimited-text allowance.
func TestUnlimitedTextReceiptAccountingUnchanged(t *testing.T) {
	events := &eventSink{}
	clock := newFakeClock()
	config := policy.DefaultLunaUnlimitedTextConfig("policy-unlimited", "provider", "model", provider.ReceiptRouteSafety())
	config.MinimumStartInterval = time.Nanosecond
	config.Model = "concrete-model"
	config.RequireSingleAttempt = false
	config.RequireAttemptReceipts = true
	config.AttemptProviderID = "provider"
	config.AccountLaneHash = "lane"
	governor, err := policy.New(config, policy.Options{Clock: clock, Jitter: fixedJitter{}, Events: events})
	if err != nil {
		t.Fatal(err)
	}

	client := &receiptClient{set: receiptSet(clock.Now(), 1)}
	result := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "prompt"},
	}, client, nil)
	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("receipt-aware unlimited Execute() = %#v", result)
	}
	if result.Completion.AttemptDebited != 1 || governor.Snapshot().Budgets.Rolling3hUsed != 1 {
		t.Fatalf("receipt accounting = %#v, want exactly one debit", result.Completion)
	}
	governor.DrainEvents()
	var upstreamEvents int
	for _, event := range events.Events() {
		if event.Kind == policy.EventUpstreamAttempt {
			upstreamEvents++
		}
	}
	if upstreamEvents != 1 {
		t.Fatalf("upstream attempt events = %d, want 1", upstreamEvents)
	}

	// Hidden amplification is still rejected and accounted: two fresh
	// receipts reconcile both debits and block the lane, exactly as before
	// #58. The first receipt set already retained its attempt ids, so the
	// amplified set uses new upstream-owned attempt identities.
	clock.Advance(time.Second)
	amplifiedSet := rebindReceiptSet(receiptSet(clock.Now(), 2), "request-2")
	amplifiedSet.Receipts[0].AttemptID = "attempt-3"
	amplifiedSet.Receipts[1].AttemptID = "attempt-4"
	amplified := governor.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-2",
		ClientRequestID: "request-2",
		ModelPool:       "model",
		ProviderRequest: provider.Request{Prompt: "prompt"},
	}, &receiptClient{set: amplifiedSet}, nil)
	if amplified.Completion.Err == nil || amplified.Completion.AttemptDebited != 2 {
		t.Fatalf("amplified receipt result = %#v, want two debits and a policy error", amplified.Completion)
	}
	if !governor.Snapshot().Telemetry.Unsafe {
		t.Fatal("hidden amplification must mark telemetry unsafe under unlimited text")
	}
	if blocked := governor.TryAdmit(context.Background(), policy.AttemptRequest{TaskID: "task-3", ClientRequestID: "request-3", ModelPool: "model"}); blocked.Code != policy.AdmissionUnsafeProviderAmplification {
		t.Fatalf("post-amplification admission = %#v, want unsafe_provider_amplification", blocked)
	}
}
