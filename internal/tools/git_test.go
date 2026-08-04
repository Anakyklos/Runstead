package tools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestGitStatusAndDiffObserveARealRepositoryWithoutMutatingIt(t *testing.T) {
	root := t.TempDir()
	runGitFixture(t, root, "init", "--quiet")
	runGitFixture(t, root, "config", "user.email", "runstead@example.invalid")
	runGitFixture(t, root, "config", "user.name", "Runstead Test")
	writeFixture(t, root, "tracked.txt", "before\n")
	runGitFixture(t, root, "add", "tracked.txt")
	runGitFixture(t, root, "commit", "--quiet", "-m", "fixture")
	writeFixture(t, root, "tracked.txt", "after\n")

	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	status := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitStatus, Arguments: arguments(`{}`)})
	diff := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitDiff, Arguments: arguments(`{}`)})
	statusData, statusOK := status.Data.(GitData)
	diffData, diffOK := diff.Data.(GitData)
	if !statusOK || !diffOK || !status.Success || !diff.Success || status.Failure != nil || diff.Failure != nil {
		t.Fatalf("status = %#v, diff = %#v", status, diff)
	}
	if !strings.Contains(statusData.Stdout, "## ") || !strings.Contains(statusData.Stdout, " M tracked.txt") {
		t.Fatalf("status stdout = %q", statusData.Stdout)
	}
	if !strings.Contains(diffData.Stdout, "-before") || !strings.Contains(diffData.Stdout, "+after") {
		t.Fatalf("diff stdout = %q", diffData.Stdout)
	}
	if status.Metadata.ExitCode != 0 || diff.Metadata.ExitCode != 0 || !status.Metadata.Untrusted || !diff.Metadata.Untrusted {
		t.Fatalf("Git metadata = %#v / %#v", status.Metadata, diff.Metadata)
	}
	if got := strings.TrimSpace(string(runGitFixture(t, root, "diff", "--cached"))); got != "" {
		t.Fatalf("git observation staged a change: %q", got)
	}
}

func TestGitObservationsPreserveExitCodesAndClassifyFailures(t *testing.T) {
	nonRepo, err := NewRegistry(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	observation := nonRepo.Execute(context.Background(), protocol.Action{Tool: ToolGitStatus, Arguments: arguments(`{}`)})
	if observation.Success || observation.Failure == nil || observation.Failure.Code != FailureNotGitRepository || observation.Metadata.ExitCode != 128 {
		t.Fatalf("non-repository status = %#v", observation)
	}

	registry, err := NewRegistry(Options{
		Workspace: t.TempDir(),
		RunGit: func(context.Context, []string, string) CommandResult {
			return CommandResult{Stdout: []byte("partial"), Stderr: []byte("diagnostic"), ExitCode: 7, Err: errors.New("exit 7")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitDiff, Arguments: arguments(`{}`)})
	if failed.Success || failed.Failure == nil || failed.Failure.Code != FailureGitFailure || failed.Metadata.ExitCode != 7 {
		t.Fatalf("Git failure = %#v", failed)
	}
}

func TestGitObservationsUseFixedArgumentsAndBoundBothStreams(t *testing.T) {
	root := t.TempDir()
	var gotArgs []string
	var gotDir string
	registry, err := NewRegistry(Options{
		Workspace: root,
		Limits: Limits{
			MaxGitStdoutBytes: 4,
			MaxGitStderrBytes: 3,
		},
		RunGit: func(_ context.Context, args []string, dir string) CommandResult {
			gotArgs = append([]string(nil), args...)
			gotDir = dir
			return CommandResult{
				Stdout:      []byte("123456"),
				Stderr:      []byte("error"),
				StdoutBytes: 6,
				StderrBytes: 5,
				ExitCode:    0,
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitStatus, Arguments: arguments(`{}`)})
	data, ok := observation.Data.(GitData)
	if !ok || !observation.Success || observation.Failure != nil {
		t.Fatalf("observation = %#v", observation)
	}
	if data.Stdout != "1234" || data.Stderr != "err" || !observation.Truncated {
		t.Fatalf("bounded Git data = %#v, observation = %#v", data, observation)
	}
	if observation.Metadata.StdoutBytesOriginal != 6 || observation.Metadata.StdoutBytesReturned != 4 || observation.Metadata.StderrBytesOriginal != 5 || observation.Metadata.StderrBytesReturned != 3 {
		t.Fatalf("bounded Git metadata = %#v", observation.Metadata)
	}
	if gotDir != root || !containsArgs(gotArgs, "--") || containsArgs(gotArgs, "sh") {
		t.Fatalf("Git invocation = %q in %q", gotArgs, gotDir)
	}
}

func TestGitObservationsHonorTimeout(t *testing.T) {
	registry, err := NewRegistry(Options{
		Workspace: t.TempDir(),
		Limits:    Limits{GitTimeout: 5 * time.Millisecond},
		RunGit: func(ctx context.Context, _ []string, _ string) CommandResult {
			<-ctx.Done()
			return CommandResult{Err: ctx.Err(), ExitCode: -1}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitDiff, Arguments: arguments(`{}`)})
	if observation.Failure == nil || observation.Failure.Code != FailureTimeout {
		t.Fatalf("Git timeout = %#v", observation)
	}
}

func runGitFixture(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
	return output
}
