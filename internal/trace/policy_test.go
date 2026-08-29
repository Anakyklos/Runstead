package trace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestPolicySinkEmitsSanitizedStructuredEvent(t *testing.T) {
	var output bytes.Buffer
	sink := NewPolicySink(NewLogger(&output, 0))
	remaining := 7
	sink.Emit(governor.Event{
		Kind:             governor.EventAttemptFinished,
		AccountPolicyID:  "policy-account-1",
		ProviderID:       "omniroute",
		ModelPool:        "instant",
		AllowanceProfile: governor.AllowanceProfileInstant,
		TaskID:           "task-1",
		ClientRequestID:  "request-1",
		AttemptSequence:  1,
		Outcome:          governor.OutcomeSuccess,
		Telemetry: governor.TelemetrySummary{
			Available:         true,
			Remaining:         &remaining,
			ResetAt:           time.Date(2026, time.January, 1, 13, 0, 0, 0, time.UTC),
			CooldownUntil:     time.Date(2026, time.January, 1, 12, 5, 0, 0, time.UTC),
			RateLimited:       true,
			CapacityExhausted: true,
			UpstreamCircuit:   governor.UpstreamCircuitOpen,
		},
	})
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("policy log is not JSON: %v; output=%q", err, output.String())
	}
	if record["msg"] != "account policy event" || record["task_id"] != "task-1" || record["outcome"] != string(governor.OutcomeSuccess) {
		t.Fatalf("record = %#v", record)
	}
	telemetry, ok := record["telemetry"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry group = %#v", record["telemetry"])
	}
	if telemetry["remaining"] != float64(7) || telemetry["reset_at"] == nil || telemetry["cooldown_until"] == nil || telemetry["rate_limited"] != true || telemetry["capacity_exhausted"] != true || telemetry["upstream_circuit"] != string(governor.UpstreamCircuitOpen) {
		t.Fatalf("telemetry group = %#v", telemetry)
	}
	for _, forbidden := range []string{"prompt", "response", "token", "cookie", "credential", "api_key", "body"} {
		// Scan string VALUES only: structural keys (for example
		// first_token_latency) may contain secret-looking words without
		// carrying secret material (#39).
		for _, value := range jsonStringValues(t, record) {
			if strings.Contains(strings.ToLower(value), forbidden) {
				t.Fatalf("policy event contains forbidden value %q: %s", forbidden, output.String())
			}
		}
	}
}

// jsonStringValues returns every string value reachable in a decoded JSON
// record. Structural keys are excluded so field names such as
// first_token_latency never count as leaked material (#39).
func jsonStringValues(t *testing.T, node any) []string {
	t.Helper()
	var values []string
	var collect func(any)
	collect = func(current any) {
		switch value := current.(type) {
		case string:
			values = append(values, value)
		case []any:
			for _, item := range value {
				collect(item)
			}
		case map[string]any:
			for _, item := range value {
				collect(item)
			}
		}
	}
	collect(node)
	return values
}

func TestPolicySinkEmitsGatewayContractHealthWithSanitizedName(t *testing.T) {
	var output bytes.Buffer
	sink := NewPolicySink(NewLogger(&output, 0))
	health := provider.GatewayContractHealthResult{
		State:      provider.GatewayContractHealthProtocolChanged,
		ReasonCode: "missing_or_invalid_field",
		Endpoint:   "/api/settings",
		CheckedAt:  time.Date(2026, time.August, 13, 13, 0, 0, 0, time.UTC),
	}
	sink.Emit(governor.Event{Kind: governor.EventAdmission, GatewayContractHealth: &health})

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("policy health log is not JSON: %v; output=%q", err, output.String())
	}
	group, ok := record["gateway_contract_health"].(map[string]any)
	if !ok {
		t.Fatalf("gateway_contract_health group = %#v", record["gateway_contract_health"])
	}
	if group["state"] != "protocol_changed" || group["reason_code"] != "missing_or_invalid_field" || group["endpoint"] != "/api/settings" {
		t.Fatalf("gateway_contract_health group = %#v", group)
	}
	for _, forbidden := range []string{"upstream_health", "chatgpt_health", "synthetic-secret", "raw-management-body", "api-key"} {
		if strings.Contains(strings.ToLower(output.String()), strings.ToLower(forbidden)) {
			t.Fatalf("trace contains forbidden value %q: %s", forbidden, output.String())
		}
	}
}

func TestPolicySinkRendersAttemptTelemetry(t *testing.T) {
	var output bytes.Buffer
	sink := NewPolicySink(NewLogger(&output, 0))
	sink.Emit(governor.Event{
		Kind: governor.EventAttemptFinished,
		AttemptMetadata: provider.ResponseMetadata{
			AdapterVersion:    provider.CompatAdapterVersion,
			Transport:         "compat-http",
			SessionID:         "sha256:0123456789abcdef",
			RequestID:         "sha256:fedcba9876543210",
			StatusCode:        200,
			Duration:          12 * time.Millisecond,
			FirstTokenLatency: 3 * time.Millisecond,
			DeliveryState:     provider.DeliveryCompleted,
		},
	})
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("policy log is not JSON: %v; output=%q", err, output.String())
	}
	attempt, ok := record["attempt"].(map[string]any)
	if !ok {
		t.Fatalf("attempt group = %#v", record["attempt"])
	}
	checks := map[string]any{
		"adapter_version":     provider.CompatAdapterVersion,
		"transport":           "compat-http",
		"session_fingerprint": "sha256:0123456789abcdef",
		"status_code":         float64(200),
		"request_id":          "sha256:fedcba9876543210",
		"first_token_latency": "3ms",
		"duration":            "12ms",
		"delivery_state":      "completed",
		"retry_count":         float64(0),
		"fallback":            false,
		"usage_estimated":     false,
	}
	for key, want := range checks {
		if attempt[key] != want {
			t.Fatalf("attempt[%q] = %#v, want %#v", key, attempt[key], want)
		}
	}
}

func TestPolicySinkAttemptTelemetryRedaction(t *testing.T) {
	var output bytes.Buffer
	sink := NewPolicySink(NewLogger(&output, 0))
	sink.Emit(governor.Event{
		Kind: governor.EventAttemptFinished,
		AttemptMetadata: provider.ResponseMetadata{
			AdapterVersion: provider.CompatAdapterVersion,
			Transport:      "compat-http",
			SessionID:      "sha256:0123456789abcdef",
			RequestID:      "sha256:fedcba9876543210",
		},
	})
	outputText := strings.ToLower(output.String())
	for _, forbidden := range []string{"live-session", "bearer", "authorization", "api_key", "cookie", "prompt", "credential", "raw-header"} {
		if strings.Contains(outputText, forbidden) {
			t.Fatalf("attempt telemetry leaked %q: %s", forbidden, output.String())
		}
	}
}
