package omniroute

import (
	"encoding/json"
	"strings"
)

func safeResilience(body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}

	queue, ok := jsonObjectField(root, "requestQueue")
	if !ok || !intEquals(queue, "concurrentRequests", 1) || !intEquals(queue, "minTimeBetweenRequestsMs", 0) || !intAtLeast(queue, "maxWaitMs", 0) {
		return false
	}

	cooldown, ok := jsonObjectField(root, "connectionCooldown")
	if !ok || !safeConnectionCooldown(cooldown, "oauth") || !safeConnectionCooldown(cooldown, "apikey") {
		return false
	}

	wait, ok := jsonObjectField(root, "waitForCooldown")
	if !ok || !boolEquals(wait, "enabled", false) || !intEquals(wait, "maxRetries", 0) || !intEquals(wait, "maxRetryWaitSec", 0) {
		return false
	}

	combo, ok := jsonObjectField(root, "comboCooldownWait")
	if !ok || !boolEquals(combo, "enabled", false) || !intEquals(combo, "maxAttempts", 0) || !intEquals(combo, "maxWaitMs", 0) {
		return false
	}

	quotaShare, ok := jsonObjectField(root, "quotaShareConcurrencyLimit")
	if !ok || !boolEquals(quotaShare, "enabled", false) {
		return false
	}

	providerCooldown, ok := jsonObjectField(root, "providerCooldown")
	if !ok || !boolEquals(providerCooldown, "enabled", false) {
		return false
	}

	legacy, ok := jsonObjectField(root, "legacy")
	if !ok || !intEquals(legacy, "requestRetry", 0) || !intEquals(legacy, "maxRetryIntervalSec", 0) {
		return false
	}
	if !safeSingleAttemptContract(root) {
		return false
	}
	return true
}

func safeSingleAttemptContract(root map[string]json.RawMessage) bool {
	contract, ok := jsonObjectField(root, "singleAttemptContract")
	return ok &&
		intEquals(contract, "version", 1) &&
		boolEquals(contract, "guaranteed", true) &&
		boolEquals(contract, "internalRetries", false) &&
		boolEquals(contract, "credentialRefreshRetry", false) &&
		boolEquals(contract, "cooldownReplay", false) &&
		boolEquals(contract, "accountPooling", false) &&
		boolEquals(contract, "automaticFallback", false)
}

func safeConnectionCooldown(root map[string]json.RawMessage, category string) bool {
	profile, ok := jsonObjectField(root, category)
	if !ok || !boolEquals(profile, "useUpstreamRetryHints", false) || !intEquals(profile, "maxBackoffSteps", 0) {
		return false
	}
	if raw, exists := profile["useUpstream429BreakerHints"]; exists {
		var value *bool
		if json.Unmarshal(raw, &value) != nil || value == nil || *value {
			return false
		}
	}
	return true
}

func safeRouteEvidence(model string, evidence map[string][]byte) bool {
	providerName, _, ok := explicitRouteModel(model)
	if !ok || !safeSettingsEvidence(model, evidence[settingsPath]) || !safeAliasEvidence(model, evidence[modelAliasesPath]) || !safeSettingsAliasEvidence(model, evidence[settingsModelAliasesPath]) || !safeFallbackEvidence(evidence[fallbackChainsPath]) || !safeCombosEvidence(evidence[combosPath]) || !safeModelComboMappingsEvidence(evidence[modelComboMappingsPath]) || !safeProvidersEvidence(providerName, evidence[providersPath]) {
		return false
	}
	return true
}

func explicitRouteModel(model string) (providerName, modelName string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	providerName = strings.TrimSpace(parts[0])
	modelName = strings.TrimSpace(parts[1])
	if providerName == "" || modelName == "" || strings.EqualFold(providerName, "auto") {
		return "", "", false
	}
	return providerName, modelName, true
}

func safeSettingsEvidence(model string, body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	wildcardAliases, exists := root["wildcardAliases"]
	if !exists {
		return false
	}
	var aliases []json.RawMessage
	if json.Unmarshal(wildcardAliases, &aliases) != nil || aliases == nil || len(aliases) != 0 {
		return false
	}
	modelAliases, exists := root["modelAliases"]
	if !exists {
		return false
	}
	aliasObject, ok := jsonObject(modelAliases)
	if !ok || hasModelAlias(aliasObject, model) {
		return false
	}
	fallbackValue, exists := root["globalFallbackModel"]
	if !exists {
		return false
	}
	var fallback *string
	if json.Unmarshal(fallbackValue, &fallback) != nil || fallback == nil || strings.TrimSpace(*fallback) != "" {
		return false
	}
	return true
}

func safeAliasEvidence(model string, body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	aliases, ok := jsonObjectField(root, "aliases")
	return ok && !hasModelAlias(aliases, model)
}

func safeSettingsAliasEvidence(model string, body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	aliases, ok := jsonObjectField(root, "all")
	return ok && !hasModelAlias(aliases, model)
}

func safeFallbackEvidence(body []byte) bool {
	var chains []json.RawMessage
	return json.Unmarshal(body, &chains) == nil && chains != nil && len(chains) == 0
}

func safeCombosEvidence(body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	raw, exists := root["combos"]
	if !exists {
		return false
	}
	var combos []json.RawMessage
	return json.Unmarshal(raw, &combos) == nil && combos != nil && len(combos) == 0
}

func safeModelComboMappingsEvidence(body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	raw, exists := root["mappings"]
	if !exists {
		return false
	}
	var mappings []json.RawMessage
	return json.Unmarshal(raw, &mappings) == nil && mappings != nil && len(mappings) == 0
}

func safeProvidersEvidence(providerName string, body []byte) bool {
	root, ok := jsonObject(body)
	if !ok {
		return false
	}
	var connections []map[string]json.RawMessage
	raw, exists := root["connections"]
	if !exists || json.Unmarshal(raw, &connections) != nil || connections == nil {
		return false
	}
	activeMatches := 0
	for _, connection := range connections {
		var provider string
		var active bool
		if json.Unmarshal(connection["provider"], &provider) != nil || json.Unmarshal(connection["isActive"], &active) != nil {
			return false
		}
		if active && strings.EqualFold(strings.TrimSpace(provider), providerName) {
			activeMatches++
		}
	}
	return activeMatches == 1
}

func hasModelAlias(aliases map[string]json.RawMessage, model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	for alias := range aliases {
		if strings.ToLower(strings.TrimSpace(alias)) == want {
			return true
		}
	}
	return false
}

func jsonObject(body []byte) (map[string]json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, false
	}
	return root, true
}

func jsonObjectField(root map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	value, exists := root[key]
	if !exists {
		return nil, false
	}
	return jsonObject(value)
}

func boolEquals(root map[string]json.RawMessage, key string, want bool) bool {
	value, exists := root[key]
	if !exists {
		return false
	}
	var got *bool
	return json.Unmarshal(value, &got) == nil && got != nil && *got == want
}

func intEquals(root map[string]json.RawMessage, key string, want int) bool {
	value, exists := root[key]
	if !exists {
		return false
	}
	var got *int
	return json.Unmarshal(value, &got) == nil && got != nil && *got == want
}

func intAtLeast(root map[string]json.RawMessage, key string, minimum int) bool {
	value, exists := root[key]
	if !exists {
		return false
	}
	var got *int
	return json.Unmarshal(value, &got) == nil && got != nil && *got >= minimum
}
