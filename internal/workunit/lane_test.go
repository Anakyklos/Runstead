package workunit

import "testing"

// TestClassifyMatrix proves the fail-closed shared/exclusive classification
// of a Work Unit from its persisted tool envelope (issue #109): ONLY an
// explicit envelope contained in the closed observational set is shared;
// omitted (nil = task default surface) and any unknown/effectful tool are
// exclusive. The model never supplies the lane; it is derived determinis-
// tically from the persisted envelope.
func TestClassifyMatrix(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  Lane
	}{
		// Omitted envelope (nil) = task default surface = exclusive.
		{name: "omitted nil envelope", tools: nil, want: LaneExclusive},
		// Explicitly EMPTY envelope = no tools = provably read-only.
		{name: "explicit empty envelope", tools: []string{}, want: LaneShared},
		// Every observational tool alone and together is shared.
		{name: "read_file", tools: []string{"read_file"}, want: LaneShared},
		{name: "list_files", tools: []string{"list_files"}, want: LaneShared},
		{name: "search_text", tools: []string{"search_text"}, want: LaneShared},
		{name: "git_status", tools: []string{"git_status"}, want: LaneShared},
		{name: "git_diff", tools: []string{"git_diff"}, want: LaneShared},
		{name: "all observational", tools: []string{"read_file", "list_files", "search_text", "git_status", "git_diff"}, want: LaneShared},
		{name: "observational unsorted duplicates", tools: []string{"git_diff", "read_file", "read_file"}, want: LaneShared},
		// Every effectful tool is exclusive, alone or mixed with readers.
		{name: "write_file", tools: []string{"write_file"}, want: LaneExclusive},
		{name: "apply_patch", tools: []string{"apply_patch"}, want: LaneExclusive},
		{name: "run_recipe", tools: []string{"run_recipe"}, want: LaneExclusive},
		{name: "read plus write", tools: []string{"read_file", "write_file"}, want: LaneExclusive},
		{name: "observational plus apply_patch", tools: []string{"list_files", "apply_patch"}, want: LaneExclusive},
		{name: "observational plus run_recipe", tools: []string{"search_text", "run_recipe"}, want: LaneExclusive},
		// Fail-closed for future/unknown capability surfaces: an
		// unrecognized tool is NEVER presumed read-only.
		{name: "unknown future tool", tools: []string{"future_scanner"}, want: LaneExclusive},
		{name: "read plus unknown", tools: []string{"read_file", "future_scanner"}, want: LaneExclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.tools); got != tc.want {
				t.Fatalf("Classify(%v) = %s, want %s", tc.tools, got, tc.want)
			}
		})
	}
}

// TestClassifyLaneString proves the stable lane spellings used by
// diagnostics.
func TestClassifyLaneString(t *testing.T) {
	if got := string(LaneShared); got != "shared" {
		t.Fatalf("shared lane string = %q", got)
	}
	if got := string(LaneExclusive); got != "exclusive" {
		t.Fatalf("exclusive lane string = %q", got)
	}
}
