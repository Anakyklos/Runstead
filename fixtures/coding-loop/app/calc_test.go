package calc

import (
	"reflect"
	"testing"
)

// TestParseValuesTrimsWhitespace fails against the initial fixture
// implementation: ParseValues does not trim, so " 2" cannot be parsed. A fix
// that only trims inside SumValues leaves this test failing; the corrective
// fix must trim inside ParseValues.
func TestParseValuesTrimsWhitespace(t *testing.T) {
	got, err := ParseValues("1, 2 , 3")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("ParseValues = %v, want [1 2 3]", got)
	}
}

// TestParseValuesIgnoresEmpty passes against the initial fixture
// implementation; it guards against a regression that would turn empty
// entries into parse errors.
func TestParseValuesIgnoresEmpty(t *testing.T) {
	got, err := ParseValues("1,,3")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{1, 3}) {
		t.Fatalf("ParseValues = %v, want [1 3]", got)
	}
}

// TestSumValues fails against the initial fixture implementation because
// ParseValues rejects the untrimmed " 2" entry. A fix that trims inside
// SumValues makes this test pass while TestParseValuesTrimsWhitespace stays
// red: the scenario therefore requires a second, corrective write.
func TestSumValues(t *testing.T) {
	got, err := SumValues("1, 2, 3")
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Fatalf("SumValues = %d, want 6", got)
	}
}
