package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestRegistryIsTheRealParserCatalogAndOnlyAcceptedActionsExecute(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "README.md", "read me")
	gitRuns := 0
	registry, err := NewRegistry(Options{
		Workspace: root,
		RunGit: func(context.Context, []string, string) CommandResult {
			gitRuns++
			return CommandResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	valid := protocol.Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`, registry)
	if valid.Failure != nil || !valid.Accepted || !valid.Executable || valid.Action == nil {
		t.Fatalf("valid parser result = %#v", valid)
	}
	observation := registry.Execute(context.Background(), *valid.Action)
	if !observation.Success || observation.Failure != nil {
		t.Fatalf("accepted action observation = %#v", observation)
	}

	unknown := protocol.Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"shell","arguments":{}}</runstead_action>`, registry)
	if unknown.Failure == nil || unknown.Failure.Code != protocol.FailureUnknownTool || unknown.Executable {
		t.Fatalf("unknown parser result = %#v", unknown)
	}
	invalid := protocol.Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"git_status","arguments":{"id":"model-id"}}</runstead_action>`, registry)
	if invalid.Failure == nil || invalid.Failure.Code != protocol.FailureInvalidArguments || invalid.Executable {
		t.Fatalf("invalid parser result = %#v", invalid)
	}

	validatedOnly := protocol.Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"git_status","arguments":{}}</runstead_action>`, registry)
	if validatedOnly.Failure != nil || !validatedOnly.Executable || gitRuns != 0 {
		t.Fatalf("parser validation executed Git: result=%#v runs=%d", validatedOnly, gitRuns)
	}
	registry.Execute(context.Background(), *validatedOnly.Action)
	if gitRuns != 1 {
		t.Fatalf("accepted Git action ran %d times, want once", gitRuns)
	}
}

func TestObservationTreatsCredentialLikeFileContentAsUntrustedData(t *testing.T) {
	root := t.TempDir()
	secret := "Authorization: Bearer test-token-value"
	writeFixture(t, root, "config.txt", secret)
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolReadFile,
		Arguments: arguments(`{"path":"config.txt"}`),
	})
	data, ok := observation.Data.(FileData)
	if !ok || data.Content != secret || !observation.Metadata.Untrusted {
		t.Fatalf("credential-like content observation = %#v", observation)
	}
	metadata, err := json.Marshal(observation.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), secret) {
		t.Fatalf("credential-like content leaked into metadata: %s", metadata)
	}
	if observation.Failure != nil && strings.Contains(observation.Failure.Error(), secret) {
		t.Fatal("credential-like content leaked into failure")
	}
}

func TestConcurrentExecutionGeneratesUniqueObservationIDs(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "file.txt", "content")
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	const executions = 32
	ids := make(chan string, executions)
	var group sync.WaitGroup
	for index := 0; index < executions; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			observation := registry.Execute(context.Background(), protocol.Action{
				Tool:      ToolReadFile,
				Arguments: arguments(`{"path":"file.txt"}`),
			})
			if !observation.Success {
				t.Errorf("concurrent observation = %#v", observation)
				return
			}
			ids <- observation.ID
		}()
	}
	group.Wait()
	close(ids)
	seen := make(map[string]struct{}, executions)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate observation ID %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != executions {
		t.Fatalf("got %d unique observation IDs, want %d", len(seen), executions)
	}
}

func TestRegistryRejectsNegativeLimits(t *testing.T) {
	_, err := NewRegistry(Options{Workspace: t.TempDir(), Limits: Limits{MaxReadBytes: -1}})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("NewRegistry() error = %v, want negative-limit error", err)
	}
}
