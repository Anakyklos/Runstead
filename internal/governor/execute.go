package governor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func (g *Governor) Execute(ctx context.Context, request AttemptRequest, client provider.Client, classifier OutcomeClassifier) ExecutionResult {
	if client == nil {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("provider client is required"))}
	}
	aware, ok := client.(provider.SafetyAware)
	if !ok {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeProviderAmplification, AdmissionUnsafeProviderAmplification, time.Time{}, provider.ErrUnsafeRoute)}
	}
	safety := aware.RouteSafety()
	if err := safety.Validate(); err != nil || !safety.Equal(g.config.RouteSafety) {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeProviderAmplification, AdmissionUnsafeProviderAmplification, time.Time{}, provider.ErrUnsafeRoute)}
	}
	if healthAware, ok := client.(provider.ContractHealthAware); ok {
		health := healthAware.GatewayContractHealth()
		if !health.Healthy() {
			admission := g.gatewayContractHealthAdmission(request, health)
			return ExecutionResult{Admission: admission, Err: admission.Err}
		}
	}
	receiptAware := false
	if g.config.RequireAttemptReceipts {
		capability, ok := client.(provider.AttemptReceiptAware)
		if !ok || !capability.AttemptReceiptsEnabled() {
			return ExecutionResult{Admission: g.result(AdmissionMissingAttemptReceipts, AdmissionMissingAttemptReceipts, time.Time{}, provider.ErrInvalidAttemptReceipts)}
		}
		receiptAware = true
	}
	if model := strings.TrimSpace(request.ProviderRequest.Model); model == "" {
		request.ProviderRequest.Model = g.config.Model
	} else if g.config.Model != "" && model != g.config.Model {
		return ExecutionResult{Admission: g.result(AdmissionUnsafeConfiguration, AdmissionUnsafeConfiguration, time.Time{}, errors.New("request model differs from account policy"))}
	}
	admission := g.Admit(ctx, request)
	if !admission.Admitted() {
		return ExecutionResult{Admission: admission, Err: admission.Err}
	}
	if err := ctx.Err(); err != nil {
		admission.Permit.CancelBeforeStart()
		admission.Code = contextAdmissionCode(ctx)
		admission.Err = &AdmissionError{Code: admission.Code, Cause: err}
		return ExecutionResult{Admission: admission, Err: admission.Err}
	}
	request.ProviderRequest.ClientRequestID = request.ClientRequestID
	var startErr error
	if receiptAware {
		startErr = admission.Permit.StartReceiptAware()
	} else {
		startErr = admission.Permit.Start()
	}
	if err := startErr; err != nil {
		return ExecutionResult{Admission: admission, Err: err}
	}

	// TX 1: persist the provider attempt intent and the post-start governor
	// protection state BEFORE the provider call. A crash after this commit
	// leaves durable evidence that the upstream may have been reached. If the
	// durable intent cannot be committed, the provider call must not proceed:
	// the effect is not executed, the permit is finished conservatively and
	// the run stops fail-closed.
	if g.persistence != nil {
		prepared := ProviderPrepared{
			TaskID:           request.TaskID,
			ClientRequestID:  request.ClientRequestID,
			ProviderID:       g.config.ProviderID,
			ModelPool:        g.config.ModelPool,
			Model:            g.config.Model,
			ProtocolFamily:   g.config.ProtocolFamily,
			ConfigIdentity:   g.config.ConfigIdentity,
			AllowanceProfile: g.config.AllowanceProfile,
			AttemptSequence:  admission.Permit.AttemptSequence(),
			StartedAt:        admission.Permit.StartedAt(),
			ReceiptAware:     receiptAware,
			TelemetryHealthy: admission.TelemetryHealthy,
			State:            g.PersistedState(),
		}
		if err := g.persistence.RecordProviderPrepared(ctx, prepared); err != nil {
			// The durable intent could not be committed, so the provider call
			// must not proceed. Abort the started permit with no additional
			// debit and a fully released lane. This path is valid for
			// receipt-aware permits too: Finish would refuse them, and an
			// upstream call that never happened must not leave the lane stuck.
			completion := admission.Permit.CancelAfterStart()
			return ExecutionResult{
				Admission:  admission,
				Completion: completion,
				Err:        fmt.Errorf("durable provider intent could not be persisted: %w", err),
			}
		}
	}

	response, callErr := client.Complete(ctx, request.ProviderRequest)
	if classifier == nil {
		classifier = defaultOutcome
	}
	outcome := classifier(response, callErr)
	outcome.DeliveryState = response.Metadata.DeliveryState
	outcome = applyDeliveryEvidence(outcome)
	var completion FinishResult
	if receiptAware {
		completion = admission.Permit.FinishWithAttemptReceipts(outcome, response.Metadata.AttemptReceipts)
		if completion.Err != nil && callErr == nil {
			callErr = completion.Err
		}
	} else {
		completion = admission.Permit.Finish(outcome)
	}

	// TX 2: persist the classified outcome, receipt evidence and the
	// post-finish governor protection state. A crash before this commit
	// leaves the attempt 'prepared' in the store: ambiguous, never a silent
	// re-execution.
	if g.persistence != nil {
		finished := ProviderFinished{
			TaskID:           request.TaskID,
			ClientRequestID:  request.ClientRequestID,
			Outcome:          completion.Outcome,
			ProtocolFamily:   g.config.ProtocolFamily,
			ConfigIdentity:   g.config.ConfigIdentity,
			RequestID:        response.Metadata.RequestID,
			UpstreamReached:  outcome.UpstreamReached,
			Uncertain:        completion.Outcome == OutcomeUncertainReached,
			DeliveryState:    response.Metadata.DeliveryState,
			AttemptDebited:   completion.AttemptDebited,
			SelectedBackoff:  completion.SelectedBackoff,
			Circuit:          completion.Circuit,
			RetryEligible:    completion.RetryEligible,
			Receipts:         receiptEvidence(response.Metadata.AttemptReceipts),
			ReceiptErrorCode: receiptErrorCode(completion.Err),
			State:            g.PersistedState(),
		}
		if err := g.persistence.RecordProviderFinished(ctx, finished); err != nil {
			return ExecutionResult{
				Admission:  admission,
				Response:   response,
				Completion: completion,
				Err:        fmt.Errorf("%w: %v", ErrProviderOutcomePersist, err),
			}
		}
	}
	return ExecutionResult{Admission: admission, Response: response, Completion: completion, Err: callErr}
}

func (g *Governor) gatewayContractHealthAdmission(request AttemptRequest, health provider.GatewayContractHealthResult) AdmissionResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := g.resultLocked(AdmissionGatewayContractUnhealthy, AdmissionGatewayContractUnhealthy, time.Time{}, provider.ErrGatewayContractUnhealthy)
	result.GatewayContractHealth = &health
	g.emitAdmissionLocked(request, result, false)
	return result
}

// receiptEvidence flattens the sanitized receipt set for persistence. The
// receipt attempt IDs are upstream-owned evidence identities, never Runstead
// execution identities.
func receiptEvidence(set *provider.AttemptReceiptSet) []provider.AttemptReceipt {
	if set == nil {
		return nil
	}
	return append([]provider.AttemptReceipt(nil), set.Receipts...)
}

// receiptErrorCode extracts the structural receipt validation code, if any,
// from a completion error. Only the code is persisted, never raw error text.
func receiptErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var receiptErr *provider.AttemptReceiptError
	if errors.As(err, &receiptErr) {
		return string(receiptErr.Code)
	}
	if errors.Is(err, provider.ErrInvalidAttemptReceipts) {
		return string(provider.AttemptReceiptMalformed)
	}
	return ""
}
