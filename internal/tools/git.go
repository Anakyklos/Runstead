package tools

import (
	"context"
)

func (r *Registry) executeGit(ctx context.Context, observation Observation, tool string) Observation {
	commandContext, cancel := context.WithTimeout(ctx, r.limits.GitTimeout)
	defer cancel()
	args := gitArguments(tool)
	result := r.runGit(commandContext, args, r.workspace.root)
	stdoutOriginal := result.StdoutBytes
	if stdoutOriginal < int64(len(result.Stdout)) {
		stdoutOriginal = int64(len(result.Stdout))
	}
	stderrOriginal := result.StderrBytes
	if stderrOriginal < int64(len(result.Stderr)) {
		stderrOriginal = int64(len(result.Stderr))
	}
	stdout := boundedBytes(result.Stdout, r.limits.MaxGitStdoutBytes)
	stderr := boundedBytes(result.Stderr, r.limits.MaxGitStderrBytes)
	truncated := stdoutOriginal > int64(r.limits.MaxGitStdoutBytes) || stderrOriginal > int64(r.limits.MaxGitStderrBytes)
	observation.Metadata = Metadata{
		Source:              tool,
		Untrusted:           true,
		StdoutBytesOriginal: stdoutOriginal,
		StdoutBytesReturned: int64(len(stdout)),
		StderrBytesOriginal: stderrOriginal,
		StderrBytesReturned: int64(len(stderr)),
		ExitCode:            result.ExitCode,
		Signal:              result.Signal,
	}
	observation.Truncated = truncated
	observation.Data = GitData{Stdout: string(stdout), Stderr: string(stderr)}
	if failure := commandResultContextFailure(ctx, commandContext); failure != nil {
		observation.Failure = failure
		return observation
	}
	if result.ExitCode == 128 {
		observation.Failure = newFailure(FailureNotGitRepository)
		return observation
	}
	if result.ExitCode != 0 || result.Err != nil {
		observation.Failure = newFailure(FailureGitFailure)
		return observation
	}
	observation.Success = true
	return observation
}

func gitArguments(tool string) []string {
	common := []string{"--no-pager", "-c", "color.ui=false", "-c", "core.quotepath=false", "-c", "core.pager=cat", "-c", "core.fsmonitor=false"}
	if tool == ToolGitStatus {
		return append(common, "status", "--short", "--branch", "--no-renames", "--untracked-files=all", "--", ".")
	}
	return append(common, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--", ".")
}

func boundedBytes(value []byte, limit int) []byte {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
