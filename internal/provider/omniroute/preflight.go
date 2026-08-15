package omniroute

import (
	"context"
	"errors"
)

// Preflight validates observable management settings for diagnostics, but it
// never authorizes protected execution: the finalized attempt receipt remains
// the sole authority over the physical model send.
//
// The semantics are split by attempt-accounting mode:
//
//   - legacySingleAttemptPreflight preserves the historical single-attempt
//     behavior byte-for-byte: it requires globally empty fallback chains,
//     combos, model-combo mappings, neutralized aliases/fallback and exactly
//     one active connection for the provider, and it keeps the historical
//     fail-closed "attempt receipts unavailable" block.
//
//   - receiptAwarePreflight applies only to EnableAttemptReceipts lanes. The
//     strict receipt-v1 producer is secure per request (exact connection pin,
//     exact provider/model, dedicated route, combo/reroute/retry/fallback
//     suppressed inside the request), so unrelated global OmniRoute
//     configuration for non-Runstead traffic never blocks a pinned protected
//     request. The preflight only confirms the gateway is a compatible build:
//     a healthy ProbeGatewayContract result (schema recognized on the
//     mandatory management endpoints) and an explicit <provider>/<model>.
func (c *Client) Preflight(ctx context.Context) error {
	if c == nil {
		return unsafeError(nil)
	}
	c.mu.Lock()
	c.verified = false
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contextError(err, false)
	}
	if c.config.EnableAttemptReceipts {
		return c.receiptAwarePreflight(ctx)
	}
	return c.legacySingleAttemptPreflight(ctx)
}

// legacySingleAttemptPreflight is the historical preflight for non-receipt /
// single-attempt lanes. It is preserved unchanged: mutable/global management
// evidence is the corroboration surface of the legacy route declaration, and
// the lane stays fail-closed until authoritative attempt receipts exist.
func (c *Client) legacySingleAttemptPreflight(ctx context.Context) error {
	evidence := make(map[string][]byte, 1+7)
	for _, endpoint := range []string{resiliencePath, settingsPath, modelAliasesPath, settingsModelAliasesPath, fallbackChainsPath, combosPath, modelComboMappingsPath, providersPath} {
		body, err := c.managementEvidence(ctx, endpoint)
		if err != nil {
			return err
		}
		evidence[endpoint] = body
	}
	if !safeResilience(evidence[resiliencePath]) {
		return unsafeError(errors.New("OmniRoute resilience evidence is missing or unsafe"))
	}
	if !safeRouteEvidence(c.config.Model, evidence) {
		return unsafeError(errors.New("OmniRoute route evidence is missing or unsafe"))
	}
	return unsafeError(errAttemptReceiptsUnavailable)
}

// receiptAwarePreflight is the protected-lane preflight. It fails closed on
// the receipt lane prerequisites that the per-request strict producer cannot
// observe by itself: a healthy gateway contract with a recognized management
// schema (populated by ProbeGatewayContract) and an explicit
// <provider>/<model> matching the configured provider identity.
//
// It deliberately does NOT require globally empty fallback chains, combos,
// model-combo mappings, neutralized aliases or exactly one active connection:
// those are legacy single-attempt global guarantees, while receipt-v1
// neutrality is enforced per request by the pinned producer.
func (c *Client) receiptAwarePreflight(ctx context.Context) error {
	// The gateway contract health must have been populated by an explicit
	// ProbeGatewayContract call; its zero value is conservatively unknown.
	health := c.GatewayContractHealth()
	if !health.Healthy() {
		return gatewayContractUnhealthyError()
	}
	providerName, _, ok := explicitRouteModel(c.config.Model)
	if !ok || providerName != c.config.Provider {
		return unsafeError(errors.New("the receipt lane requires an explicit <provider>/<model> matching the configured provider"))
	}
	if c.config.Provider == ProviderChatGPTWeb {
		if c.config.ConnectionID == "" {
			return unsafeError(errors.New("the pinned chatgpt-web receipt lane requires a connection id"))
		}
		if c.config.ChatEndpoint != DedicatedChatEndpoint {
			return unsafeError(errors.New("the pinned chatgpt-web receipt lane requires the dedicated providers/chatgpt-web/chat/completions endpoint"))
		}
	}
	return nil
}
