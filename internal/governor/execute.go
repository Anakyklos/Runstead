package governor

import (
	"context"
	"errors"
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
	receiptAware := false
	if g.config.RequireAttemptReceipts {
		capability, ok := client.(provider.AttemptReceiptAware)
		if !ok || !capability.AttemptReceiptsEnabled() {
			return ExecutionResult{Admission: g.result(AdmissionMissingAttemptReceipts, AdmissionMissingAttemptReceipts, time.Time{}, provider.ErrInvalidAttemptReceipts)}
		}
		receiptAware = true
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
	response, callErr := client.Complete(ctx, request.ProviderRequest)
	if classifier == nil {
		classifier = defaultOutcome
	}
	outcome := classifier(response, callErr)
	if outcome.Class == OutcomeCancelledBeforeUpstream {
		outcome.Class = OutcomeUncertainReached
	}
	var completion FinishResult
	if receiptAware {
		completion = admission.Permit.FinishWithAttemptReceipts(outcome, response.Metadata.AttemptReceipts)
		if completion.Err != nil && callErr == nil {
			callErr = completion.Err
		}
	} else {
		completion = admission.Permit.Finish(outcome)
	}
	return ExecutionResult{Admission: admission, Response: response, Completion: completion, Err: callErr}
}
