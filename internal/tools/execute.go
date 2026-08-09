package tools

import (
	"context"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func (r *Registry) Execute(ctx context.Context, action protocol.Action) Observation {
	observation := r.newObservation(action.Tool, normalizedArguments(action))
	if ctx == nil {
		ctx = context.Background()
	}
	if failure := contextFailure(ctx); failure != nil {
		observation.Failure = failure
		return observation
	}
	registered, err := r.ValidateArguments(action.Tool, action.Arguments)
	if !registered {
		observation.Failure = newFailure(FailureUnknownTool)
		return observation
	}
	if err != nil {
		if failure, ok := err.(*Failure); ok {
			observation.Failure = failure
		} else {
			observation.Failure = newFailure(FailureInvalidArguments)
		}
		return observation
	}

	switch action.Tool {
	case ToolReadFile:
		path, _ := stringArgument(action.Arguments, "path")
		return r.executeReadFile(ctx, observation, path)
	case ToolListFiles:
		path, _ := stringArgument(action.Arguments, "path")
		return r.executeListFiles(ctx, observation, path)
	case ToolSearchText:
		query, _ := stringArgument(action.Arguments, "query")
		path, _ := stringArgument(action.Arguments, "path")
		return r.executeSearchText(ctx, observation, query, path)
	case ToolGitStatus, ToolGitDiff:
		return r.executeGit(ctx, observation, action.Tool)
	case ToolWriteFile:
		path, _ := stringArgument(action.Arguments, "path")
		content, _ := stringArgumentAllowEmpty(action.Arguments, "content")
		expected, _ := stringArgument(action.Arguments, "expected_before_hash")
		return r.executeWriteFile(ctx, observation, path, content, expected)
	case ToolApplyPatch:
		path, _ := stringArgument(action.Arguments, "path")
		patch, _ := stringArgumentAllowEmpty(action.Arguments, "patch")
		expected, _ := stringArgument(action.Arguments, "expected_before_hash")
		return r.executeApplyPatch(ctx, observation, path, patch, expected)
	case ToolRunRecipe:
		recipeID, _ := stringArgument(action.Arguments, "recipe")
		return r.executeRunRecipe(ctx, observation, recipeID)
	default:
		observation.Failure = newFailure(FailureInvalidArguments)
		return observation
	}
}

func (r *Registry) newObservation(tool string, arguments any) Observation {
	sequence := r.nextID.Add(1)
	return Observation{
		ID:        fmt.Sprintf("obs-%06d", sequence),
		Tool:      tool,
		Arguments: arguments,
		Metadata:  Metadata{Source: tool, ExitCode: -1},
	}
}

func normalizedArguments(action protocol.Action) any {
	arguments := map[string]string{}
	switch action.Tool {
	case ToolReadFile, ToolListFiles:
		if path, failure := stringArgument(action.Arguments, "path"); failure == nil {
			if normalized, pathFailure := normalizeRelativePath(path); pathFailure == nil {
				arguments["path"] = normalized
			}
		}
	case ToolSearchText:
		query, queryFailure := stringArgument(action.Arguments, "query")
		path, pathFailure := stringArgument(action.Arguments, "path")
		if queryFailure == nil && pathFailure == nil {
			if normalized, failure := normalizeRelativePath(path); failure == nil {
				arguments["query"] = query
				arguments["path"] = normalized
			}
		}
	case ToolWriteFile, ToolApplyPatch:
		path, pathFailure := stringArgument(action.Arguments, "path")
		expected, expectedFailure := stringArgument(action.Arguments, "expected_before_hash")
		if pathFailure == nil && expectedFailure == nil {
			if normalized, failure := normalizeRelativePath(path); failure == nil {
				arguments["path"] = normalized
				arguments["expected_before_hash"] = expected
				if action.Tool == ToolWriteFile {
					if content, contentFailure := stringArgumentAllowEmpty(action.Arguments, "content"); contentFailure == nil {
						arguments["content"] = content
					}
				} else {
					if patch, patchFailure := stringArgumentAllowEmpty(action.Arguments, "patch"); patchFailure == nil {
						arguments["patch"] = patch
					}
				}
			}
		}
	case ToolRunRecipe:
		if recipeID, failure := stringArgument(action.Arguments, "recipe"); failure == nil {
			arguments["recipe"] = recipeID
		}
	}
	return arguments
}

func contextFailure(ctx context.Context) *Failure {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return newFailure(FailureTimeout)
	}
	return newFailure(FailureCanceled)
}
