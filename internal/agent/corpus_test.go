package agent_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/tools"
)

// TestLoopGoldenCorpusIsOwnedByIssueSeven freezes the runstead.protocol.v1
// envelope behavior the loop depends on. Every corpus fixture is parsed with
// the real read-only registry as catalog; accidental schema drift fails this
// test deterministically. The corpus lives in experiments/protocol and is
// shared with the protocol package tests; this test adds the loop-owned
// coverage (truncated, oversized, duplicate key, unknown field, unsupported
// version) on top of the shared corpus.
func TestLoopGoldenCorpusIsOwnedByIssueSeven(t *testing.T) {
	corpusDir := filepath.Join("..", "..", "experiments", "protocol", "fixtures", "corpus")
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := os.Open(filepath.Join(corpusDir, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()

	type expectation struct {
		kind       protocol.EnvelopeKind
		code       protocol.FailureCode
		accepted   bool
		executable bool
		mixed      bool
		tool       string
	}
	expected := map[string]expectation{
		"valid_action":        {kind: protocol.KindAction, accepted: true, executable: true, tool: "read_file"},
		"valid_final":         {kind: protocol.KindFinal, accepted: true},
		"mixed_prose_action":  {kind: protocol.KindAction, accepted: true, executable: true, mixed: true, tool: "list_files"},
		"malformed_json":      {kind: protocol.KindInvalid, code: protocol.FailureMalformedJSON},
		"invalid_schema":      {kind: protocol.KindAction, code: protocol.FailureInvalidActionSchema},
		"unknown_tool":        {kind: protocol.KindAction, code: protocol.FailureUnknownTool},
		"protocol_refusal":    {kind: protocol.KindInvalid, code: protocol.FailureProtocolRefusal},
		"unsupported_claim":   {kind: protocol.KindInvalid, code: protocol.FailureUnsupportedExecutionClaim},
		"repeated_action":     {kind: protocol.KindAction, accepted: true, executable: true, tool: "read_file"},
		"secret_response":     {kind: protocol.KindAction, accepted: true, executable: true, mixed: true, tool: "read_file"},
		"secret_json":         {kind: protocol.KindInvalid, code: protocol.FailureMissingEnvelope},
		"truncated":           {kind: protocol.KindInvalid, code: protocol.FailureUnclosedEnvelope},
		"duplicate_key":       {kind: protocol.KindInvalid, code: protocol.FailureMalformedJSON},
		"unknown_field":       {kind: protocol.KindAction, code: protocol.FailureInvalidActionSchema},
		"unsupported_version": {kind: protocol.KindAction, code: protocol.FailureUnsupportedProtocolVersion},
	}

	scanner := bufio.NewScanner(manifest)
	seen := make(map[string]bool)
	for scanner.Scan() {
		var entry struct {
			Case    string `json:"case"`
			Fixture string `json:"fixture"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("manifest entry: %v", err)
		}
		want, ok := expected[entry.Case]
		if !ok {
			t.Fatalf("missing loop expectation for corpus case %q", entry.Case)
		}
		seen[entry.Case] = true
		response, err := os.ReadFile(filepath.Join(corpusDir, entry.Fixture))
		if err != nil {
			t.Fatalf("case %q: %v", entry.Case, err)
		}
		t.Run(entry.Case, func(t *testing.T) {
			result := protocol.Parse(string(response), registry)
			assertEnvelope(t, result, want.kind, want.code, want.accepted, want.executable, want.mixed, want.tool)
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("loop expectation %q was not in manifest", name)
		}
	}
}

// TestLoopGoldenEnvelopeDriftCases covers the schema-drift fixtures the loop
// needs beyond the shared corpus: truncated, oversized, duplicate key, unknown
// field and unsupported version. Oversized input is generated at runtime
// because a fixture file larger than the parser limit would bloat the repo.
func TestLoopGoldenEnvelopeDriftCases(t *testing.T) {
	registry, err := tools.NewRegistry(tools.Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	action := `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`
	cases := []struct {
		name string
		text string
		kind protocol.EnvelopeKind
		code protocol.FailureCode
	}{
		{"truncated", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{}`, protocol.KindInvalid, protocol.FailureUnclosedEnvelope},
		{"duplicate_key", `<runstead_action>{"version":"runstead.protocol.v1","version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`, protocol.KindInvalid, protocol.FailureMalformedJSON},
		{"unknown_field", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a.txt"},"extra":true}</runstead_action>`, protocol.KindAction, protocol.FailureInvalidActionSchema},
		{"unsupported_version", `<runstead_action>{"version":"runstead.protocol.v99","tool":"read_file","arguments":{"path":"a.txt"}}</runstead_action>`, protocol.KindAction, protocol.FailureUnsupportedProtocolVersion},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := protocol.Parse(testCase.text, registry)
			assertEnvelope(t, result, testCase.kind, testCase.code, false, false, false, "")
		})
	}
	t.Run("oversized", func(t *testing.T) {
		result := protocol.Parse(action+strings.Repeat("x", 1<<20), registry)
		assertEnvelope(t, result, protocol.KindInvalid, protocol.FailureMalformedJSON, false, false, false, "")
	})
}

func assertEnvelope(t *testing.T, result protocol.Result, kind protocol.EnvelopeKind, code protocol.FailureCode, accepted, executable, mixed bool, tool string) {
	t.Helper()
	if result.Kind != kind || result.Accepted != accepted || result.Executable != executable || result.MixedProse != mixed {
		t.Fatalf("kind=%q accepted=%t executable=%t mixed=%t, want kind=%q accepted=%t executable=%t mixed=%t", result.Kind, result.Accepted, result.Executable, result.MixedProse, kind, accepted, executable, mixed)
	}
	if code == "" {
		if result.Failure != nil {
			t.Fatalf("failure code = %q, want none", result.Failure.Code)
		}
	} else if result.Failure == nil || result.Failure.Code != code {
		t.Fatalf("failure = %v, want %q", result.Failure, code)
	}
	if tool != "" && (result.Action == nil || result.Action.Tool != tool) {
		t.Fatalf("action tool = %#v, want %q", result.Action, tool)
	}
}
