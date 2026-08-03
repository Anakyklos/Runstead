package agent

import (
	"context"
	"errors"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

var ErrExecutorUnavailable = errors.New("agent executor is unavailable")

// Executor is the only agent-facing provider execution seam. The provider is
// retained privately and every attempt is routed through the governor. It
// owns one explicit event-drain step after each execution; no background
// dispatcher is required to deliver the governor's mandatory events.
type Executor struct {
	governor   *governor.Governor
	provider   provider.Client
	classifier governor.OutcomeClassifier
}

func NewExecutor(accountGovernor *governor.Governor, client provider.Client, classifier governor.OutcomeClassifier) (*Executor, error) {
	if accountGovernor == nil || client == nil {
		return nil, ErrExecutorUnavailable
	}
	return &Executor{governor: accountGovernor, provider: client, classifier: classifier}, nil
}

func (e *Executor) Execute(ctx context.Context, request governor.AttemptRequest) governor.ExecutionResult {
	if e == nil || e.governor == nil || e.provider == nil {
		return governor.ExecutionResult{Err: ErrExecutorUnavailable}
	}
	result := e.governor.Execute(ctx, request, e.provider, e.classifier)
	e.governor.DrainEvents()
	return result
}
