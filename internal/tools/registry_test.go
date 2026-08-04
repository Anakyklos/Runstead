package tools

import (
	"encoding/json"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

var _ protocol.ToolCatalog = (*Registry)(nil)

func TestRegistryValidatesTheStaticReadOnlyCatalog(t *testing.T) {
	registry, err := NewRegistry(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		tool       string
		arguments  protocol.Arguments
		registered bool
		wantErr    FailureCode
	}{
		{name: "read file", tool: "read_file", arguments: arguments(`{"path":"README.md"}`), registered: true},
		{name: "list files", tool: "list_files", arguments: arguments(`{"path":"."}`), registered: true},
		{name: "search text", tool: "search_text", arguments: arguments(`{"query":"needle","path":"."}`), registered: true},
		{name: "git status", tool: "git_status", arguments: arguments(`{}`), registered: true},
		{name: "git diff", tool: "git_diff", arguments: arguments(`{}`), registered: true},
		{name: "unknown tool", tool: "shell", arguments: arguments(`{}`), registered: false},
		{name: "missing path", tool: "read_file", arguments: arguments(`{}`), registered: true, wantErr: FailureInvalidArguments},
		{name: "unexpected field", tool: "git_status", arguments: arguments(`{"path":"."}`), registered: true, wantErr: FailureInvalidArguments},
		{name: "wrong path type", tool: "list_files", arguments: arguments(`{"path":42}`), registered: true, wantErr: FailureInvalidArguments},
		{name: "missing search query", tool: "search_text", arguments: arguments(`{"path":"."}`), registered: true, wantErr: FailureInvalidArguments},
		{name: "absolute path", tool: "read_file", arguments: arguments(`{"path":"/etc/passwd"}`), registered: true, wantErr: FailureAbsolutePath},
		{name: "traversal", tool: "read_file", arguments: arguments(`{"path":"../outside"}`), registered: true, wantErr: FailurePathTraversal},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registered, err := registry.ValidateArguments(testCase.tool, testCase.arguments)
			if registered != testCase.registered {
				t.Fatalf("registered = %t, want %t", registered, testCase.registered)
			}
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateArguments() error = %v, want nil", err)
				}
				return
			}
			failure, ok := err.(*Failure)
			if !ok || failure.Code != testCase.wantErr {
				t.Fatalf("ValidateArguments() error = %#v, want *Failure{%q}", err, testCase.wantErr)
			}
		})
	}
}

func arguments(value string) protocol.Arguments {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		panic(err)
	}
	return protocol.Arguments(object)
}
