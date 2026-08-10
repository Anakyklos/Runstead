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
	AllowanceProfileInstant           AllowanceProfile = "plus_go_instant"
	AllowanceProfileReasoning         AllowanceProfile = "reasoning"
	AllowanceProfileUnknown           AllowanceProfile = "unknown"
	AllowanceProfileLunaUnlimitedText AllowanceProfile = "luna_unlimited_text"
	// Short aliases keep call sites readable without introducing another type.
	ProfileInstant           = AllowanceProfileInstant
	ProfileReasoning         = AllowanceProfileReasoning
	ProfileUnknown           = AllowanceProfileUnknown
	ProfileLunaUnlimitedText = AllowanceProfileLunaUnlimitedText
)

// AllowanceKind is the typed upstream allowance semantic (#58). It answers
// "does the configured/observed upstream surface publish a numeric text
// allowance?" independently of the historical profile identifier and of
// Runstead-local workload ceilings (task budgets, pacing, circuits, cooldown),
// which remain active for every kind.
type AllowanceKind string

const (
	// AllowanceKindPublishedQuota means the upstream publishes or exposes a
	// numeric rolling text allowance. Rolling ceilings and a manual reserve
	// are meaningful and enforced.
	AllowanceKindPublishedQuota AllowanceKind = "published_quota"
	// AllowanceKindUnlimitedText means the account/plan is explicitly
	// configured (or observed through trustworthy evidence) as unlimited
	// text. There is no fabricated numeric rolling quota and no shared
	// numeric allowance to protect with a manual reserve. Runstead-local
	// workload controls and fail-closed security behavior remain active.
	AllowanceKindUnlimitedText AllowanceKind = "unlimited_text"
	// AllowanceKindUnknown means there is no evidence either way: no numeric
	// allowance and no confirmed unlimited contract. Unknown must never
	// silently become unlimited because requests keep succeeding; admission
	// stays governed by the local workload controls.
	AllowanceKindUnknown AllowanceKind = "unknown"
)

// AllowanceKindForProfile maps the historical profile identifier to its typed
// allowance semantic. The mapping is fixed so a persisted profile survives
// restarts and legacy rows without a schema migration.
func AllowanceKindForProfile(profile AllowanceProfile) AllowanceKind {
	switch profile {
	case AllowanceProfileInstant, AllowanceProfileReasoning:
		return AllowanceKindPublishedQuota
	case AllowanceProfileLunaUnlimitedText:
		return AllowanceKindUnlimitedText
	case AllowanceProfileUnknown:
		return AllowanceKindUnknown
	default:
		return ""
	}
}

func (k AllowanceKind) Valid() bool {
	switch k {
	case AllowanceKindPublishedQuota, AllowanceKindUnlimitedText, AllowanceKindUnknown:
		return true
	default:
		return false
	}
}

// EffectiveAllowanceKind returns the explicit kind when configured and the
// profile-derived kind otherwise. It is only meaningful after Validate.
func (c Config) EffectiveAllowanceKind() AllowanceKind {
	if c.AllowanceKind.Valid() {
		return c.AllowanceKind
	}
	return AllowanceKindForProfile(c.AllowanceProfile)
}

type Config struct {
	AccountPolicyID  string
	ProviderID       string
	ModelPool        string
	Model            string
	AllowanceProfile AllowanceProfile
	// AllowanceKind is the typed upstream allowance semantic (#58). When
	// empty, Validate derives it from AllowanceProfile. When set, it must
	// match the profile-derived kind.
	AllowanceKind          AllowanceKind
	Rolling3h              int
	ManualReserve          int
	Rolling1h              int
	Rolling10m             int
	TaskBudget             int
	RetryBudget            int
	QueueCapacity          int
	FairnessQuantum        int
	MinimumStartInterval   time.Duration
	BurstCapacity          int
	MaxInFlight            int
	RequireSingleAttempt   bool
	RequireAttemptReceipts bool
	AttemptProviderID      string
	AccountLaneHash        string
	RateResponseThreshold  int
	RateResponseWindow     time.Duration
	ResetSafetyMargin      time.Duration
	RouteSafety            provider.RouteSafety
}

func DefaultInstantConfig(accountPolicyID, providerID, modelPool string, safety provider.RouteSafety) Config {
	return Config{
		AccountPolicyID:       accountPolicyID,
		ProviderID:            providerID,
		ModelPool:             modelPool,
		AllowanceProfile:      AllowanceProfileInstant,
		AllowanceKind:         AllowanceKindPublishedQuota,
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

// DefaultLunaUnlimitedTextConfig is the explicit unlimited-text allowance
// policy (#58). It carries no fabricated numeric rolling quota and no manual
// reserve: unlimited text means no known numeric upstream text-message quota,
// never ungoverned Runstead execution. All local workload controls (serial
// lane, pacing, task/retry budgets, queue/fairness, cooldown, circuits,
// fail-closed security and attempt receipts) stay exactly as strict as the
// Instant policy.
func DefaultLunaUnlimitedTextConfig(accountPolicyID, providerID, modelPool string, safety provider.RouteSafety) Config {
	config := DefaultInstantConfig(accountPolicyID, providerID, modelPool, safety)
	config.AllowanceProfile = AllowanceProfileLunaUnlimitedText
	config.AllowanceKind = AllowanceKindUnlimitedText
	config.Rolling3h = 0
	config.Rolling1h = 0
	config.Rolling10m = 0
	config.ManualReserve = 0
	return config
}

// DefaultUnknownConfig is the no-evidence allowance policy (#58). The
// upstream allowance is unknown, so the conservative local layer stays
// mandatory exactly as in the #21 contract: explicit positive local rolling
// ceilings and a local manual-use reserve are enforced as Runstead workload
// protections, not as upstream allowance claims. The convenience defaults
// reuse the same conservative local family as the Instant profile; operators
// must review and may override them, and repeated successful calls never
// promote Unknown to unlimited.
func DefaultUnknownConfig(accountPolicyID, providerID, modelPool string, safety provider.RouteSafety) Config {
	config := DefaultInstantConfig(accountPolicyID, providerID, modelPool, safety)
	config.AllowanceProfile = AllowanceProfileUnknown
	config.AllowanceKind = AllowanceKindUnknown
	return config
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
	case AllowanceProfileInstant, AllowanceProfileReasoning, AllowanceProfileUnknown, AllowanceProfileLunaUnlimitedText:
	default:
		return fmt.Errorf("unsupported allowance profile %q", c.AllowanceProfile)
	}
	kind := c.AllowanceKind
	if kind == "" {
		kind = AllowanceKindForProfile(c.AllowanceProfile)
	} else if !kind.Valid() {
		return fmt.Errorf("unsupported allowance kind %q", c.AllowanceKind)
	} else if AllowanceKindForProfile(c.AllowanceProfile) != kind {
		return fmt.Errorf("allowance kind %q does not match profile %q", c.AllowanceKind, c.AllowanceProfile)
	}
	switch kind {
	case AllowanceKindPublishedQuota:
		if c.Rolling3h <= 0 || c.Rolling1h <= 0 || c.Rolling10m <= 0 {
			return errors.New("published-quota allowances require positive rolling ceilings")
		}
		if c.Rolling10m >= c.Rolling1h || c.Rolling1h >= c.Rolling3h {
			return errors.New("rolling windows require 10m < 1h < 3h ceilings")
		}
		if c.ManualReserve < 0 || c.ManualReserve >= c.Rolling3h {
			return errors.New("manual reserve must be non-negative and below the 3h ceiling")
		}
	case AllowanceKindUnlimitedText:
		if c.Rolling3h != 0 || c.Rolling1h != 0 || c.Rolling10m != 0 {
			return errors.New("unlimited-text allowances must not fabricate numeric rolling ceilings")
		}
		if c.ManualReserve != 0 {
			return errors.New("unlimited-text allowances have no shared numeric allowance to protect with a manual reserve")
		}
	case AllowanceKindUnknown:
		// The upstream allowance is unknown, so the conservative local layer
		// remains mandatory (#21 contract, #58 review): explicit positive
		// local rolling ceilings and a local manual-use reserve are enforced
		// as Runstead workload protections. This is not an upstream claim:
		// Unknown still never becomes unlimited from repeated success.
		if c.Rolling3h <= 0 || c.Rolling1h <= 0 || c.Rolling10m <= 0 {
			return errors.New("unknown allowances require explicit conservative local rolling ceilings")
		}
		if c.Rolling10m >= c.Rolling1h || c.Rolling1h >= c.Rolling3h {
			return errors.New("rolling windows require 10m < 1h < 3h ceilings")
		}
		if c.ManualReserve < 0 || c.ManualReserve >= c.Rolling3h {
			return errors.New("manual reserve must be non-negative and below the 3h ceiling")
		}
	default:
		return fmt.Errorf("unsupported allowance kind %q", kind)
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
	if c.RequireSingleAttempt == c.RequireAttemptReceipts {
		return errors.New("exactly one attempt-accounting requirement must be enabled")
	}
	if c.RequireSingleAttempt && c.RouteSafety.AttemptAccounting != provider.AttemptAccountingSingle {
		return errors.New("single-attempt requirement requires single-attempt route safety")
	}
	if c.RequireAttemptReceipts && c.RouteSafety.AttemptAccounting != provider.AttemptAccountingReceipts {
		return errors.New("receipt requirement requires receipt-aware route safety")
	}
	if c.RequireAttemptReceipts && strings.TrimSpace(c.AccountLaneHash) == "" {
		return errors.New("receipt requirement requires an account lane hash")
	}
	if c.RequireAttemptReceipts && strings.TrimSpace(c.Model) == "" {
		return errors.New("receipt requirement requires a concrete model identity")
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
	AdmissionMissingAttemptReceipts        AdmissionCode = "missing_attempt_receipts"
)

type AttemptRequest struct {
	TaskID          string
	ClientRequestID string
	ModelPool       string
	Retry           bool
	ProviderRequest provider.Request
}

type AdmissionResult struct {
	Code             AdmissionCode
	Reason           AdmissionCode
	RetryAt          time.Time
	Delay            time.Duration
	Permit           *Permit
	Err              error
	TelemetryHealthy bool
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
	DeliveryState   provider.DeliveryState
}

func effectiveDeliveryState(state provider.DeliveryState) provider.DeliveryState {
	if state.Valid() {
		return state
	}
	return provider.DeliverySentUnconfirmed
}

func deliveryUpstreamReached(state provider.DeliveryState) bool {
	return effectiveDeliveryState(state) != provider.DeliveryNotSent
}

func applyDeliveryEvidence(outcome Outcome) Outcome {
	if !outcome.DeliveryState.Valid() {
		return outcome
	}
	effective := effectiveDeliveryState(outcome.DeliveryState)
	outcome.UpstreamReached = deliveryUpstreamReached(outcome.DeliveryState)
	switch effective {
	case provider.DeliverySentUnconfirmed, provider.DeliveryResponseStarted:
		outcome.Class = OutcomeUncertainReached
	case provider.DeliveryNotSent:
		if outcome.Class == OutcomeCancelledBeforeUpstream {
			return outcome
		}
	}
	return outcome
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
	EventAdmission        EventKind = "admission"
	EventAttemptStarted   EventKind = "attempt_started"
	EventAttemptFinished  EventKind = "attempt_finished"
	EventUpstreamAttempt  EventKind = "upstream_attempt"
	EventUncertainAttempt EventKind = "uncertain_attempt"
	EventCircuit          EventKind = "circuit"
)

type TelemetrySummary struct {
	Available         bool
	Remaining         *int
	ResetAt           time.Time
	CooldownUntil     time.Time
	RateLimited       bool
	CapacityExhausted bool
	UpstreamCircuit   UpstreamCircuitState
	// Unsafe reports that conservative accounting is active (#29): the
	// governor refuses further admission until the state is resolved. It is
	// restored from the persisted projection across restart.
	Unsafe bool
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
	Kind                  EventKind
	AccountPolicyID       string
	ProviderID            string
	ModelPool             string
	Model                 string
	AllowanceProfile      AllowanceProfile
	AllowanceKind         AllowanceKind
	TaskID                string
	ClientRequestID       string
	AttemptSequence       int
	AttemptID             string
	AttemptTrigger        provider.AttemptTrigger
	AttemptReceiptOutcome provider.AttemptOutcome
	UpstreamReached       bool
	DeliveryState         provider.DeliveryState
	Admission             AdmissionCode
	Reason                AdmissionCode
	Delay                 time.Duration
	RetryAt               time.Time
	BudgetsBefore         BudgetSnapshot
	BudgetsAfter          BudgetSnapshot
	Telemetry             TelemetrySummary
	Outcome               OutcomeClass
	CooldownUntil         time.Time
	SelectedBackoff       time.Duration
	CircuitFrom           CircuitState
	CircuitTo             CircuitState
	CircuitReason         OutcomeClass
	TelemetryHealthy      bool
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
	Model              string
	AllowanceProfile   AllowanceProfile
	AllowanceKind      AllowanceKind
	InFlight           bool
	QueueLength        int
	NextAttempt        int
	LastStart          time.Time
	CooldownUntil      time.Time
	Budgets            BudgetSnapshot
	Circuit            CircuitSnapshot
	Telemetry          TelemetrySummary
	PendingEvents      int
	Tasks              map[string]TaskSnapshot
	RetainedRequestIDs int
	RetainedAttemptIDs int
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
	// Persistence is the optional durable-state boundary (issue #8). When
	// nil, the governor keeps the M1 in-memory behavior.
	Persistence Persistence
	// Restore is an optional persisted protection projection applied at
	// construction so a restart does not reset account protection (#21).
	Restore *PersistedState
}

var (
	ErrPermitCompleted         = errors.New("permit already completed")
	ErrPermitNotStarted        = errors.New("permit has not started")
	ErrPermitStarted           = errors.New("permit already started")
	ErrGovernorClosed          = errors.New("governor is closed")
	ErrAttemptReceiptsRequired = errors.New("authoritative attempt receipts are required")
	ErrAttemptReceiptReplayed  = errors.New("attempt receipt was already reconciled")
	// ErrProviderOutcomePersist reports that the classified provider outcome
	// (TX 2) could not be persisted after the upstream call returned. The
	// provider attempt stays durably 'prepared' (the upstream may have been
	// reached), so the runtime must keep the task resumable instead of
	// finalizing it terminally; recovery reconciles the attempt
	// conservatively (issue #13 review).
	ErrProviderOutcomePersist = errors.New("durable provider outcome could not be persisted")
)

type FinishResult struct {
	Outcome         OutcomeClass
	RetryEligible   bool
	SelectedBackoff time.Duration
	AttemptDebited  int
	Circuit         CircuitSnapshot
	DeliveryState   provider.DeliveryState
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
	if errors.Is(err, context.Canceled) {
		return Outcome{Class: OutcomeCancelledBeforeUpstream, DeliveryState: response.Metadata.DeliveryState}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Outcome{Class: OutcomeTimeout, DeliveryState: response.Metadata.DeliveryState}
	}
	if err == nil {
		if strings.TrimSpace(response.Text) == "" {
			return Outcome{Class: OutcomeEmptyResponse, UpstreamReached: true, DeliveryState: response.Metadata.DeliveryState}
		}
		return Outcome{Class: OutcomeSuccess, UpstreamReached: true, DeliveryState: response.Metadata.DeliveryState}
	}
	return Outcome{Class: OutcomeUncertainReached, UpstreamReached: true, DeliveryState: response.Metadata.DeliveryState}
}
