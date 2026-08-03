package omniroute

import (
	"encoding/json"
	"strings"
)

type resilienceEvidence struct {
	RequestQueue *struct {
		ConcurrentRequests       *int `json:"concurrentRequests"`
		MinTimeBetweenRequestsMs *int `json:"minTimeBetweenRequestsMs"`
		MaxWaitMs                *int `json:"maxWaitMs"`
	} `json:"requestQueue"`
	ConnectionCooldown *struct {
		UseUpstreamRetryHints *bool `json:"useUpstreamRetryHints"`
		MaxBackoffSteps       *int  `json:"maxBackoffSteps"`
	} `json:"connectionCooldown"`
	WaitForCooldown *struct {
		Enabled         *bool `json:"enabled"`
		MaxRetries      *int  `json:"maxRetries"`
		MaxRetryWaitSec *int  `json:"maxRetryWaitSec"`
	} `json:"waitForCooldown"`
	ComboCooldownWait *struct {
		Enabled     *bool `json:"enabled"`
		MaxAttempts *int  `json:"maxAttempts"`
		MaxWaitMs   *int  `json:"maxWaitMs"`
	} `json:"comboCooldownWait"`
	QuotaShareConcurrencyLimit *struct {
		Enabled *bool `json:"enabled"`
	} `json:"quotaShareConcurrencyLimit"`
	ProviderCooldown *struct {
		Enabled *bool `json:"enabled"`
	} `json:"providerCooldown"`
	StreamRecovery *struct {
		Enabled          *bool `json:"enabled"`
		MidstreamEnabled *bool `json:"midstreamEnabled"`
	} `json:"streamRecovery"`
}

func safeResilience(body []byte) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil || !onlyKeys(raw, "requestQueue", "connectionCooldown", "waitForCooldown", "comboCooldownWait", "quotaShareConcurrencyLimit", "providerCooldown", "streamRecovery") {
		return false
	}
	var evidence resilienceEvidence
	if json.Unmarshal(body, &evidence) != nil {
		return false
	}
	if evidence.RequestQueue == nil || evidence.RequestQueue.ConcurrentRequests == nil || *evidence.RequestQueue.ConcurrentRequests != 1 || evidence.RequestQueue.MinTimeBetweenRequestsMs == nil || *evidence.RequestQueue.MinTimeBetweenRequestsMs != 0 || evidence.RequestQueue.MaxWaitMs == nil || *evidence.RequestQueue.MaxWaitMs != 0 {
		return false
	}
	if evidence.ConnectionCooldown == nil || evidence.ConnectionCooldown.UseUpstreamRetryHints == nil || *evidence.ConnectionCooldown.UseUpstreamRetryHints || evidence.ConnectionCooldown.MaxBackoffSteps == nil || *evidence.ConnectionCooldown.MaxBackoffSteps != 0 {
		return false
	}
	if evidence.WaitForCooldown == nil || evidence.WaitForCooldown.Enabled == nil || *evidence.WaitForCooldown.Enabled || evidence.WaitForCooldown.MaxRetries == nil || *evidence.WaitForCooldown.MaxRetries != 0 || evidence.WaitForCooldown.MaxRetryWaitSec == nil || *evidence.WaitForCooldown.MaxRetryWaitSec != 0 {
		return false
	}
	if evidence.ComboCooldownWait == nil || evidence.ComboCooldownWait.Enabled == nil || *evidence.ComboCooldownWait.Enabled || evidence.ComboCooldownWait.MaxAttempts == nil || *evidence.ComboCooldownWait.MaxAttempts != 0 || evidence.ComboCooldownWait.MaxWaitMs == nil || *evidence.ComboCooldownWait.MaxWaitMs != 0 {
		return false
	}
	if evidence.QuotaShareConcurrencyLimit == nil || evidence.QuotaShareConcurrencyLimit.Enabled == nil || *evidence.QuotaShareConcurrencyLimit.Enabled {
		return false
	}
	if evidence.ProviderCooldown == nil || evidence.ProviderCooldown.Enabled == nil || *evidence.ProviderCooldown.Enabled {
		return false
	}
	if evidence.StreamRecovery == nil || evidence.StreamRecovery.Enabled == nil || *evidence.StreamRecovery.Enabled || evidence.StreamRecovery.MidstreamEnabled == nil || *evidence.StreamRecovery.MidstreamEnabled {
		return false
	}
	return true
}

func onlyKeys(values map[string]json.RawMessage, allowed ...string) bool {
	for key := range values {
		known := false
		for _, candidate := range allowed {
			if key == candidate {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

func routeModelSafe(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || model == "auto" {
		return false
	}
	for _, marker := range []string{"combo", "priority", "weighted", "fallback"} {
		if strings.Contains(model, marker) {
			return false
		}
	}
	return true
}
