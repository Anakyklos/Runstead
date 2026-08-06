package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

var ErrUnsafeRoute = errors.New("provider route cannot guarantee authoritative upstream attempt accounting")

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
}

func SafeRouteSafety() RouteSafety {
	return RouteSafety{
		AttemptAccounting: AttemptAccountingSingle,
		SingleAttempt:     SingleAttemptGuaranteed,
		InternalRetries:   AmplificationDisabled,
		CooldownReplay:    AmplificationDisabled,
		AccountPooling:    AmplificationDisabled,
		AutomaticFallback: AmplificationDisabled,
	}
}

func ReceiptRouteSafety() RouteSafety {
	return RouteSafety{
		AttemptAccounting: AttemptAccountingReceipts,
		InternalRetries:   AmplificationEnabled,
		CooldownReplay:    AmplificationEnabled,
		AccountPooling:    AmplificationEnabled,
		AutomaticFallback: AmplificationEnabled,
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
		} {
			if value == AmplificationUnknown {
				return fmt.Errorf("%w: %s coverage is unknown", ErrUnsafeRoute, name)
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
	AttemptReceipts *AttemptReceiptSet
}

// Client performs one logical completion. Legacy clients configured with
// SafeRouteSafety account for one attempt; receipt-aware clients may produce
// one or more authoritative attempts in ResponseMetadata.
type Client interface {
	Complete(context.Context, Request) (Response, error)
}
