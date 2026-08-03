package trace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/governor"
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
		if strings.Contains(strings.ToLower(output.String()), forbidden) {
			t.Fatalf("policy event contains forbidden field %q: %s", forbidden, output.String())
		}
	}
}
