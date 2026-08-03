package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestSearchTextUsesFixedRGArgumentsAndReturnsStructuredMatches(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "a.txt", "needle\n")
	var gotExecutable string
	var gotArgs []string
	var gotDir string
	registry, err := NewRegistry(Options{
		Workspace: root,
		LookPath: func(name string) (string, error) {
			if name != "rg" {
				t.Fatalf("LookPath(%q), want rg", name)
			}
			return "/usr/bin/rg", nil
		},
		RunRG: func(_ context.Context, executable string, args []string, dir string) CommandResult {
			gotExecutable = executable
			gotArgs = append([]string(nil), args...)
			gotDir = dir
			return CommandResult{
				Stdout:   []byte(`{"type":"match","data":{"path":{"text":"a.txt"},"line_number":1,"lines":{"text":"needle\n"}}}` + "\n"),
				ExitCode: 0,
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"--needle","path":"."}`),
	})
	data, ok := observation.Data.(SearchData)
	if !ok || observation.Failure != nil || !observation.Success {
		t.Fatalf("observation = %#v, want successful SearchData", observation)
	}
	want := SearchData{Matches: []SearchMatch{{Path: "a.txt", Line: 1, Text: "needle"}}}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("search data = %#v, want %#v", data, want)
	}
	if gotExecutable != "/usr/bin/rg" || gotDir != root || !containsArgs(gotArgs, "--") || gotArgs[len(gotArgs)-2] != "--needle" || gotArgs[len(gotArgs)-1] != "." {
		t.Fatalf("rg invocation = %q %q in %q", gotExecutable, gotArgs, gotDir)
	}
	if observation.Metadata.Backend != "rg" || !observation.Metadata.Untrusted || observation.Metadata.ExitCode != 0 {
		t.Fatalf("search metadata = %#v", observation.Metadata)
	}
}

func TestSearchTextFallbackMatchesRGContractAndSkipsUnsafeText(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "z.txt", "needle\nnope\n")
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "nested"), "a.txt", "needle twice\n")
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte("needle\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.dat"), []byte("needle\xff"), 0o644); err != nil {
		t.Fatal(err)
	}
	rgCalled := false
	registry, err := NewRegistry(Options{
		Workspace: root,
		LookPath:  func(string) (string, error) { return "", errors.New("rg unavailable") },
		RunRG: func(context.Context, string, []string, string) CommandResult {
			rgCalled = true
			return CommandResult{ExitCode: 0}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"."}`),
	})
	data, ok := observation.Data.(SearchData)
	if !ok || observation.Failure != nil || !observation.Success {
		t.Fatalf("observation = %#v", observation)
	}
	if rgCalled {
		t.Fatal("fallback invoked rg")
	}
	want := []SearchMatch{
		{Path: "nested/a.txt", Line: 1, Text: "needle twice"},
		{Path: "z.txt", Line: 1, Text: "needle"},
	}
	if !reflect.DeepEqual(data.Matches, want) || data.SkippedBinaryFiles != 1 || data.SkippedInvalidUTF8 != 1 {
		t.Fatalf("fallback data = %#v, want matches and explicit skips", data)
	}
	if observation.Metadata.Backend != "fallback" || !observation.Metadata.Untrusted {
		t.Fatalf("fallback metadata = %#v", observation.Metadata)
	}
}

func TestSearchTextTruncatesMatchesAndDistinguishesNoMatchFromFailure(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "a.txt", "needle\nneedle\n")
	registry, err := NewRegistry(Options{
		Workspace: root,
		Limits:    Limits{MaxSearchMatches: 1},
		LookPath:  func(string) (string, error) { return "", errors.New("rg unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"a.txt"}`),
	})
	data := observation.Data.(SearchData)
	if len(data.Matches) != 1 || !observation.Truncated || observation.Metadata.MatchesOriginal != 2 || observation.Metadata.MatchesReturned != 1 {
		t.Fatalf("truncated search = %#v", observation)
	}

	noMatchRegistry, err := NewRegistry(Options{
		Workspace: root,
		LookPath:  func(string) (string, error) { return "/fake/rg", nil },
		RunRG: func(context.Context, string, []string, string) CommandResult {
			return CommandResult{ExitCode: 1, Err: errors.New("no matches")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	noMatch := noMatchRegistry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"absent","path":"a.txt"}`),
	})
	if !noMatch.Success || noMatch.Failure != nil || len(noMatch.Data.(SearchData).Matches) != 0 {
		t.Fatalf("no-match search = %#v", noMatch)
	}

	byteLimited, err := NewRegistry(Options{
		Workspace: root,
		Limits:    Limits{MaxSearchBytes: 5},
		LookPath:  func(string) (string, error) { return "", errors.New("rg unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	limited := byteLimited.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"a.txt"}`),
	})
	if !limited.Success || !limited.Truncated || len(limited.Data.(SearchData).Matches) != 0 || limited.Metadata.BytesOriginal <= limited.Metadata.BytesReturned {
		t.Fatalf("byte-limited search = %#v", limited)
	}
}

func TestSearchTextHonorsCancellationAndTimeout(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "a.txt", "needle")
	blockingRunner := func(ctx context.Context, _ string, _ []string, _ string) CommandResult {
		<-ctx.Done()
		return CommandResult{Err: ctx.Err(), ExitCode: -1}
	}
	registry, err := NewRegistry(Options{
		Workspace: root,
		Limits:    Limits{SearchTimeout: 5 * time.Millisecond},
		LookPath:  func(string) (string, error) { return "/fake/rg", nil },
		RunRG:     blockingRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	timeout := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"a.txt"}`),
	})
	if timeout.Failure == nil || timeout.Failure.Code != FailureTimeout {
		t.Fatalf("timeout = %#v", timeout)
	}

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := registry.Execute(canceledContext, protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"a.txt"}`),
	})
	if canceled.Failure == nil || canceled.Failure.Code != FailureCanceled {
		t.Fatalf("canceled = %#v", canceled)
	}
}

func TestSearchTextFallbackHonorsTimeout(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "a.txt", "needle\n")
	registry, err := NewRegistry(Options{
		Workspace: root,
		Limits:    Limits{SearchTimeout: time.Nanosecond},
		LookPath:  func(string) (string, error) { return "", errors.New("rg unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"a.txt"}`),
	})
	if observation.Failure == nil || observation.Failure.Code != FailureTimeout {
		t.Fatalf("fallback timeout = %#v", observation)
	}
}

func TestSearchTextRejectsExternalSymlinkPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFixture(t, filepath.Dir(outside), filepath.Base(outside), "needle")
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := NewRegistry(Options{Workspace: root, LookPath: func(string) (string, error) { return "", errors.New("rg unavailable") }})
	if err != nil {
		t.Fatal(err)
	}
	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolSearchText,
		Arguments: arguments(`{"query":"needle","path":"outside-link"}`),
	})
	if observation.Failure == nil || observation.Failure.Code != FailureSymlinkEscape {
		t.Fatalf("external symlink search = %#v", observation)
	}
}

func containsArgs(args []string, want string) bool {
	return strings.Contains("\x00"+strings.Join(args, "\x00")+"\x00", "\x00"+want+"\x00")
}
