package governor

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type AllowanceProfile string

const (
	AllowanceProfileInstant   AllowanceProfile = "plus_go_instant"
	AllowanceProfileReasoning AllowanceProfile = "reasoning"
	AllowanceProfileUnknown   AllowanceProfile = "unknown"
	// Short aliases keep call sites readable without introducing another type.
	ProfileInstant   = AllowanceProfileInstant
	ProfileReasoning = AllowanceProfileReasoning
	ProfileUnknown   = AllowanceProfileUnknown
)

type Config struct {
	AccountPolicyID       string
	ProviderID            string
	ModelPool             string
	AllowanceProfile      AllowanceProfile
	Rolling3h             int
	ManualReserve         int
	Rolling1h             int
	Rolling10m            int
	TaskBudget            int
	RetryBudget           int
	QueueCapacity         int
	FairnessQuantum       int
	MinimumStartInterval  time.Duration
	BurstCapacity         int
	MaxInFlight           int
	RequireSingleAttempt  bool
	RateResponseThreshold int
	RateResponseWindow    time.Duration
	ResetSafetyMargin     time.Duration
	RouteSafety           provider.RouteSafety
}

func DefaultInstantConfig(accountPolicyID, providerID, modelPool string, safety provider.RouteSafety) Config {
	return Config{
		AccountPolicyID:       accountPolicyID,
		ProviderID:            providerID,
		ModelPool:             modelPool,
		AllowanceProfile:      AllowanceProfileInstant,
		Rolling3h:             140,
		ManualReserve:         20,
		Rolling1h:             80,
		Rolling10m:            25,
		TaskBudget:            80,
		RetryBudget:           2,
		QueueCapacity:         16,
		FairnessQuantum:       1,
		MinimumStartInterval:  5 * time.Second,
		BurstCapacity:         1,
		MaxInFlight:           1,
		RequireSingleAttempt:  true,
		RateResponseThreshold: 3,
		RateResponseWindow:    time.Hour,
		ResetSafetyMargin:     5 * time.Minute,
		RouteSafety:           safety,
	}
}

func (c Config) Validate() error {
	for name, value := range map[string]string{
		"account policy identifier": c.AccountPolicyID,
		"provider identifier":       c.ProviderID,
		"model pool":                c.ModelPool,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	switch c.AllowanceProfile {
	case AllowanceProfileInstant, AllowanceProfileReasoning, AllowanceProfileUnknown:
	default:
		return fmt.Errorf("unsupported allowance profile %q", c.AllowanceProfile)
	}
	if c.Rolling3h <= 0 || c.Rolling1h <= 0 || c.Rolling10m <= 0 {
		return errors.New("rolling ceilings must be positive")
	}
	if c.Rolling10m >= c.Rolling1h || c.Rolling1h >= c.Rolling3h {
		return errors.New("rolling windows require 10m < 1h < 3h ceilings")
	}
	if c.ManualReserve < 0 || c.ManualReserve >= c.Rolling3h {
		return errors.New("manual reserve must be non-negative and below the 3h ceiling")
	}
	if c.TaskBudget <= 0 || c.RetryBudget < 0 || c.QueueCapacity <= 0 || c.FairnessQuantum <= 0 {
		return errors.New("task, queue and fairness budgets are invalid")
	}
	if c.MinimumStartInterval <= 0 {
		return errors.New("minimum start interval must be positive")
	}
	if c.BurstCapacity != 1 {
		return errors.New("burst capacity must be exactly one")
	}
	if c.MaxInFlight != 1 {
		return errors.New("max_in_flight must be exactly one for a personal ChatGPT Web account")
	}
	if !c.RequireSingleAttempt {
		return errors.New("single-attempt requirement must be enabled")
	}
	if c.RateResponseThreshold <= 0 || c.RateResponseWindow <= 0 || c.ResetSafetyMargin <= 0 {
		return errors.New("circuit thresholds are invalid")
	}
	if err := c.RouteSafety.Validate(); err != nil {
		return err
	}
	return nil
}

type AdmissionCode string

const (
	AdmissionAdmitted                      AdmissionCode = "admitted"
	AdmissionDelayed                       AdmissionCode = "delayed"
	AdmissionContextCancelled              AdmissionCode = "context_cancelled"
	AdmissionQueueFull                     AdmissionCode = "queue_full"
	AdmissionTaskDeadlineExceeded          AdmissionCode = "task_deadline_exceeded"
	AdmissionTaskBudgetExhausted           AdmissionCode = "task_budget_exhausted"
	AdmissionRollingBudgetExhausted        AdmissionCode = "rolling_budget_exhausted"
	AdmissionUpstreamAllowanceExhausted    AdmissionCode = "upstream_allowance_exhausted"
	AdmissionCooldownActive                AdmissionCode = "cooldown_active"
	AdmissionCircuitOpen                   AdmissionCode = "circuit_open"
	AdmissionUnsafeProviderAmplification   AdmissionCode = "unsafe_provider_amplification"
	AdmissionUnsafeConfiguration           AdmissionCode = "unsafe_configuration"
	AdmissionHumanAcknowledgementRequired  AdmissionCode = "human_acknowledgement_required"
	AdmissionRetryBudgetExhausted          AdmissionCode = "retry_budget_exhausted"
	AdmissionAuthenticationRefreshRequired AdmissionCode = "authentication_refresh_required"
	AdmissionDuplicateClientRequest        AdmissionCode = "duplicate_client_request"
	AdmissionGovernorClosed                AdmissionCode = "governor_closed"
)

type AttemptRequest struct {
	TaskID          string
	ClientRequestID string
	ModelPool       string
	Retry           bool
	ProviderRequest provider.Request
}

type AdmissionResult struct {
	Code    AdmissionCode
	Reason  AdmissionCode
	RetryAt time.Time
	Delay   time.Duration
	Permit  *Permit
	Err     error
}

func (r AdmissionResult) Admitted() bool {
	return r.Code == AdmissionAdmitted && r.Permit != nil
}

type AdmissionError struct {
	Code  AdmissionCode
	Cause error
}

func (e *AdmissionError) Error() string {
	if e.Cause == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *AdmissionError) Unwrap() error { return e.Cause }

type OutcomeClass string

const (
	OutcomeSuccess                 OutcomeClass = "success"
	OutcomeRateCapacity            OutcomeClass = "rate_or_capacity"
	OutcomeAuthenticationExpired   OutcomeClass = "authentication_expired"
	OutcomeAuthenticationDenied    OutcomeClass = "authentication_denied"
	OutcomeHTTP403                 OutcomeClass = "http_403"
	OutcomeLoginChallenge          OutcomeClass = "login_challenge"
	OutcomeCAPTCHA                 OutcomeClass = "captcha"
	OutcomeSuspiciousActivity      OutcomeClass = "suspicious_activity"
	OutcomeAccountWarning          OutcomeClass = "account_warning"
	OutcomeFeatureRestriction      OutcomeClass = "feature_restriction"
	OutcomeConnectionReset         OutcomeClass = "connection_reset"
	OutcomeTimeout                 OutcomeClass = "timeout"
	OutcomeEmptyResponse           OutcomeClass = "empty_response"
	OutcomeMalformedUpstream       OutcomeClass = "malformed_upstream_response"
	OutcomeUpstreamServerFailure   OutcomeClass = "upstream_server_failure"
	OutcomeCancelledBeforeUpstream OutcomeClass = "cancelled_before_upstream"
	OutcomeUncertainReached        OutcomeClass = "uncertain_reached"
)

type Outcome struct {
	Class           OutcomeClass
	RetryAfter      time.Duration
	ResetAt         time.Time
	UpstreamReached bool
}

type CircuitState string

const (
	CircuitClosed              CircuitState = "closed"
	CircuitOpenUntil           CircuitState = "open_until"
	CircuitHumanReviewRequired CircuitState = "human_review_required"
)

type UpstreamCircuitState string

const (
	UpstreamCircuitUnknown UpstreamCircuitState = "unknown"
	UpstreamCircuitClosed  UpstreamCircuitState = "closed"
	UpstreamCircuitOpen    UpstreamCircuitState = "open"
)

type TelemetrySnapshot struct {
	Remaining         *int
	ResetAt           time.Time
	CooldownUntil     time.Time
	RetryAfter        time.Duration
	RateLimited       bool
	CapacityExhausted bool
	UpstreamCircuit   UpstreamCircuitState
	RouteSafety       *provider.RouteSafety
}

type TelemetrySource interface {
	Snapshot(context.Context) (TelemetrySnapshot, error)
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

func (realClock) NewTimer(delay time.Duration) Timer { return realTimer{timer: time.NewTimer(delay)} }

type Jitter interface {
	Apply(time.Duration, int) time.Duration
}

type randomJitter struct {
	mu  sync.Mutex
	rng *rand.Rand
}

func (j *randomJitter) Apply(base time.Duration, _ int) time.Duration {
	if base <= 0 {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	extra := base / 2
	return base + time.Duration(j.rng.Int63n(int64(extra)+1))
}

type EventKind string

const (
	EventAdmission       EventKind = "admission"
	EventAttemptStarted  EventKind = "attempt_started"
	EventAttemptFinished EventKind = "attempt_finished"
	EventCircuit         EventKind = "circuit"
)

type TelemetrySummary struct {
	Available         bool
	Remaining         *int
	ResetAt           time.Time
	CooldownUntil     time.Time
	RateLimited       bool
	CapacityExhausted bool
	UpstreamCircuit   UpstreamCircuitState
}

type BudgetSnapshot struct {
	Rolling3hUsed          int
	Rolling3hCeiling       int
	Automated3hCeiling     int
	Rolling1hUsed          int
	Rolling1hCeiling       int
	Rolling10mUsed         int
	Rolling10mCeiling      int
	TaskUsed               int
	TaskCeiling            int
	RetriesUsed            int
	RetryCeiling           int
	ManualReserve          int
	ManualReserveRemaining int
}

type Event struct {
	Kind             EventKind
	AccountPolicyID  string
	ProviderID       string
	ModelPool        string
	AllowanceProfile AllowanceProfile
	TaskID           string
	ClientRequestID  string
	AttemptSequence  int
	Admission        AdmissionCode
	Reason           AdmissionCode
	Delay            time.Duration
	RetryAt          time.Time
	BudgetsBefore    BudgetSnapshot
	BudgetsAfter     BudgetSnapshot
	Telemetry        TelemetrySummary
	Outcome          OutcomeClass
	CooldownUntil    time.Time
	SelectedBackoff  time.Duration
	CircuitFrom      CircuitState
	CircuitTo        CircuitState
	CircuitReason    OutcomeClass
	TelemetryHealthy bool
}

type EventSink interface {
	Emit(Event)
}

type CircuitSnapshot struct {
	State           CircuitState
	Reason          OutcomeClass
	OpenUntil       time.Time
	RefreshRequired bool
}

type Snapshot struct {
	AccountPolicyID    string
	ProviderID         string
	ModelPool          string
	AllowanceProfile   AllowanceProfile
	InFlight           bool
	QueueLength        int
	NextAttempt        int
	LastStart          time.Time
	CooldownUntil      time.Time
	Budgets            BudgetSnapshot
	Circuit            CircuitSnapshot
	Telemetry          TelemetrySummary
	Tasks              map[string]TaskSnapshot
	RetainedRequestIDs int
	RetainedTaskStates int
}

type TaskSnapshot struct {
	TaskID   string
	Attempts int
	Retries  int
}

type Options struct {
	Clock     Clock
	Jitter    Jitter
	Telemetry TelemetrySource
	Events    EventSink
}

var (
	ErrPermitCompleted  = errors.New("permit already completed")
	ErrPermitNotStarted = errors.New("permit has not started")
	ErrPermitStarted    = errors.New("permit already started")
	ErrGovernorClosed   = errors.New("governor is closed")
)

type FinishResult struct {
	Outcome         OutcomeClass
	RetryEligible   bool
	SelectedBackoff time.Duration
	AttemptDebited  int
	Circuit         CircuitSnapshot
	Err             error
}

type ExecutionResult struct {
	Admission  AdmissionResult
	Response   provider.Response
	Completion FinishResult
	Err        error
}

type OutcomeClassifier func(provider.Response, error) Outcome

func defaultOutcome(response provider.Response, err error) Outcome {
	if err == nil {
		if strings.TrimSpace(response.Text) == "" {
			return Outcome{Class: OutcomeEmptyResponse, UpstreamReached: true}
		}
		return Outcome{Class: OutcomeSuccess, UpstreamReached: true}
	}
	return Outcome{Class: OutcomeUncertainReached, UpstreamReached: true}
}
