package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
)

const (
	maxResponseBytes = 1 << 20
	maxJSONDepth     = 128
)

type EnvelopeKind string

const (
	KindInvalid EnvelopeKind = "invalid"
	KindAction  EnvelopeKind = "action"
	KindFinal   EnvelopeKind = "final"
)

type Arguments map[string]json.RawMessage

type Action struct {
	Version   Version
	Tool      string
	Arguments Arguments
}

type FinalStatus string

const (
	StatusComplete   FinalStatus = "complete"
	StatusIncomplete FinalStatus = "incomplete"
)

// EvidenceCitation is one typed evidence citation in a final response: the
// model must declare the tool that produced the evidence it cites. The
// verifier checks the declared tool against the persisted evidence row; a
// fabricated, foreign or type-incompatible citation is rejected (issue #11).
type EvidenceCitation struct {
	// EvidenceID is the cited obs- identifier.
	EvidenceID string `json:"evidence_id"`
	// Tool is the tool the model claims produced the evidence. It must match
	// the persisted tool of the evidence row.
	Tool string `json:"tool"`
}

type FinalResponse struct {
	Version  Version
	Status   FinalStatus
	Summary  string
	Evidence []EvidenceCitation
}

type FailureCode string

const (
	FailureMissingEnvelope            FailureCode = "missing_envelope"
	FailureProtocolRefusal            FailureCode = "protocol_refusal"
	FailureUnsupportedExecutionClaim  FailureCode = "unsupported_execution_claim"
	FailureMultipleEnvelopes          FailureCode = "multiple_envelopes"
	FailureUnclosedEnvelope           FailureCode = "unclosed_envelope"
	FailureMalformedJSON              FailureCode = "malformed_json"
	FailureInvalidActionSchema        FailureCode = "invalid_action_schema"
	FailureInvalidFinalSchema         FailureCode = "invalid_final_schema"
	FailureUnsupportedProtocolVersion FailureCode = "unsupported_protocol_version"
	FailureUnknownTool                FailureCode = "unknown_tool"
	FailureInvalidArguments           FailureCode = "invalid_arguments"
	FailureRepeatedAction             FailureCode = "repeated_action"
)

type ParseFailure struct {
	Code                 FailureCode
	CorrectionReasonable bool
}

func (f ParseFailure) Error() string {
	return string(f.Code)
}

type Result struct {
	Kind        EnvelopeKind
	Action      *Action
	Final       *FinalResponse
	Failure     *ParseFailure
	MixedProse  bool
	SchemaValid bool
	Accepted    bool
	Executable  bool
}

type ToolCatalog interface {
	ValidateArguments(tool string, arguments Arguments) (registered bool, err error)
}

func Parse(response string, catalog ToolCatalog) Result {
	if len(response) > maxResponseBytes {
		return failedResult(KindInvalid, nil, false, false, FailureMalformedJSON)
	}
	kind, block, mixedProse, code := extractEnvelope(response)
	if code != "" {
		return failedResult(kind, nil, mixedProse, false, code)
	}

	raw, err := decodeSingleJSON(block)
	if err != nil {
		return failedResult(KindInvalid, nil, mixedProse, false, FailureMalformedJSON)
	}
	if kind == KindAction {
		return parseAction(raw, mixedProse, catalog)
	}
	return parseFinal(raw, mixedProse)
}

func parseAction(raw json.RawMessage, mixedProse bool, catalog ToolCatalog) Result {
	var envelope struct {
		Version   *Version        `json:"version"`
		Tool      *string         `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeStrict(raw, &envelope); err != nil || envelope.Version == nil || envelope.Tool == nil || envelope.Arguments == nil {
		return failedResult(KindAction, nil, mixedProse, false, FailureInvalidActionSchema)
	}
	if *envelope.Version != Current {
		return failedResult(KindAction, nil, mixedProse, false, FailureUnsupportedProtocolVersion)
	}
	if *envelope.Tool == "" {
		return failedResult(KindAction, nil, mixedProse, false, FailureInvalidActionSchema)
	}

	arguments, err := decodeArguments(envelope.Arguments)
	if err != nil {
		return failedResult(KindAction, nil, mixedProse, false, FailureInvalidActionSchema)
	}
	action := &Action{Version: *envelope.Version, Tool: *envelope.Tool, Arguments: arguments}
	if catalog == nil {
		return failedResult(KindAction, action, mixedProse, true, FailureUnknownTool)
	}
	registered, err := catalog.ValidateArguments(action.Tool, action.Arguments)
	if !registered {
		return failedResult(KindAction, action, mixedProse, true, FailureUnknownTool)
	}
	if err != nil {
		return failedResult(KindAction, action, mixedProse, true, FailureInvalidArguments)
	}
	return Result{
		Kind:        KindAction,
		Action:      action,
		MixedProse:  mixedProse,
		SchemaValid: true,
		Accepted:    true,
		Executable:  true,
	}
}

func parseFinal(raw json.RawMessage, mixedProse bool) Result {
	var envelope struct {
		Version  *Version        `json:"version"`
		Status   *FinalStatus    `json:"status"`
		Summary  *string         `json:"summary"`
		Evidence json.RawMessage `json:"evidence"`
	}
	if err := decodeStrict(raw, &envelope); err != nil || envelope.Version == nil || envelope.Status == nil || envelope.Summary == nil || envelope.Evidence == nil {
		return failedResult(KindFinal, nil, mixedProse, false, FailureInvalidFinalSchema)
	}
	if *envelope.Version != Current {
		return failedResult(KindFinal, nil, mixedProse, false, FailureUnsupportedProtocolVersion)
	}
	if *envelope.Status != StatusComplete && *envelope.Status != StatusIncomplete {
		return failedResult(KindFinal, nil, mixedProse, false, FailureInvalidFinalSchema)
	}
	evidence, err := decodeEvidence(envelope.Evidence)
	if err != nil {
		return failedResult(KindFinal, nil, mixedProse, false, FailureInvalidFinalSchema)
	}
	return Result{
		Kind:        KindFinal,
		Final:       &FinalResponse{Version: *envelope.Version, Status: *envelope.Status, Summary: *envelope.Summary, Evidence: evidence},
		MixedProse:  mixedProse,
		SchemaValid: true,
		Accepted:    true,
	}
}

func failedResult(kind EnvelopeKind, action *Action, mixedProse, schemaValid bool, code FailureCode) Result {
	return Result{
		Kind:        kind,
		Action:      action,
		MixedProse:  mixedProse,
		SchemaValid: schemaValid,
		Failure:     &ParseFailure{Code: code, CorrectionReasonable: true},
	}
}

type envelopeTag struct {
	kind    EnvelopeKind
	opening bool
	start   int
	end     int
}

var envelopeMarkers = []struct {
	text    string
	kind    EnvelopeKind
	opening bool
}{
	{text: "<runstead_action>", kind: KindAction, opening: true},
	{text: "</runstead_action>", kind: KindAction},
	{text: "<runstead_final>", kind: KindFinal, opening: true},
	{text: "</runstead_final>", kind: KindFinal},
}

func scanEnvelopeTags(response string) []envelopeTag {
	var tags []envelopeTag
	inEnvelope := false
	inJSONString := false
	escaped := false
	for offset := 0; offset < len(response); {
		if inEnvelope && inJSONString {
			if escaped {
				escaped = false
			} else if response[offset] == '\\' {
				escaped = true
			} else if response[offset] == '"' {
				inJSONString = false
			}
			offset++
			continue
		}

		marker, ok := markerAt(response, offset)
		if ok {
			tags = append(tags, envelopeTag{
				kind:    marker.kind,
				opening: marker.opening,
				start:   offset,
				end:     offset + len(marker.text),
			})
			offset += len(marker.text)
			if marker.opening {
				inEnvelope = true
			} else {
				inEnvelope = false
				inJSONString = false
				escaped = false
			}
			continue
		}

		if inEnvelope && response[offset] == '"' {
			inJSONString = true
		}
		offset++
	}
	return tags
}

func markerAt(response string, offset int) (struct {
	text    string
	kind    EnvelopeKind
	opening bool
}, bool) {
	for _, marker := range envelopeMarkers {
		if strings.HasPrefix(response[offset:], marker.text) {
			return marker, true
		}
	}
	return struct {
		text    string
		kind    EnvelopeKind
		opening bool
	}{}, false
}

func extractEnvelope(response string) (EnvelopeKind, string, bool, FailureCode) {
	tags := scanEnvelopeTags(response)
	if len(tags) == 0 {
		return KindInvalid, "", false, classifyMissingEnvelope(response)
	}

	openCount := 0
	for _, tag := range tags {
		if tag.opening {
			openCount++
		}
	}
	if openCount == 0 {
		return KindInvalid, "", false, FailureUnclosedEnvelope
	}
	if openCount > 1 {
		return KindInvalid, "", false, FailureMultipleEnvelopes
	}
	if !tags[0].opening {
		return KindInvalid, "", false, FailureUnclosedEnvelope
	}
	if len(tags) != 2 {
		return KindInvalid, "", false, FailureUnclosedEnvelope
	}
	if tags[1].opening || tags[0].kind != tags[1].kind {
		return KindInvalid, "", false, FailureMultipleEnvelopes
	}

	outside := response[:tags[0].start] + response[tags[1].end:]
	return tags[0].kind, response[tags[0].end:tags[1].start], strings.TrimSpace(outside) != "", ""
}

var (
	refusalPattern = regexp.MustCompile(`(?i)(^|[[:space:]])(I|we) (cannot|can't|am unable|refuse|won't|do not have access|don't have access)[^.!?\n]*(\b(local|files?|repository|workspace|paths?|directories|folders|tests?|commands?|scripts?|execute|read|list)\b)`)
	claimPattern   = regexp.MustCompile(`(?i)((I (have )?(read|listed|inspected|checked)|successfully (read|listed))[^.!?\n]*(\b(local|files?|repository|workspace|paths?|directories|folders)\b|README[.]md|[.]go\b)|(I (have )?(ran|executed)|successfully (ran|executed))[^.!?\n]*(\b(local|tests?|commands?|scripts?|repository|workspace|files?)\b)|the file [^.!?\n]* contains)`)
)

func classifyMissingEnvelope(response string) FailureCode {
	if refusalPattern.MatchString(response) {
		return FailureProtocolRefusal
	}
	if claimPattern.MatchString(response) {
		return FailureUnsupportedExecutionClaim
	}
	return FailureMissingEnvelope
}

func decodeSingleJSON(block string) (json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(block))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	if err := rejectDuplicateObjectKeys(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func decodeStrict(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func decodeArguments(raw json.RawMessage) (Arguments, error) {
	var arguments Arguments
	if err := json.Unmarshal(raw, &arguments); err != nil || arguments == nil {
		return nil, errors.New("arguments must be an object")
	}
	return arguments, nil
}

// decodeEvidence decodes the final envelope's evidence array strictly. Every
// entry must be an object with exactly evidence_id and tool (both non-empty
// strings); a string, number, missing or unknown field is rejected. The typed
// citation is the model's claim about the evidence, which the verifier checks
// against the persisted evidence row (issue #11).
func decodeEvidence(raw json.RawMessage) ([]EvidenceCitation, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return nil, errors.New("evidence must be a non-empty array")
	}
	citations := make([]EvidenceCitation, len(values))
	for index, value := range values {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var citation EvidenceCitation
		if err := decoder.Decode(&citation); err != nil {
			return nil, errors.New("evidence entries must be objects with evidence_id and tool")
		}
		if strings.TrimSpace(citation.EvidenceID) == "" || strings.TrimSpace(citation.Tool) == "" {
			return nil, errors.New("evidence entries require non-empty evidence_id and tool")
		}
		citations[index] = citation
	}
	return citations, nil
}

func rejectDuplicateObjectKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxJSONDepth {
		return errors.New("JSON nesting is too deep")
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return errors.New("duplicate object key")
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	}
	return nil
}

func ActionFingerprint(action Action) string {
	arguments, err := json.Marshal(action.Arguments)
	if err != nil {
		arguments = []byte("null")
	}
	canonicalArguments, err := canonicalJSON(arguments)
	if err != nil {
		canonicalArguments = arguments
	}
	payload, err := json.Marshal(struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}{Tool: action.Tool, Arguments: canonicalArguments})
	if err != nil {
		payload = []byte(action.Tool)
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}

type RepeatGuard struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func NewRepeatGuard() *RepeatGuard {
	return &RepeatGuard{seen: make(map[string]struct{})}
}

func (g *RepeatGuard) Seen(action Action) bool {
	return g.Check(action) != nil
}

func (g *RepeatGuard) Check(action Action) *ParseFailure {
	if g == nil {
		return nil
	}
	fingerprint := ActionFingerprint(action)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = make(map[string]struct{})
	}
	if _, exists := g.seen[fingerprint]; exists {
		return &ParseFailure{Code: FailureRepeatedAction, CorrectionReasonable: true}
	}
	g.seen[fingerprint] = struct{}{}
	return nil
}

func GenerateCorrectionMessage(code FailureCode, retriesRemaining int) (string, error) {
	if code == "" {
		return "", errors.New("failure code is required")
	}
	if retriesRemaining < 0 {
		return "", errors.New("retries_remaining must not be negative")
	}
	message := struct {
		ProtocolVersion  Version     `json:"protocol_version"`
		Type             string      `json:"type"`
		OK               bool        `json:"ok"`
		ErrorCode        FailureCode `json:"error_code"`
		RetriesRemaining int         `json:"retries_remaining"`
		Required         string      `json:"required"`
	}{
		ProtocolVersion:  Current,
		Type:             "protocol_correction",
		OK:               false,
		ErrorCode:        code,
		RetriesRemaining: retriesRemaining,
		Required:         correctionGuidance(code),
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
