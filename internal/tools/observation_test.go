package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestExecuteGeneratesOpaqueSequentialObservationIDs(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "one.txt", "one")
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	action := protocol.Action{Tool: ToolReadFile, Arguments: arguments(`{"path":"one.txt"}`)}
	first := registry.Execute(context.Background(), action)
	second := registry.Execute(context.Background(), action)
	if first.ID != "obs-000001" || second.ID != "obs-000002" {
		t.Fatalf("observation IDs = %q, %q", first.ID, second.ID)
	}
	if first.ID == string(action.Arguments["id"]) || first.Arguments == nil {
		t.Fatalf("observation accepted a model identifier: %#v", first)
	}
}

func TestExecuteRejectsUnknownAndInvalidActionsBeforeRunningTools(t *testing.T) {
	runs := 0
	registry, err := NewRegistry(Options{
		Workspace: t.TempDir(),
		RunGit: func(context.Context, []string, string) CommandResult {
			runs++
			return CommandResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	unknown := registry.Execute(context.Background(), protocol.Action{Tool: "shell", Arguments: arguments(`{}`)})
	invalid := registry.Execute(context.Background(), protocol.Action{Tool: ToolGitStatus, Arguments: arguments(`{"unexpected":true}`)})
	if unknown.Success || unknown.Failure == nil || unknown.Failure.Code != FailureUnknownTool {
		t.Fatalf("unknown execution = %#v", unknown)
	}
	if invalid.Success || invalid.Failure == nil || invalid.Failure.Code != FailureInvalidArguments {
		t.Fatalf("invalid execution = %#v", invalid)
	}
	if runs != 0 {
		t.Fatalf("invalid actions invoked a tool %d times", runs)
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
