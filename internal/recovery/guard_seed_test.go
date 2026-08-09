package recovery

import (
	"encoding/json"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/recipe"
	"github.com/RenyEnnos/Runstead/internal/state"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TestBuildSeedContinuesFailureGuards proves that the #12 failure-guard
// counters in the recovery seed are recomputed from the persisted trailing
// streaks: the consecutive tool/process failure streak from the trailing
// failing observations, and the repeated-verification-failure streak from the
// trailing failed verification attempts. A resumed run therefore continues
// the guards instead of silently resetting them.
func TestBuildSeedContinuesFailureGuards(t *testing.T) {
	snapshot := &state.RecoverySnapshot{
		Task: state.RecoveryTask{TaskID: "task-seed", Objective: "o", Workspace: "/ws", Status: "running"},
		ToolAttempts: []state.RecoveryToolAttempt{
			// Executed in this order: completed read (success, resets),
			// failed read (failure), completed recipe with exit 1 (process
			// failure), failed write (failure). The trailing streak is 3.
			{ExecutionID: "exec-000001", Tool: tools.ToolReadFile, Status: "completed", EvidenceID: "obs-000001"},
			{ExecutionID: "exec-000002", Tool: tools.ToolReadFile, Status: "failed", Classification: "path_not_found"},
			{ExecutionID: "exec-000003", Tool: tools.ToolRunRecipe, Status: "completed", EvidenceID: "obs-000003"},
			{ExecutionID: "exec-000004", Tool: tools.ToolWriteFile, Status: "failed", Classification: "stale_state"},
		},
		Evidence: []state.RecoveryEvidence{
			{EvidenceID: "obs-000001", Tool: tools.ToolReadFile, DataJSON: `{"path":"a.txt"}`},
			{EvidenceID: "obs-000003", Tool: tools.ToolRunRecipe, DataJSON: mustEncode(t, recipe.Evidence{RecipeID: "test", ExitCode: 1, Started: true, NetworkIsolation: recipe.NetworkIsolationValue})},
		},
		VerificationAttempts: []state.VerificationAttemptRow{
			{Sequence: 1, Decision: "passed"},
			{Sequence: 2, Decision: "failed"},
			{Sequence: 3, Decision: "failed"},
		},
	}
	seed := buildSeed(snapshot, Context{Text: "ctx"}, 0)
	if seed.ConsecutiveFailures != 3 {
		t.Fatalf("ConsecutiveFailures = %d, want 3 (the trailing failing streak)", seed.ConsecutiveFailures)
	}
	if seed.VerificationRetries != 2 {
		t.Fatalf("VerificationRetries = %d, want 2 (the trailing failed verifications)", seed.VerificationRetries)
	}
}

// TestBuildSeedFailureGuardsResetOnTrailingSuccess proves a trailing success
// resets the streaks: the seed carries zero when the last observation (or the
// last verification) was not a failure, even when earlier failures exist.
func TestBuildSeedFailureGuardsResetOnTrailingSuccess(t *testing.T) {
	snapshot := &state.RecoverySnapshot{
		Task: state.RecoveryTask{TaskID: "task-seed-ok", Objective: "o", Workspace: "/ws", Status: "running"},
		ToolAttempts: []state.RecoveryToolAttempt{
			{ExecutionID: "exec-000001", Tool: tools.ToolReadFile, Status: "failed", Classification: "path_not_found"},
			{ExecutionID: "exec-000002", Tool: tools.ToolReadFile, Status: "completed", EvidenceID: "obs-000002"},
		},
		Evidence: []state.RecoveryEvidence{
			{EvidenceID: "obs-000002", Tool: tools.ToolReadFile, DataJSON: `{"path":"a.txt"}`},
		},
		VerificationAttempts: []state.VerificationAttemptRow{
			{Sequence: 1, Decision: "failed"},
			{Sequence: 2, Decision: "blocked"},
		},
	}
	seed := buildSeed(snapshot, Context{Text: "ctx"}, 0)
	if seed.ConsecutiveFailures != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 (the trailing observation was a success)", seed.ConsecutiveFailures)
	}
	if seed.VerificationRetries != 0 {
		t.Fatalf("VerificationRetries = %d, want 0 (the trailing verification was blocked)", seed.VerificationRetries)
	}
}

// TestBuildSeedRecipeExitZeroBreaksStreak proves a completed recipe whose
// real exit code is zero is a success for the guard: only a non-zero exit
// counts as a process failure.
func TestBuildSeedRecipeExitZeroBreaksStreak(t *testing.T) {
	snapshot := &state.RecoverySnapshot{
		Task: state.RecoveryTask{TaskID: "task-seed-zero", Objective: "o", Workspace: "/ws", Status: "running"},
		ToolAttempts: []state.RecoveryToolAttempt{
			{ExecutionID: "exec-000001", Tool: tools.ToolReadFile, Status: "failed", Classification: "path_not_found"},
			{ExecutionID: "exec-000002", Tool: tools.ToolRunRecipe, Status: "completed", EvidenceID: "obs-000002"},
		},
		Evidence: []state.RecoveryEvidence{
			{EvidenceID: "obs-000002", Tool: tools.ToolRunRecipe, DataJSON: mustEncode(t, recipe.Evidence{RecipeID: "test", ExitCode: 0, Started: true, NetworkIsolation: recipe.NetworkIsolationValue})},
		},
	}
	seed := buildSeed(snapshot, Context{Text: "ctx"}, 0)
	if seed.ConsecutiveFailures != 0 {
		t.Fatalf("ConsecutiveFailures = %d, want 0 (the trailing recipe exit was zero)", seed.ConsecutiveFailures)
	}
}

func mustEncode(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}
