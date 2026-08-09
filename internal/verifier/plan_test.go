package verifier

import (
	"strings"
	"testing"
)

func TestParsePlanValid(t *testing.T) {
	plan, err := ParsePlan([]byte(`{
		"version": 1,
		"checks": [
			{"id":"artifact-exists","type":"file_exists","path":"src/main.go"},
			{"id":"tests-pass","type":"recipe_exit_zero","recipe":"test","require_untruncated":true}
		]
	}`))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if plan.Version != 1 || len(plan.Checks) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestParsePlanRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty version", `{"version":2,"checks":[]}`},
		{"missing version", `{"checks":[]}`},
		{"unknown field", `{"version":1,"checks":[],"bogus":true}`},
		{"duplicate key", `{"version":1,"version":1,"checks":[]}`},
		{"duplicate check id", `{"version":1,"checks":[{"id":"a","type":"file_exists","path":"x"},{"id":"a","type":"file_exists","path":"y"}]}`},
		{"empty check id", `{"version":1,"checks":[{"id":"","type":"file_exists","path":"x"}]}`},
		{"unknown type", `{"version":1,"checks":[{"id":"a","type":"bogus"}]}`},
		{"file check without path", `{"version":1,"checks":[{"id":"a","type":"file_exists"}]}`},
		{"file_hash without sha", `{"version":1,"checks":[{"id":"a","type":"file_hash","path":"x"}]}`},
		{"recipe check without recipe", `{"version":1,"checks":[{"id":"a","type":"recipe_exit_zero"}]}`},
		{"trailing json", `{"version":1,"checks":[]} {"x":1}`},
		{"trailing garbage", `{"version":1,"checks":[]} garbage`},
		{"not an object", `[1,2]`},
	}
	for _, tc := range cases {
		if _, err := ParsePlan([]byte(tc.raw)); err == nil {
			t.Fatalf("%s: plan %s must be rejected", tc.name, tc.raw)
		}
	}
	// Whitespace after the plan is fine.
	if _, err := ParsePlan([]byte("{\"version\":1,\"checks\":[]}\n \t")); err != nil {
		t.Fatalf("trailing whitespace must be accepted: %v", err)
	}
}

func TestPlanDigestStableAndSensitive(t *testing.T) {
	base := `{"version":1,"checks":[{"id":"a","type":"file_exists","path":"x.txt"}]}`
	planA, err := ParsePlan([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	planB, err := ParsePlan([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if planA.Digest() != planB.Digest() {
		t.Fatal("digest must be stable")
	}
	changed, err := ParsePlan([]byte(`{"version":1,"checks":[{"id":"a","type":"file_exists","path":"y.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if planA.Digest() == changed.Digest() {
		t.Fatal("digest must change when a check changes")
	}
	added, err := ParsePlan([]byte(`{"version":1,"checks":[{"id":"a","type":"file_exists","path":"x.txt"},{"id":"b","type":"file_absent","path":"z.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if planA.Digest() == added.Digest() {
		t.Fatal("digest must change when a check is added")
	}
	// Order of checks must not change the digest.
	reordered, err := ParsePlan([]byte(`{"version":1,"checks":[{"id":"b","type":"file_absent","path":"z.txt"},{"id":"a","type":"file_exists","path":"x.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if added.Digest() != reordered.Digest() {
		t.Fatal("digest must be canonical across check order")
	}
}

func TestEmptyPlan(t *testing.T) {
	plan := EmptyPlan()
	if plan.Version != PlanVersion || len(plan.Checks) != 0 {
		t.Fatalf("empty plan = %+v", plan)
	}
	if strings.TrimSpace(plan.Digest()) == "" {
		t.Fatal("empty plan digest must not be empty")
	}
}
