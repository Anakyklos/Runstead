package state

import (
	"encoding/json"
	"testing"
)

// TestWorkUnitConcurrencyFromConfigJSON proves the durable scheduler
// configuration contract (issue #109 + review): the helper explicitly
// DISTINGUISHES real absence (Stage A legacy = serial 1) from
// present-but-corrupted values. Only true absence maps to the default;
// every incompatible shape returns an error the caller must fail closed on
// BEFORE the recovery pipeline.
func TestWorkUnitConcurrencyFromConfigJSON(t *testing.T) {
	// Empty snapshot: un-inspectable -> error (not "absent").
	if _, present, err := WorkUnitConcurrencyFromConfigJSON(""); present || err == nil {
		t.Fatalf("empty config = present:%t err:%v, want absent-with-error (fail closed)", present, err)
	}
	// Malformed JSON: un-inspectable -> error.
	if _, present, err := WorkUnitConcurrencyFromConfigJSON("not json"); present || err == nil {
		t.Fatalf("malformed config = present:%t err:%v, want absent-with-error", present, err)
	}
	// A Stage A task (no scheduler key) is the ONLY absent case: nothing to
	// adopt, caller uses DefaultConcurrency.
	if value, present, err := WorkUnitConcurrencyFromConfigJSON(`{"max_steps":24}`); present || err != nil || value != 0 {
		t.Fatalf("absent concurrency = %d/%t/%v, want 0/false/nil (legacy serial)", value, present, err)
	}
	// A present integer is adopted as persisted.
	snapshot, _ := json.Marshal(map[string]any{WorkUnitConcurrencyKey: 3, "max_steps": 24})
	if value, present, err := WorkUnitConcurrencyFromConfigJSON(string(snapshot)); !present || err != nil || value != 3 {
		t.Fatalf("present concurrency = %d/%t/%v, want 3/true/nil", value, present, err)
	}
	// Present but WRONG TYPES are corrupted state: never silently
	// reinterpreted as the default (issue #109 review).
	for name, bad := range map[string]any{
		"string": "2",
		"float":  2.5,
		"bool":   true,
		"null":   nil,
		"object": map[string]any{},
		"array":  []any{2},
	} {
		t.Run("invalid-"+name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{WorkUnitConcurrencyKey: bad})
			value, present, err := WorkUnitConcurrencyFromConfigJSON(string(raw))
			if !present || err == nil {
				t.Fatalf("invalid %T = %d/present:%t/err:%v, want present-with-error (fail closed)", bad, value, present, err)
			}
		})
	}
	// An out-of-range integer IS parsed (present, no type error); the range
	// contract lives in the workunit bounds and is enforced by the caller
	// pre-flight (resume refuses before recovery) and by the driver before
	// execution.
	if value, present, err := WorkUnitConcurrencyFromConfigJSON(`{"workunit_concurrency":99}`); !present || err != nil || value != 99 {
		t.Fatalf("out-of-range integer = %d/%t/%v, want 99/true/nil (range enforced by callers)", value, present, err)
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
