package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureCatalog struct{}

func (fixtureCatalog) ValidateArguments(tool string, arguments Arguments) (bool, error) {
	if tool != "read_file" && tool != "list_files" {
		return false, nil
	}
	if len(arguments) != 1 {
		return true, errors.New("path is the only supported argument")
	}
	path, ok := arguments["path"]
	if !ok {
		return true, errors.New("path is required")
	}
	var value string
	if err := json.Unmarshal(path, &value); err != nil || value == "" {
		return true, errors.New("path must be a non-empty string")
	}
	return true, nil
}

func TestParseValidAction(t *testing.T) {
	result := Parse(`<runstead_action>
{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}
</runstead_action>`, fixtureCatalog{})

	if result.Failure != nil || !result.Accepted || !result.Executable {
		t.Fatalf("result = %#v, want accepted executable action", result)
	}
	if result.Kind != KindAction || result.Action == nil || result.Final != nil {
		t.Fatalf("result outputs = %#v, want one action", result)
	}
	if result.Action.Version != Current || result.Action.Tool != "read_file" {
		t.Fatalf("action = %#v", result.Action)
	}
	if got := string(result.Action.Arguments["path"]); got != `"README.md"` {
		t.Fatalf("path argument = %s, want %s", got, `"README.md"`)
	}
	if !result.SchemaValid || result.MixedProse {
		t.Fatalf("result flags = %#v, want schema-valid without mixed prose", result)
	}
}

func TestParseValidFinal(t *testing.T) {
	result := Parse(`<runstead_final>
{"version":"runstead.protocol.v1","status":"incomplete","summary":"still working","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}
</runstead_final>`, fixtureCatalog{})

	if result.Failure != nil || !result.Accepted || result.Executable {
		t.Fatalf("result = %#v, want accepted non-executable final", result)
	}
	if result.Kind != KindFinal || result.Action != nil || result.Final == nil {
		t.Fatalf("result outputs = %#v, want one final response", result)
	}
	if result.Final.Version != Current || result.Final.Status != StatusIncomplete || result.Final.Summary != "still working" {
		t.Fatalf("final = %#v", result.Final)
	}
	if len(result.Final.Evidence) != 1 || result.Final.Evidence[0].EvidenceID != "obs-000001" || result.Final.Evidence[0].Tool != "read_file" {
		t.Fatalf("evidence = %#v", result.Final.Evidence)
	}
}

func TestEnvelopeMarkersInsideJSONStringsAreContent(t *testing.T) {
	final := Parse(`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"literal <runstead_action> text","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`, fixtureCatalog{})
	if final.Failure != nil || !final.Accepted || final.Final == nil {
		t.Fatalf("final = %#v, want accepted final", final)
	}

	action := Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"literal <runstead_final> text </runstead_action>"}}</runstead_action>`, fixtureCatalog{})
	if action.Failure != nil || !action.Accepted || action.Action == nil {
		t.Fatalf("action = %#v, want accepted action", action)
	}
}

func TestCorpusFixtures(t *testing.T) {
	manifest, err := os.Open(filepath.Join("..", "..", "experiments", "protocol", "fixtures", "corpus", "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()

	expected := map[string]struct {
		code       FailureCode
		kind       EnvelopeKind
		accepted   bool
		executable bool
		mixed      bool
		tool       string
	}{
		"valid_action":        {kind: KindAction, accepted: true, executable: true, tool: "read_file"},
		"valid_final":         {kind: KindFinal, accepted: true},
		"mixed_prose_action":  {kind: KindAction, accepted: true, executable: true, mixed: true, tool: "list_files"},
		"malformed_json":      {code: FailureMalformedJSON, kind: KindInvalid},
		"invalid_schema":      {code: FailureInvalidActionSchema, kind: KindAction},
		"unknown_tool":        {code: FailureUnknownTool, kind: KindAction},
		"protocol_refusal":    {code: FailureProtocolRefusal, kind: KindInvalid},
		"unsupported_claim":   {code: FailureUnsupportedExecutionClaim, kind: KindInvalid},
		"repeated_action":     {kind: KindAction, accepted: true, executable: true, tool: "read_file"},
		"secret_response":     {kind: KindAction, accepted: true, executable: true, mixed: true, tool: "read_file"},
		"secret_json":         {code: FailureMissingEnvelope, kind: KindInvalid},
		"truncated":           {code: FailureUnclosedEnvelope, kind: KindInvalid},
		"duplicate_key":       {code: FailureMalformedJSON, kind: KindInvalid},
		"unknown_field":       {code: FailureInvalidActionSchema, kind: KindAction},
		"unsupported_version": {code: FailureUnsupportedProtocolVersion, kind: KindAction},
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
			t.Fatalf("missing expectation for corpus case %q", entry.Case)
		}
		seen[entry.Case] = true
		response, err := os.ReadFile(filepath.Join("..", "..", "experiments", "protocol", "fixtures", "corpus", entry.Fixture))
		if err != nil {
			t.Fatalf("case %q: %v", entry.Case, err)
		}

		t.Run(entry.Case, func(t *testing.T) {
			result := Parse(string(response), fixtureCatalog{})
			if result.Kind != want.kind || result.Accepted != want.accepted || result.Executable != want.executable || result.MixedProse != want.mixed {
				t.Fatalf("kind=%q accepted=%t executable=%t mixed=%t, want kind=%q accepted=%t executable=%t mixed=%t", result.Kind, result.Accepted, result.Executable, result.MixedProse, want.kind, want.accepted, want.executable, want.mixed)
			}
			if want.code == "" {
				if result.Failure != nil {
					t.Fatalf("failure code = %q, want no failure", result.Failure.Code)
				}
			} else if result.Failure == nil || result.Failure.Code != want.code {
				t.Fatalf("failure code = %v, want %q", result.Failure, want.code)
			}
			if want.tool != "" && (result.Action == nil || result.Action.Tool != want.tool) {
				t.Fatalf("action tool = %#v, want %q", result.Action, want.tool)
			}
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range expected {
		if !seen[name] {
			t.Errorf("corpus expectation %q was not in manifest", name)
		}
	}
}

func TestParseFocusedFailures(t *testing.T) {
	cases := []struct {
		name string
		text string
		code FailureCode
		kind EnvelopeKind
	}{
		{"truncated_json", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{}</runstead_action>`, FailureMalformedJSON, KindInvalid},
		{"closing_without_opening", `</runstead_action>`, FailureUnclosedEnvelope, KindInvalid},
		{"opening_without_closing", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{}}`, FailureUnclosedEnvelope, KindInvalid},
		{"multiple_action_envelopes", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}}</runstead_action><runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"b"}}</runstead_action>`, FailureMultipleEnvelopes, KindInvalid},
		{"multiple_final_envelopes", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"a","evidence":[{"evidence_id":"a","tool":"read_file"}]}</runstead_final><runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"b","evidence":[{"evidence_id":"b","tool":"read_file"}]}</runstead_final>`, FailureMultipleEnvelopes, KindInvalid},
		{"mixed_action_and_final", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}}</runstead_action><runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"a","tool":"read_file"}]}</runstead_final>`, FailureMultipleEnvelopes, KindInvalid},
		{"duplicate_action_field", `<runstead_action>{"version":"runstead.protocol.v1","version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}}</runstead_action>`, FailureMalformedJSON, KindInvalid},
		{"duplicate_nested_argument_field", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a","options":{"mode":"safe","mode":"unsafe"}}}</runstead_action>`, FailureMalformedJSON, KindInvalid},
		{"trailing_json_value", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}} {"extra":true}</runstead_action>`, FailureMalformedJSON, KindInvalid},
		{"prose_inside_json", `<runstead_action>not json</runstead_action>`, FailureMalformedJSON, KindInvalid},
		{"wrong_protocol_version", `<runstead_action>{"version":"runstead.protocol.v0","tool":"read_file","arguments":{"path":"a"}}</runstead_action>`, FailureUnsupportedProtocolVersion, KindAction},
		{"empty_tool", `<runstead_action>{"version":"runstead.protocol.v1","tool":"","arguments":{"path":"a"}}</runstead_action>`, FailureInvalidActionSchema, KindAction},
		{"missing_arguments", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file"}</runstead_action>`, FailureInvalidActionSchema, KindAction},
		{"arguments_not_object", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":[]}</runstead_action>`, FailureInvalidActionSchema, KindAction},
		{"missing_required_argument", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{}}</runstead_action>`, FailureInvalidArguments, KindAction},
		{"unexpected_argument", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a","extra":"b"}}</runstead_action>`, FailureInvalidArguments, KindAction},
		{"invalid_argument_type", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":42}}</runstead_action>`, FailureInvalidArguments, KindAction},
		{"empty_evidence", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"string_evidence", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":["obs-000001"]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"non_object_evidence", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[1]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"evidence_missing_tool", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001"}]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"evidence_missing_id", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"tool":"read_file"}]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"evidence_unknown_field", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file","extra":1}]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"invalid_final_status", `<runstead_final>{"version":"runstead.protocol.v1","status":"done","summary":"done","evidence":[{"evidence_id":"a","tool":"read_file"}]}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
		{"unknown_action_field", `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"},"extra":true}</runstead_action>`, FailureInvalidActionSchema, KindAction},
		{"unknown_final_field", `<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"a","tool":"read_file"}],"extra":true}</runstead_final>`, FailureInvalidFinalSchema, KindFinal},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Parse(testCase.text, fixtureCatalog{})
			if result.Failure == nil || result.Failure.Code != testCase.code {
				t.Fatalf("failure = %v, want %q", result.Failure, testCase.code)
			}
			if result.Kind != testCase.kind || result.Accepted || result.Executable || result.Final != nil {
				t.Fatalf("result = %#v, want failed non-executable %q", result, testCase.kind)
			}
			if result.Action != nil && result.Final != nil {
				t.Fatal("result contains both action and final")
			}
		})
	}
}

func TestParseMissingEnvelopeClassifications(t *testing.T) {
	cases := []struct {
		name string
		text string
		code FailureCode
	}{
		{"ordinary prose", "Please inspect the repository.", FailureMissingEnvelope},
		{"protocol refusal", "I cannot access local files from this conversation.", FailureProtocolRefusal},
		{"unsupported execution claim", "I have read README.md and the file contains the expected direction.", FailureUnsupportedExecutionClaim},
		{"ordinary protocol commentary", "I checked the protocol description and need more context.", FailureMissingEnvelope},
		{"ordinary test commentary", "I checked the tests in the protocol description and need more context.", FailureMissingEnvelope},
		{"native tool commentary", "We can't use native tool calls here; return an envelope instead.", FailureMissingEnvelope},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Parse(testCase.text, fixtureCatalog{})
			if result.Failure == nil || result.Failure.Code != testCase.code {
				t.Fatalf("failure = %v, want %q", result.Failure, testCase.code)
			}
			if result.Action != nil || result.Final != nil || result.Executable {
				t.Fatalf("result = %#v, want no output", result)
			}
		})
	}
}

func TestParserDoesNotCallToolCatalogForInvalidStructure(t *testing.T) {
	calls := 0
	catalog := catalogFunc(func(tool string, arguments Arguments) (bool, error) {
		calls++
		return true, errors.New("invalid arguments")
	})

	result := Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{}}</runstead_action>`, catalog)
	if result.Failure == nil || result.Failure.Code != FailureInvalidArguments {
		t.Fatalf("failure = %v, want invalid arguments", result.Failure)
	}
	if calls != 1 {
		t.Fatalf("catalog calls = %d, want 1 for schema-valid action", calls)
	}

	result = Parse(`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":}</runstead_action>`, catalog)
	if result.Failure == nil || result.Failure.Code != FailureMalformedJSON {
		t.Fatalf("failure = %v, want malformed JSON", result.Failure)
	}
	if calls != 1 {
		t.Fatalf("catalog calls = %d, want no call for malformed JSON", calls)
	}
}

type catalogFunc func(string, Arguments) (bool, error)

func (f catalogFunc) ValidateArguments(tool string, arguments Arguments) (bool, error) {
	return f(tool, arguments)
}

func TestActionFingerprintCanonicalizesArgumentKeyOrder(t *testing.T) {
	first := Action{Version: Current, Tool: "read_file", Arguments: Arguments{
		"path":    json.RawMessage(`"README.md"`),
		"options": json.RawMessage(`{"encoding":"utf-8","line_endings":"lf"}`),
	}}
	second := Action{Version: Current, Tool: "read_file", Arguments: Arguments{
		"options": json.RawMessage(`{"line_endings":"lf","encoding":"utf-8"}`),
		"path":    json.RawMessage(`"README.md"`),
	}}
	if got, want := ActionFingerprint(first), ActionFingerprint(second); got != want {
		t.Fatalf("fingerprints differ for equivalent arguments: %s != %s", got, want)
	}

	cases := []Action{
		{Version: Current, Tool: "read_file", Arguments: Arguments{"path": json.RawMessage(`"other.md"`)}},
		{Version: Current, Tool: "list_files", Arguments: Arguments{"path": json.RawMessage(`"README.md"`)}},
	}
	for _, action := range cases {
		if ActionFingerprint(first) == ActionFingerprint(action) {
			t.Fatalf("action %#v was treated as a repeat", action)
		}
	}
}

func TestRepeatGuardIsCallerOwned(t *testing.T) {
	guard := NewRepeatGuard()
	action := Action{Version: Current, Tool: "read_file", Arguments: Arguments{"path": json.RawMessage(`"README.md"`)}}
	if failure := guard.Check(action); failure != nil {
		t.Fatalf("first action failure = %#v", failure)
	}
	if failure := guard.Check(action); failure == nil || failure.Code != FailureRepeatedAction {
		t.Fatalf("second action failure = %#v, want repeated_action", failure)
	}
	if !guard.Seen(action) {
		t.Fatal("Seen did not report the repeated action")
	}
}

func TestParseRejectsOversizedResponses(t *testing.T) {
	result := Parse(strings.Repeat("x", maxResponseBytes+1), fixtureCatalog{})
	if result.Failure == nil || result.Failure.Code != FailureMalformedJSON || result.Executable {
		t.Fatalf("result = %#v, want bounded malformed-json failure", result)
	}
}

func TestParseRejectsDeepJSON(t *testing.T) {
	response := `<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":` +
		strings.Repeat("[", maxJSONDepth+1) + "null" + strings.Repeat("]", maxJSONDepth+1) +
		`}}</runstead_action>`
	result := Parse(response, fixtureCatalog{})
	if result.Failure == nil || result.Failure.Code != FailureMalformedJSON || result.Executable {
		t.Fatalf("result = %#v, want bounded deep-json failure", result)
	}
}

func TestCorrectionMessageIsDeterministicAndRejectsNegativeRetries(t *testing.T) {
	first, err := GenerateCorrectionMessage(FailureMalformedJSON, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateCorrectionMessage(FailureMalformedJSON, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("correction messages differ: %q != %q", first, second)
	}
	var message map[string]any
	if err := json.Unmarshal([]byte(first), &message); err != nil {
		t.Fatal(err)
	}
	if message["protocol_version"] != string(Current) || message["type"] != "protocol_correction" || message["ok"] != false || message["error_code"] != string(FailureMalformedJSON) || message["retries_remaining"] != float64(1) {
		t.Fatalf("correction message = %s", first)
	}
	if !strings.Contains(first, "Return exactly one valid runstead_action or runstead_final envelope") {
		t.Fatalf("correction message missing required guidance: %s", first)
	}
	if _, err := GenerateCorrectionMessage(FailureMalformedJSON, -1); err == nil {
		t.Fatal("negative retries were accepted")
	}
}

func FuzzParseDoesNotPanicOrContradict(f *testing.F) {
	seeds := []string{
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_final>{"version":"runstead.protocol.v1","status":"complete","summary":"done","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}</runstead_final>`,
		`<runstead_action>{"version":}</runstead_action>`,
		`outside prose <runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}}</runstead_action><runstead_action>{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"a"}}</runstead_action>`,
		`<runstead_action>{"version":"runstead.protocol.v1"}`,
		`I cannot access local files from this conversation.`,
		`I have read README.md already.`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		result := Parse(input, fixtureCatalog{})
		if result.Action != nil && result.Final != nil {
			t.Fatal("result contains both action and final")
		}
		if result.Failure != nil && result.Executable {
			t.Fatal("failure reported executable output")
		}
		if result.Accepted && result.Failure != nil {
			t.Fatal("accepted result contains a failure")
		}
		if result.Executable && (result.Action == nil || result.Final != nil || result.Failure != nil || result.Action.Version != Current) {
			t.Fatal("executable result violates action invariants")
		}
		if result.Accepted && result.Final != nil && result.Final.Version != Current {
			t.Fatal("accepted final has unsupported protocol version")
		}
	})
}
