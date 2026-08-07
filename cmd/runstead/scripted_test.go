package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadScriptedResponsesReplaysInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script.jsonl")
	content := "{\"text\":\"first\"}\n{\"text\":\"second\"}\n\n{\"text\":\"third\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	responses, err := loadScriptedResponses(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 3 {
		t.Fatalf("responses = %d, want 3", len(responses))
	}
	for index, want := range []string{"first", "second", "third"} {
		if responses[index].Text != want {
			t.Errorf("response %d = %q, want %q", index, responses[index].Text, want)
		}
	}
}

func TestLoadScriptedResponsesRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"empty file", "", "empty"},
		{"malformed json", "not json\n", "line 1"},
		{"missing text", "{\"other\":1}\n", "text must not be empty"},
		{"blank text", "{\"text\":\"  \"}\n", "text must not be empty"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "script.jsonl")
			if err := os.WriteFile(path, []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadScriptedResponses(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestLoadScriptedResponsesMissingFile(t *testing.T) {
	_, err := loadScriptedResponses(filepath.Join(t.TempDir(), "missing.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v, want unavailable", err)
	}
}
