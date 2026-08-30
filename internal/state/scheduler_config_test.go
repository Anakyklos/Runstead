package state

import (
	"encoding/json"
	"testing"
)

// TestWorkUnitConcurrencyFromConfigJSON proves the durable scheduler
// configuration contract (issue #109): the effective concurrency runs are
// persisted inside the task's existing config_json (no new migration) and
// read back authoritatively. Absent = the Stage A serial contract (1);
// a present value is returned as persisted; a non-integer value is NOT
// adopted (the caller fails closed).
func TestWorkUnitConcurrencyFromConfigJSON(t *testing.T) {
	if got, ok := WorkUnitConcurrencyFromConfigJSON(""); got != 0 || ok {
		t.Fatalf("empty config = %d/%t, want 0/false", got, ok)
	}
	snapshot := map[string]any{
		"max_steps":              24,
		WorkUnitConcurrencyKey:   3,
		"provider_id":            "op",
		"acceptance_plan_digest": "d",
		"does_not_leak":          "sanitized-only",
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := WorkUnitConcurrencyFromConfigJSON(string(raw)); !ok || got != 3 {
		t.Fatalf("present concurrency = %d/%t, want 3/true", got, ok)
	}
	// A Stage A task (no scheduler key) reads as absent.
	if got, ok := WorkUnitConcurrencyFromConfigJSON(`{"max_steps":24}`); got != 0 || ok {
		t.Fatalf("absent concurrency = %d/%t, want 0/false", got, ok)
	}
	// Malformed JSON is absent (caller validates; never guessed).
	if got, ok := WorkUnitConcurrencyFromConfigJSON("not json"); got != 0 || ok {
		t.Fatalf("malformed config = %d/%t, want 0/false", got, ok)
	}
	// The key must never collide with another snapshot field.
	other, _ := json.Marshal(map[string]any{WorkUnitConcurrencyKey: "not-an-int"})
	if got, ok := WorkUnitConcurrencyFromConfigJSON(string(other)); got != 0 || ok {
		t.Fatalf("non-integer concurrency = %d/%t, want 0/false (fail closed, never adopted)", got, ok)
	}
}

// TestWorkUnitConcurrencyKeyStable proves the persisted JSON key is the
// versioned contract spelling; renaming it silently would break resume
// continuity for tasks persisted under the previous spelling.
func TestWorkUnitConcurrencyKeyStable(t *testing.T) {
	if WorkUnitConcurrencyKey != "workunit_concurrency" {
		t.Fatalf("WorkUnitConcurrencyKey = %q", WorkUnitConcurrencyKey)
	}
}
