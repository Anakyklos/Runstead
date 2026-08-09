// calc-wrong.go is the deterministic FIRST fix attempt of the #12 coding-loop
// scenario: it trims only inside SumValues, so TestSumValues passes while
// TestParseValuesTrimsWhitespace stays red. The model must diagnose the
// remaining failure from the real test evidence and apply a corrective write
// (calc-correct.go). This file is fixture input, not part of the app module.
package calc

import (
	"strconv"
	"strings"
)

// ParseValues parses the integer values of a comma-separated string. Empty
// entries are ignored.
func ParseValues(input string) ([]int, error) {
	if input == "" {
		return nil, nil
	}
	raw := strings.Split(input, ",")
	values := make([]int, 0, len(raw))
	for _, part := range raw {
		if part == "" {
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// SumValues sums the integer values of a comma-separated string. Empty
// entries are ignored.
func SumValues(input string) (int, error) {
	total := 0
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return 0, err
		}
		total += value
	}
	return total, nil
}
