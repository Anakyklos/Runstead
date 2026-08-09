package tools

import (
	"context"
	"os"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/recipe"
)

// executeRunRecipe performs one run_recipe effect. The contract (issue #26):
//
//  1. the model selected a recipe ID from the operator-controlled catalog;
//  2. the caller (agent loop) consulted the control-plane policy and persisted
//     the decision before calling;
//  3. the working directory is resolved against the canonical workspace
//     boundary (absolute, traversal, symlink escapes fail closed);
//  4. the child environment is built from the recipe allowlist (credential
//     names never inherited);
//  5. the process runs in its own process group with a wall-clock timeout;
//     the whole group is terminated on timeout or cancellation;
//  6. stdout/stderr are bounded independently with explicit truncation;
//  7. the observed result becomes structured recipe.Evidence.
//
// A process that ran (even with a non-zero exit, a signal, a timeout or a
// cancellation) produces citable evidence: exit code 0 is never conflated
// with task completion. Only start failures and validation failures carry no
// evidence.
func (r *Registry) executeRunRecipe(ctx context.Context, observation Observation, recipeID string) Observation {
	if r.recipes == nil {
		observation.Failure = newFailure(FailureNoRecipes)
		return observation
	}
	selected, ok := r.recipes.Get(recipeID)
	if !ok {
		observation.Failure = newFailure(FailureUnknownRecipe)
		return observation
	}

	// Working directory boundary: the recipe's relative working directory is
	// resolved with the same canonical security model as reads and writes.
	// The result is an absolute canonical path guaranteed to live inside the
	// workspace.
	cwd, normalized, failure := r.resolveRecipeWorkingDirectory(selected.WorkingDirectory)
	if failure != nil {
		observation.Failure = failure
		return observation
	}

	// Child environment: explicit allowlist, never the full parent
	// environment, never credential-shaped names.
	env := recipe.BuildEnvironment(os.Environ(), selected)

	result := r.runRecipe(ctx, selected, cwd, env)
	if !result.Started {
		observation.Failure = newFailure(FailureRecipeStart)
		return observation
	}
	evidence := recipe.BuildEvidence(selected, normalized, recipe.PolicyDecision{Decision: "allowed", Reason: "policy_allow"}, result)
	observation.Success = true
	observation.Data = evidence
	observation.Metadata = Metadata{
		Source:              ToolRunRecipe,
		Untrusted:           true,
		Path:                normalized,
		StdoutBytesOriginal: result.StdoutBytes,
		StdoutBytesReturned: int64(len(result.Stdout)),
		StderrBytesOriginal: result.StderrBytes,
		StderrBytesReturned: int64(len(result.Stderr)),
		ExitCode:            result.ExitCode,
		Signal:              result.Signal,
	}
	observation.Truncated = result.StdoutTruncated || result.StderrTruncated
	return observation
}

// AnnotateRecipeEvidence fills the execution identities of a successful recipe
// observation with the REAL control-plane policy decision. The loop calls it
// after the execution id is allocated and before TX 2: the persisted evidence
// carries action/execution/evidence ids and the actual policy outcome (for
// example allowed/approved_by_operator), never a hardcoded placeholder.
func (r *Registry) AnnotateRecipeEvidence(observation *Observation, actionID, executionID, decision, reason string) {
	if observation == nil || !observation.Success {
		return
	}
	evidence, ok := observation.Data.(recipe.Evidence)
	if !ok {
		return
	}
	evidence.ActionID = actionID
	evidence.ExecutionID = executionID
	evidence.EvidenceID = observation.ID
	evidence.Policy = recipe.PolicyDecision{Decision: decision, Reason: reason}
	observation.Data = evidence
}

// resolveRecipeWorkingDirectory resolves a recipe's relative working directory
// to an absolute canonical path inside the workspace. Empty means the
// workspace root. The parent directory must exist and must resolve inside the
// workspace; symlink escapes and traversal fail closed.
func (r *Registry) resolveRecipeWorkingDirectory(relative string) (absolute, normalized string, failure *Failure) {
	target := "."
	if strings.TrimSpace(relative) != "" {
		target = relative
	}
	resolved, failure := r.workspace.resolve(target)
	if failure != nil {
		return "", "", failure
	}
	if !resolved.info.IsDir() {
		return "", "", newFailure(FailureWrongType)
	}
	return resolved.canonical, resolved.relative, nil
}
