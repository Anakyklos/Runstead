package governor

import (
	"context"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// Persistence is the optional governor persistence boundary. A nil
// implementation disables durable governor state (the M1 in-memory behavior);
// the CLI wires the SQLite store. Implementations must never keep a SQLite
// transaction open across the provider call: RecordProviderPrepared is
// invoked after admission and permit start but before client.Complete, and
// RecordProviderFinished after the permit is finished. Each call persists
// projection rows and journal events atomically.
type Persistence interface {
	// RecordProviderPrepared persists the provider attempt intent (TX 1)
	// together with the post-start governor protection state. A crash after
	// this commit leaves durable evidence that the upstream may have been
	// reached.
	RecordProviderPrepared(ctx context.Context, record ProviderPrepared) error
	// RecordProviderFinished persists the classified outcome, receipt
	// evidence and post-finish governor protection state (TX 2).
	RecordProviderFinished(ctx context.Context, record ProviderFinished) error
}

// ProviderPrepared is the durable intent of one governed provider execution,
// persisted before the provider call.
type ProviderPrepared struct {
	TaskID           string
	ClientRequestID  string
	ProviderID       string
	ModelPool        string
	Model            string
	AllowanceProfile AllowanceProfile
	AttemptSequence  int
	StartedAt        time.Time
	ReceiptAware     bool
	TelemetryHealthy bool
	// State is the governor protection projection immediately after Start.
	State PersistedState
}

// ProviderFinished is the classified result of one governed provider
// execution, persisted after the provider call and the permit finish.
type ProviderFinished struct {
	TaskID          string
	ClientRequestID string
	Outcome         OutcomeClass
	UpstreamReached bool
	Uncertain       bool
	DeliveryState   provider.DeliveryState
	AttemptDebited  int
	SelectedBackoff time.Duration
	Circuit         CircuitSnapshot
	RetryEligible   bool
	// Receipts are the sanitized authoritative receipt evidence (#29). The
	// receipt attempt IDs are upstream-owned and never Runstead execution
	// identities.
	Receipts []provider.AttemptReceipt
	// ReceiptErrorCode is the structural validation code when the receipt set
	// was missing or invalid; the attempt is then conservatively uncertain.
	ReceiptErrorCode string
	// State is the governor protection projection after Finish.
	State PersistedState
}

// LedgerEvent is one rolling usage ledger entry.
type LedgerEvent struct {
	At     time.Time
	TaskID string
}

// TaskStateRecord is the governor's per-task usage projection.
type TaskStateRecord struct {
	TaskID      string
	Attempts    int
	Retries     int
	LastTouched time.Time
}

// RequestRecordState is a retained client request record for duplicate
// detection.
type RequestRecordState struct {
	RequestID   string
	State       string // pending, active or completed
	CompletedAt time.Time
}

// AttemptIDRecord is a retained receipt attempt id for replay detection.
type AttemptIDRecord struct {
	AttemptID string
	SeenAt    time.Time
}

// BudgetCeilings is the informational account policy snapshot persisted with
// the governor projection so inspect can render usage against ceilings.
// ManualReserve is the profile-specific manual reserve and is only nonzero
// for published-quota allowances (#58); unlimited-text and unknown policies
// persist zero.
type BudgetCeilings struct {
	Rolling3h     int
	Rolling1h     int
	Rolling10m    int
	TaskBudget    int
	RetryBudget   int
	ManualReserve int
}

// PersistedState is the serializable account-protection projection (#21):
// the rolling usage ledger, cooldown, circuit and retained accounting state
// required so that restarting the process does not reset account protection.
// In-flight and queue state are process-local and deliberately not persisted.
// AllowanceKind is the typed upstream allowance semantic (#58); when it is
// empty the caller derives it from AllowanceProfile, so legacy persisted
// projections remain usable without a schema migration. Changing the
// allowance kind never resets the durable ledger, task, circuit, cooldown or
// receipt-replay state below.
type PersistedState struct {
	AccountPolicyID  string
	ProviderID       string
	ModelPool        string
	Model            string
	AllowanceProfile AllowanceProfile
	AllowanceKind    AllowanceKind
	NextAttempt      int
	LastStart        time.Time
	CooldownUntil    time.Time
	Circuit          CircuitSnapshot
	RateEvents       []time.Time
	LastRateReset    time.Time
	Telemetry        PersistedTelemetry
	RollingEvents    []LedgerEvent
	TaskStates       []TaskStateRecord
	RequestRecords   []RequestRecordState
	AttemptIDs       []AttemptIDRecord
	Ceilings         BudgetCeilings
}

// PersistedTelemetry is the retained telemetry evidence that affects
// admission after restart.
type PersistedTelemetry struct {
	Available         *int
	ResetAt           time.Time
	CooldownUntil     time.Time
	RateLimited       bool
	CapacityExhausted bool
	UpstreamCircuit   UpstreamCircuitState
	Unsafe            bool
}
