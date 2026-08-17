package provider

import (
	"context"
	"time"
)

// AdapterConfig configures how a minimal ProviderClient is adapted to the legacy
// LegacyClient interface expected by the governor.
type AdapterConfig struct {
	// RouteSafety declares the provider's safety properties for the governor.
	// For ChatGPT Web direct: single attempt, no amplification.
	RouteSafety RouteSafety

	// ProviderID identifies this provider for accounting (e.g., "chatgptweb").
	ProviderID string

	// ModelPool is the allowance bucket name (e.g., "luna_unlimited_text").
	ModelPool string

	// RequireAttemptReceipts if true, the adapter will require the underlying
	// client to support receipt-aware completion (not yet for ChatGPT Web).
	RequireAttemptReceipts bool
}

// LegacyClientAdapter wraps a minimal ProviderClient and implements the legacy
// LegacyClient interface (with SafetyAware, ContractHealthAware, AttemptReceiptAware)
// so it can be used with the governor executor.
type LegacyClientAdapter struct {
	client ProviderClient
	config AdapterConfig
}

var _ LegacyClient = (*LegacyClientAdapter)(nil)
var _ SafetyAware = (*LegacyClientAdapter)(nil)
var _ ContractHealthAware = (*LegacyClientAdapter)(nil)
var _ AttemptReceiptAware = (*LegacyClientAdapter)(nil)

// NewLegacyClientAdapter creates an adapter from a minimal ProviderClient.
func NewLegacyClientAdapter(client ProviderClient, config AdapterConfig) *LegacyClientAdapter {
	return &LegacyClientAdapter{client: client, config: config}
}

// Complete implements the legacy LegacyClient interface.
func (a *LegacyClientAdapter) Complete(ctx context.Context, req Request) (Response, error) {
	// Convert legacy request to new request
	newReq := ProviderRequest{
		ClientRequestID: req.ClientRequestID,
		Model:           req.Model,
		Messages: []Message{
			{Role: "system", Content: req.Prompt}, // legacy uses Prompt as system
		},
		Stream: false, // legacy doesn't stream
	}

	// If Model is empty, use a default (will be validated by client)
	if newReq.Model == "" {
		newReq.Model = "default"
	}

	resp, err := a.client.Complete(ctx, newReq)
	if err != nil {
		return Response{}, err
	}

	// Convert new response to legacy response
	return Response{
		Text: resp.Content,
		Metadata: ResponseMetadata{
			StatusCode:      resp.Metadata.StatusCode,
			RequestID:       resp.Metadata.RequestID,
			SessionID:       resp.Metadata.SessionID,
			Duration:        resp.Metadata.Duration,
			RetryAfter:      resp.Metadata.RetryAfter,
			ResetAt:         resp.Metadata.ResetAt,
			Endpoint:        resp.Metadata.Endpoint,
			Model:           resp.Metadata.Model,
			DeliveryState:   DeliveryCompleted, // default for non-streaming
			AttemptReceipts: nil,               // ChatGPT Web direct doesn't have receipts yet
		},
	}, nil
}

// RouteSafety implements SafetyAware.
func (a *LegacyClientAdapter) RouteSafety() RouteSafety {
	return a.config.RouteSafety
}

// GatewayContractHealth implements ContractHealthAware.
// For direct providers, we delegate to the client's HealthCheck.
func (a *LegacyClientAdapter) GatewayContractHealth() GatewayContractHealthResult {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	health, err := a.client.HealthCheck(ctx)
	if err != nil {
		return GatewayContractHealthResult{
			State:      GatewayContractHealthUnknown,
			ReasonCode: "health_check_failed",
			Endpoint:   a.client.Name(),
			CheckedAt:  time.Now(),
		}
	}

	var state GatewayContractHealth
	switch {
	case health.Healthy:
		state = GatewayContractHealthHealthy
	case health.Reason == "drift_detected":
		state = GatewayContractHealthProtocolChanged
	case health.Reason == "rate_limited":
		state = GatewayContractHealthDegraded
	default:
		state = GatewayContractHealthUnknown
	}

	return GatewayContractHealthResult{
		State:      state,
		ReasonCode: health.Reason,
		Endpoint:   a.client.Name(),
		CheckedAt:  time.Now(),
	}
}

// AttemptReceiptsEnabled implements AttemptReceiptAware.
func (a *LegacyClientAdapter) AttemptReceiptsEnabled() bool {
	return a.config.RequireAttemptReceipts
}