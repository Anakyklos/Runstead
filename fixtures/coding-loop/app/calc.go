// Package calc is the deterministic sample repository of the #12
// inspect-edit-test-fix coding loop. It intentionally contains a real bug:
// ParseValues does not trim whitespace, so `go test ./...` fails until the
// implementation is corrected. The model must inspect multiple files, run the
// operator-declared test recipe, observe the real failure evidence, apply a
// corrective write, rerun the same recipe and let the control-plane verifier
// confirm completion.
package calc

import (
	"strconv"
	"strings"
)

// ParseValues parses the integer values of a comma-separated string. Empty
// entries are ignored.
//
// BUG(fixture): whitespace is not trimmed, so entries like " 2" fail to
// parse. The deterministic scenario fixes this implementation.
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
