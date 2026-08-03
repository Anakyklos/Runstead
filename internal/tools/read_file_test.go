package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestReadFileReturnsBoundedUntrustedUTF8Data(t *testing.T) {
	root := t.TempDir()
	content := "🙂abc"
	writeFixture(t, root, "file.txt", content)
	registry, err := NewRegistry(Options{
		Workspace: root,
		Limits:    Limits{MaxReadBytes: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	observation := registry.Execute(context.Background(), protocol.Action{
		Tool:      ToolReadFile,
		Arguments: arguments(`{"path":"./file.txt"}`),
	})
	data, ok := observation.Data.(FileData)
	if !ok || observation.Failure != nil || !observation.Success {
		t.Fatalf("observation = %#v, want successful FileData", observation)
	}
	if data.Path != "file.txt" || data.Content != "🙂" || !utf8.ValidString(data.Content) {
		t.Fatalf("file data = %#v, want normalized valid prefix", data)
	}
	if !observation.Truncated || observation.Metadata.BytesOriginal != int64(len(content)) || observation.Metadata.BytesReturned != int64(len(data.Content)) {
		t.Fatalf("truncation metadata = %#v, want explicit bounded output", observation)
	}
	if !observation.Metadata.Untrusted {
		t.Fatal("file content was not marked untrusted")
	}
}

func TestReadFileRejectsBinaryInvalidAndNonRegularTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte("ok\x00bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.dat"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		code FailureCode
	}{
		{name: "binary", path: "binary.dat", code: FailureBinaryFile},
		{name: "invalid utf8", path: "invalid.dat", code: FailureInvalidUTF8},
		{name: "directory", path: "directory", code: FailureWrongType},
		{name: "missing", path: "missing.dat", code: FailurePathNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observation := registry.Execute(context.Background(), protocol.Action{
				Tool:      ToolReadFile,
				Arguments: arguments(`{"path":"` + testCase.path + `"}`),
			})
			if observation.Success || observation.Failure == nil || observation.Failure.Code != testCase.code {
				t.Fatalf("observation = %#v, want failure %q", observation, testCase.code)
			}
		})
	}
}
