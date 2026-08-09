package tools

import (
	"context"
)

func (r *Registry) executeGit(ctx context.Context, observation Observation, tool string) Observation {
	result := r.runGitObservation(ctx, tool)
	observation.Metadata = result.metadata
	observation.Truncated = result.truncated
	observation.Data = GitData{Stdout: result.stdout, Stderr: result.stderr}
	if result.commandFailure != nil {
		observation.Failure = result.commandFailure
		return observation
	}
	if result.exitCode == 128 {
		observation.Failure = newFailure(FailureNotGitRepository)
		return observation
	}
	if result.exitCode != 0 || result.err != nil {
		observation.Failure = newFailure(FailureGitFailure)
		return observation
	}
	observation.Success = true
	return observation
}

// gitObservation is the bounded result of one authoritative git observation.
// commandFailure is captured BEFORE the command context is canceled, so
// callers never observe a spurious canceled context.
type gitObservation struct {
	stdout         string
	stderr         string
	exitCode       int
	signal         string
	truncated      bool
	metadata       Metadata
	commandFailure *Failure
	err            error
}

// runGitObservation executes one fixed git observation (status or diff) with
// the registry's git seam and bounds. It is the shared implementation for the
// model-facing git tools and the control-plane verifier, so verification uses
// the exact same bounded, non-interactive git observation as the tools.
func (r *Registry) runGitObservation(ctx context.Context, tool string) gitObservation {
	if ctx == nil {
		ctx = context.Background()
	}
	commandContext, cancel := context.WithTimeout(ctx, r.limits.GitTimeout)
	args := gitArguments(tool)
	result := r.runGit(commandContext, args, r.workspace.root)
	// Capture the command failure while the context is still alive; the
	// deferred cancel must not race the caller's read of the result.
	commandFailure := commandResultContextFailure(ctx, commandContext)
	cancel()
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
	return gitObservation{
		stdout:    string(stdout),
		stderr:    string(stderr),
		exitCode:  result.ExitCode,
		signal:    result.Signal,
		truncated: truncated,
		metadata: Metadata{
			Source:              tool,
			Untrusted:           true,
			StdoutBytesOriginal: stdoutOriginal,
			StdoutBytesReturned: int64(len(stdout)),
			StderrBytesOriginal: stderrOriginal,
			StderrBytesReturned: int64(len(stderr)),
			ExitCode:            result.ExitCode,
			Signal:              result.Signal,
		},
		commandFailure: commandFailure,
		err:            result.Err,
	}
}

// GitStatusText returns the authoritative bounded `git status --short`
// observation of the workspace. It is the control-plane observer seam for the
// verifier (issue #11): the verifier never runs git itself and never reads
// model text; it consumes this observation.
func (r *Registry) GitStatusText() (text string, truncated bool, failure error) {
	observation := r.runGitObservation(context.Background(), ToolGitStatus)
	if observation.commandFailure != nil {
		return "", observation.truncated, observation.commandFailure
	}
	if observation.exitCode == 128 {
		return "", observation.truncated, newFailure(FailureNotGitRepository)
	}
	if observation.exitCode != 0 || observation.err != nil {
		return "", observation.truncated, newFailure(FailureGitFailure)
	}
	return observation.stdout, observation.truncated, nil
}

// GitDiffText returns the authoritative bounded `git diff` observation of the
// workspace. It is the control-plane observer seam for the verifier (issue
// #11); it never runs git itself.
func (r *Registry) GitDiffText() (text string, truncated bool, failure error) {
	observation := r.runGitObservation(context.Background(), ToolGitDiff)
	if observation.commandFailure != nil {
		return "", observation.truncated, observation.commandFailure
	}
	if observation.exitCode == 128 {
		return "", observation.truncated, newFailure(FailureNotGitRepository)
	}
	if observation.exitCode != 0 || observation.err != nil {
		return "", observation.truncated, newFailure(FailureGitFailure)
	}
	return observation.stdout, observation.truncated, nil
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
