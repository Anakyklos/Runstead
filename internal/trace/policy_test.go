package trace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/policy"
)

func TestPolicySinkEmitsSanitizedStructuredEvent(t *testing.T) {
	var output bytes.Buffer
	sink := NewPolicySink(NewLogger(&output, 0))
	sink.Emit(policy.Event{
		Kind:             policy.EventAttemptFinished,
		AccountPolicyID:  "policy-account-1",
		ProviderID:       "omniroute",
		ModelPool:        "instant",
		AllowanceProfile: policy.AllowanceProfileInstant,
		TaskID:           "task-1",
		ClientRequestID:  "request-1",
		AttemptSequence:  1,
		Outcome:          policy.OutcomeSuccess,
	})
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("policy log is not JSON: %v; output=%q", err, output.String())
	}
	if record["msg"] != "account policy event" || record["task_id"] != "task-1" || record["outcome"] != string(policy.OutcomeSuccess) {
		t.Fatalf("record = %#v", record)
	}
	for _, forbidden := range []string{"prompt", "response", "token", "cookie", "credential", "api_key", "body"} {
		if strings.Contains(strings.ToLower(output.String()), forbidden) {
			t.Fatalf("policy event contains forbidden field %q: %s", forbidden, output.String())
		}
	}
}
