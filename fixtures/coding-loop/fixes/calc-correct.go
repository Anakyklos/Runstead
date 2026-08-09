// calc-correct.go is the deterministic corrective fix of the #12 coding-loop
// scenario: it trims inside ParseValues, so the whole test suite passes. This
// file is fixture input, not part of the app module.
package calc

import (
	"strconv"
	"strings"
)

// ParseValues parses the integer values of a comma-separated string. Empty
// entries are ignored.
func ParseValues(input string) ([]int, error) {
	if strings.TrimSpace(input) == "" {
		return nil, nil
	}
	raw := strings.Split(input, ",")
	values := make([]int, 0, len(raw))
	for _, part := range raw {
		part = strings.TrimSpace(part)
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
	values, err := ParseValues(input)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, value := range values {
		total += value
	}
	return total, nil
}
