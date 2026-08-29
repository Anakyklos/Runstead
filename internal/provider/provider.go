package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

var ErrUnsafeRoute = errors.New("provider route cannot guarantee authoritative upstream attempt accounting")
var ErrGatewayContractUnhealthy = errors.New("provider gateway contract is not healthy")

// GatewayContractHealth is the health of the provider's management gateway
// contract. It is deliberately distinct from upstream or model-service
// health, and its zero value is conservative.
type GatewayContractHealth uint8

const (
	GatewayContractHealthUnknown GatewayContractHealth = iota
	GatewayContractHealthHealthy
	GatewayContractHealthDegraded
	GatewayContractHealthProtocolChanged
)

func (h GatewayContractHealth) String() string {
	switch h {
	case GatewayContractHealthHealthy:
		return "healthy"
	case GatewayContractHealthDegraded:
		return "degraded"
	case GatewayContractHealthProtocolChanged:
		return "protocol_changed"
	default:
		return "unknown"
	}
}

type GatewayContractHealthResult struct {
	State      GatewayContractHealth
	ReasonCode string
	Endpoint   string
	CheckedAt  time.Time
}

func (r GatewayContractHealthResult) Healthy() bool {
	return r.State == GatewayContractHealthHealthy
}

// ContractHealthAware is an optional provider capability. Providers without
// management-contract health retain their existing behavior.
type ContractHealthAware interface {
	GatewayContractHealth() GatewayContractHealthResult
}

type SingleAttemptGuarantee uint8

const (
	SingleAttemptUnknown SingleAttemptGuarantee = iota
	SingleAttemptGuaranteed
)

type AmplificationStatus uint8

const (
	AmplificationUnknown AmplificationStatus = iota
	AmplificationDisabled
	AmplificationEnabled
)

type AttemptAccounting uint8

const (
	AttemptAccountingUnknown AttemptAccounting = iota
	AttemptAccountingSingle
	AttemptAccountingReceipts
)

// RouteSafety is an executable declaration of the provider behavior required
// by the protected account lane. Unknown values fail closed.
type RouteSafety struct {
	AttemptAccounting AttemptAccounting
	SingleAttempt     SingleAttemptGuarantee
	InternalRetries   AmplificationStatus
	CooldownReplay    AmplificationStatus
	AccountPooling    AmplificationStatus
	AutomaticFallback AmplificationStatus
	ComboRouting      AmplificationStatus
}

func SafeRouteSafety() RouteSafety {
	return RouteSafety{
		AttemptAccounting: AttemptAccountingSingle,
		SingleAttempt:     SingleAttemptGuaranteed,
		InternalRetries:   AmplificationDisabled,
		CooldownReplay:    AmplificationDisabled,
		AccountPooling:    AmplificationDisabled,
		AutomaticFallback: AmplificationDisabled,
		ComboRouting:      AmplificationDisabled,
	}
}

func ReceiptRouteSafety() RouteSafety {
	// M1 keeps one upstream attempt per protected completion. Executor retries
	// must re-enter the governor as a new completion until per-request budgets
	// exist for provider amplification.
	return RouteSafety{
		AttemptAccounting: AttemptAccountingReceipts,
		InternalRetries:   AmplificationDisabled,
		CooldownReplay:    AmplificationDisabled,
		AccountPooling:    AmplificationDisabled,
		AutomaticFallback: AmplificationDisabled,
		ComboRouting:      AmplificationDisabled,
	}
}

func (s RouteSafety) Validate() error {
	switch s.AttemptAccounting {
	case AttemptAccountingSingle:
		if s.SingleAttempt != SingleAttemptGuaranteed {
			return fmt.Errorf("%w: single-attempt guarantee is unknown", ErrUnsafeRoute)
		}
		for name, value := range map[string]AmplificationStatus{
			"internal retries":   s.InternalRetries,
			"cooldown replay":    s.CooldownReplay,
			"account pooling":    s.AccountPooling,
			"automatic fallback": s.AutomaticFallback,
			"combo routing":      s.ComboRouting,
		} {
			if value != AmplificationDisabled {
				return fmt.Errorf("%w: %s is not explicitly disabled", ErrUnsafeRoute, name)
			}
		}
	case AttemptAccountingReceipts:
		if s.SingleAttempt != SingleAttemptUnknown {
			return fmt.Errorf("%w: receipt-aware route must not claim single-attempt execution", ErrUnsafeRoute)
		}
		for name, value := range map[string]AmplificationStatus{
			"internal retries":   s.InternalRetries,
			"cooldown replay":    s.CooldownReplay,
			"account pooling":    s.AccountPooling,
			"automatic fallback": s.AutomaticFallback,
			"combo routing":      s.ComboRouting,
		} {
			if value != AmplificationDisabled {
				return fmt.Errorf("%w: M1 receipt route cannot amplify through %s", ErrUnsafeRoute, name)
			}
		}
	default:
		return fmt.Errorf("%w: attempt accounting mode is unknown", ErrUnsafeRoute)
	}
	return nil
}

func (s RouteSafety) Equal(other RouteSafety) bool {
	return s == other
}

// CompatAdapterVersion identifies the compatible-provider composition surface
// (#14/#86). It is the single adapter-version identity shared by execution
// evidence and request telemetry; bump it when the adapter set or its
// behavior changes meaningfully.
const CompatAdapterVersion = "compatible-provider-v0.1"

// SafetyAware allows an adapter to expose the same executable declaration it
// was configured with. A governor can reject a mismatch before Complete.
type SafetyAware interface {
	RouteSafety() RouteSafety
}

// AttemptReceiptAware marks a client whose Response metadata must contain a
// validated authoritative receipt set for each protected Complete call.
type AttemptReceiptAware interface {
	AttemptReceiptsEnabled() bool
}

type Request struct {
	Protocol        protocol.Version
	Prompt          string
	Model           string
	ClientRequestID string
}

type Response struct {
	Text     string
	Metadata ResponseMetadata
}

// ResponseMetadata contains provider-neutral, sanitized observations about a
// completed request. It deliberately excludes prompts, response bodies,
// credentials and raw headers.
type ResponseMetadata struct {
	StatusCode      int
	RequestID       string
	SessionID       string
	Duration        time.Duration
	RetryAfter      time.Duration
	ResetAt         time.Time
	Endpoint        string
	Model           string
	DeliveryState   DeliveryState
	AttemptReceipts *AttemptReceiptSet

	// Issue #39 request telemetry. All fields are conservative zero values
	// unless the adapter can prove the observation; nothing is ever guessed.

	// AdapterVersion is the pinned adapter/composition version
	// (CompatAdapterVersion for the compatible families; adapter-owned for
	// legacy transports). Empty only when the record was never populated.
	AdapterVersion string

	// Transport is the stable transport identifier (for example
	// "openaicompat-http"). Empty only when the record was never populated.
	Transport string

	// FirstByteLatency is the latency from request start to the first
	// observed response byte (HTTP response-header arrival), when the
	// adapter's transport observation proves it. Zero means not measured,
	// never a claim of instant arrival. First-TOKEN latency is never
	// claimed: the non-streaming lane cannot observe a model token
	// separately from the body, so any such number would be a guess (#39
	// maintainer review).
	FirstByteLatency time.Duration

	// RetryCount is the number of retries this attempt represents. The
	// protected lane has no retries outside the governor (#92): every
	// current adapter leaves it zero so amplification can never hide here.
	RetryCount int

	// Fallback reports whether this attempt used a fallback route. The
	// protected lane has no fallbacks: every current adapter leaves it
	// false.
	Fallback bool

	// UsageEstimated reports that any usage figures carried by this
	// transport are estimates rather than metered values. No current
	// adapter emits usage, so every current adapter leaves it false.
	UsageEstimated bool
}

// Client performs one logical completion. Legacy clients configured with
// SafeRouteSafety account for one attempt; receipt-aware clients may produce
// one or more authoritative attempts in ResponseMetadata.
type Client interface {
	Complete(context.Context, Request) (Response, error)
}
