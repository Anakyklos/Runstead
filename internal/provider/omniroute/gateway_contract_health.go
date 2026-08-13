package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

var gatewayContractEndpoints = [...]string{providersPath, settingsPath, modelAliasesPath}

const (
	gatewayContractReasonRecognized     = "recognized"
	gatewayContractReasonContext        = "context_cancelled"
	gatewayContractReasonContextMissing = "context_missing"
	gatewayContractReasonTimeout        = "timeout"
	gatewayContractReasonTransport      = "transport_uncertain"
	gatewayContractReasonHTTP404        = "http_404"
	gatewayContractReasonHTTP410        = "http_410"
	gatewayContractReasonTemporaryHTTP  = "temporary_http_status"
	gatewayContractReasonUnexpectedHTTP = "unexpected_http_status"
	gatewayContractReasonMalformedJSON  = "malformed_json"
	gatewayContractReasonMissingField   = "missing_or_invalid_field"
)

func (c *Client) GatewayContractHealth() provider.GatewayContractHealthResult {
	if c == nil {
		return provider.GatewayContractHealthResult{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gatewayContractHealth
}

// ProbeGatewayContract checks only the OmniRoute management gateway contract.
// It does not probe ChatGPT Web, Sentinel, or any upstream model endpoint.
func (c *Client) ProbeGatewayContract(ctx context.Context) provider.GatewayContractHealthResult {
	if c == nil {
		return provider.GatewayContractHealthResult{}
	}
	checkedAt := c.now()
	result := provider.GatewayContractHealthResult{
		State:     provider.GatewayContractHealthUnknown,
		CheckedAt: checkedAt,
	}
	if ctx == nil {
		result.ReasonCode = gatewayContractReasonContextMissing
		return c.recordGatewayContractHealth(result)
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			result.ReasonCode = gatewayContractReasonTimeout
		} else {
			result.ReasonCode = gatewayContractReasonContext
		}
		return c.recordGatewayContractHealth(result)
	}

	for _, endpoint := range gatewayContractEndpoints {
		body, metadata, err := c.getTelemetry(ctx, endpoint)
		if err != nil {
			return c.recordGatewayContractHealth(classifyGatewayContractHTTPOrTransport(endpoint, metadata, err, checkedAt))
		}
		if reasonCode := validateGatewayContractEndpoint(endpoint, body); reasonCode != "" {
			return c.recordGatewayContractHealth(healthResult(
				provider.GatewayContractHealthProtocolChanged,
				reasonCode,
				endpoint,
				checkedAt,
			))
		}
	}
	result.State = provider.GatewayContractHealthHealthy
	result.ReasonCode = gatewayContractReasonRecognized
	return c.recordGatewayContractHealth(result)
}

func (c *Client) recordGatewayContractHealth(result provider.GatewayContractHealthResult) provider.GatewayContractHealthResult {
	c.mu.Lock()
	c.gatewayContractHealth = result
	c.mu.Unlock()
	return result
}

func healthResult(state provider.GatewayContractHealth, reasonCode, endpoint string, checkedAt time.Time) provider.GatewayContractHealthResult {
	return provider.GatewayContractHealthResult{
		State:      state,
		ReasonCode: reasonCode,
		Endpoint:   endpoint,
		CheckedAt:  checkedAt,
	}
}

func classifyGatewayContractHTTPOrTransport(endpoint string, metadata provider.ResponseMetadata, err error, checkedAt time.Time) provider.GatewayContractHealthResult {
	if errors.Is(err, context.Canceled) {
		return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonContext, endpoint, checkedAt)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonTimeout, endpoint, checkedAt)
	}
	switch metadata.StatusCode {
	case http.StatusNotFound:
		return healthResult(provider.GatewayContractHealthProtocolChanged, gatewayContractReasonHTTP404, endpoint, checkedAt)
	case http.StatusGone:
		return healthResult(provider.GatewayContractHealthProtocolChanged, gatewayContractReasonHTTP410, endpoint, checkedAt)
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return healthResult(provider.GatewayContractHealthDegraded, gatewayContractReasonTemporaryHTTP, endpoint, checkedAt)
	case http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound,
		http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return healthResult(provider.GatewayContractHealthProtocolChanged, gatewayContractReasonUnexpectedHTTP, endpoint, checkedAt)
	default:
		return healthResult(provider.GatewayContractHealthUnknown, gatewayContractReasonTransport, endpoint, checkedAt)
	}
}

func validateGatewayContractEndpoint(endpoint string, body []byte) string {
	root, ok := jsonObject(body)
	if !ok {
		return gatewayContractReasonMalformedJSON
	}
	switch endpoint {
	case providersPath:
		return validateGatewayProviders(root)
	case settingsPath:
		return validateGatewaySettings(root)
	case modelAliasesPath:
		return validateGatewayModelAliases(root)
	default:
		return gatewayContractReasonMissingField
	}
}

// validateGatewayProviders checks only the structural shape of /api/providers
// that Runstead consumes. defaultModel values and active-connection selection
// are route-configuration concerns owned by RouteSafety/preflight, so they are
// deliberately ignored here: a nullable or differing defaultModel is not
// gateway-contract drift.
func validateGatewayProviders(root map[string]json.RawMessage) string {
	connectionsRaw, exists := root["connections"]
	if !exists {
		return gatewayContractReasonMissingField
	}
	var connections []json.RawMessage
	if json.Unmarshal(connectionsRaw, &connections) != nil || connections == nil {
		return gatewayContractReasonMissingField
	}
	total, ok := intField(root, "total")
	if !ok || total != len(connections) {
		return gatewayContractReasonMissingField
	}
	for _, raw := range connections {
		connection, ok := jsonObject(raw)
		if !ok {
			return gatewayContractReasonMissingField
		}
		providerName, providerOK := stringField(connection, "provider")
		_, activeOK := boolField(connection, "isActive")
		if !providerOK || !activeOK || providerName == "" {
			return gatewayContractReasonMissingField
		}
	}
	return ""
}

func validateGatewaySettings(root map[string]json.RawMessage) string {
	wildcards, exists := root["wildcardAliases"]
	// OmniRoute defines wildcardAliases as an array of {pattern,target}
	// objects. A non-empty array is schema-valid; whether aliases make the
	// protected route unsafe is a RouteSafety decision, not contract drift.
	if !exists || !jsonArray(wildcards) {
		return gatewayContractReasonMissingField
	}
	modelAliases, ok := jsonObjectField(root, "modelAliases")
	if !ok || modelAliases == nil {
		return gatewayContractReasonMissingField
	}
	if _, ok := stringField(root, "globalFallbackModel"); !ok {
		return gatewayContractReasonMissingField
	}
	return ""
}

func validateGatewayModelAliases(root map[string]json.RawMessage) string {
	aliases, ok := jsonObjectField(root, "aliases")
	if !ok || aliases == nil {
		return gatewayContractReasonMissingField
	}
	return ""
}

func jsonArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil && values != nil
}

func stringField(root map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := root[key]
	if !ok {
		return "", false
	}
	var value *string
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return "", false
	}
	return strings.TrimSpace(*value), true
}

func boolField(root map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := root[key]
	if !ok {
		return false, false
	}
	var value *bool
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, false
	}
	return *value, true
}

func intField(root map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := root[key]
	if !ok {
		return 0, false
	}
	var value *int
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return 0, false
	}
	return *value, true
}
