package workunit

// Lane classification for the M9 Stage B1 bounded scheduler (issue #109):
// a Work Unit may share the concurrent lane ONLY when its declared tool
// envelope is provably read-only. The lane is derived deterministically from
// the persisted envelope (tools_json); the model never supplies it and it is
// never stored separately.

// Lane is the scheduling class of one Work Unit.
type Lane string

const (
	// LaneShared marks a Work Unit eligible for the concurrent read-only
	// lane: explicit envelope, every tool in the closed observational set
	// (or an explicitly empty, fail-closed no-tools envelope).
	LaneShared Lane = "shared"
	// LaneExclusive marks a Work Unit that may never overlap another unit:
	// omitted (nil) envelope meaning the task default surface, any known
	// effectful tool, or any tool the scheduler cannot prove read-only.
	LaneExclusive Lane = "exclusive"
)

// readOnlyTools is the CLOSED observational set proven safe to overlap
// (issue #109 contract). A tool not listed here is never presumed
// read-only: unknown/future capabilities fail closed into the exclusive
// lane instead of becoming concurrent because the scheduler does not
// recognize their effects.
var readOnlyTools = map[string]bool{
	"read_file":   true,
	"list_files":  true,
	"search_text": true,
	"git_status":  true,
	"git_diff":    true,
}

// Classify returns the scheduling lane of a Work Unit from its persisted
// tool envelope. The nil-vs-empty distinction is security-significant and
// preserved exactly like the Stage A drift contract (issue #106 review):
//
//   - nil (omitted) = task default surface = exclusive;
//   - explicit [] = no tools = shared (provably read-only);
//   - every element of a non-empty explicit envelope in readOnlyTools =
//     shared;
//   - anything else (write_file, apply_patch, run_recipe, unknown/future)
//     = exclusive, fail-closed.
func Classify(tools []string) Lane {
	if tools == nil {
		return LaneExclusive
	}
	for _, tool := range tools {
		if !readOnlyTools[tool] {
			return LaneExclusive
		}
	}
	return LaneShared
}

// Scheduler concurrency contract (issue #109): the operator surface for
// `run`/`resume` is bounded. Invalid values fail before any Work Unit
// executes; the ceiling is the initial hard bound, NOT a performance
// inference from provider/model/observed success.
const (
	DefaultConcurrency = 1 // Stage A behavior: serial.
	MinConcurrency     = 1
	MaxConcurrency     = 4 // initial hard ceiling.
)
