package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

var ErrUnsafeRoute = errors.New("provider route cannot guarantee one upstream attempt")

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

// RouteSafety is an executable declaration of the provider behavior required
// by the protected account lane. Unknown values fail closed.
type RouteSafety struct {
	SingleAttempt     SingleAttemptGuarantee
	InternalRetries   AmplificationStatus
	CooldownReplay    AmplificationStatus
	AccountPooling    AmplificationStatus
	AutomaticFallback AmplificationStatus
}

func SafeRouteSafety() RouteSafety {
	return RouteSafety{
		SingleAttempt:     SingleAttemptGuaranteed,
		InternalRetries:   AmplificationDisabled,
		CooldownReplay:    AmplificationDisabled,
		AccountPooling:    AmplificationDisabled,
		AutomaticFallback: AmplificationDisabled,
	}
}

func (s RouteSafety) Validate() error {
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

type Request struct {
	Protocol protocol.Version
	Prompt   string
}

type Response struct {
	Text string
}

// Client performs at most one upstream model attempt per Complete invocation.
// Implementations must not retry, rotate accounts, select fallbacks, schedule
// work or apply quota policy; those decisions belong above this boundary.
type Client interface {
	Complete(context.Context, Request) (Response, error)
}
