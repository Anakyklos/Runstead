package googlecompat

import (
	"encoding/json"
	"errors"
	"strings"
)

// generateRequest is the minimal Google Gemini generateContent-style request
// this adapter sends. Baseline is text-only: no streaming, no tools, no
// systemInstruction, no generationConfig, no safetySettings. The exact
// configured model travels in the URL resource path
// (models/{exact-model}:generateContent), which is where the official
// generateContent wire carries it; the body carries only the prompt through
// contents[].parts[].text.
type generateRequest struct {
	Contents []wireContent `json:"contents"`
}

// wireContent is one generateContent-style content entry. The adapter
// preserves the Runstead prompt contract exactly as delivered in
// provider.Request.Prompt: the transcript is rendered upstream of this
// package and must not be redesigned here. A single explicit user content
// with one text part is the only input form the baseline sends.
type wireContent struct {
	Role  string     `json:"role"`
	Parts []wirePart `json:"parts"`
}

// wirePart is ONE request part. Only text parts exist in the baseline.
type wirePart struct {
	Text string `json:"text"`
}

var errResponseTooLarge = errors.New("response body exceeds configured limit")

// generateResponse accepts ONLY the minimal explicitly supported response
// shape: a generateContent-style object whose candidates array carries at
// least one candidate with a text-only content. Everything else fails closed
// instead of being guessed into success.
type generateResponse struct {
	Candidates     []generateCandidate `json:"candidates"`
	PromptFeedback *promptFeedback     `json:"promptFeedback"`
}

// generateCandidate is ONE response candidate. Only the first candidate is
// consumed, deterministically, mirroring how the other family adapters treat
// the primary output slot; extra candidates are ignored by design.
type generateCandidate struct {
	Content      *candidateContent `json:"content"`
	FinishReason string            `json:"finishReason"`
}

// candidateContent is the candidate's content object. Only text parts are
// supported; anything else fails closed because the adapter cannot prove or
// consume it as a text turn and must never reinterpret it as Runstead task
// truth.
type candidateContent struct {
	Parts []part `json:"parts"`
}

// part is ONE response part. Text is a pointer so its presence is provable:
// a part that carries no text key at all (for example inlineData, fileData
// or thought content) is structurally NOT a text part. FunctionCall is a
// pointer to RawMessage so only a REAL functionCall object counts; an absent
// or null field stays nil, while any present object is typed evidence that
// the candidate is a tool-shape response, which this baseline (native tools
// disabled) can never reinterpret as text. Thought is the semantic
// GenerateContent flag (#97 review): when explicitly true, the part carries
// reasoning/summary content that the baseline does not support and must never
// enter provider.Response.Text. The pointer distinguishes absent (normal
// text part) from explicit false (still a normal text part) from explicit
// true (thought part, fail closed). thoughtSignature and other unused
// metadata stay ignored by design and outside task truth; this is not
// thinking support.
type part struct {
	Text         *string          `json:"text"`
	FunctionCall *json.RawMessage `json:"functionCall"`
	Thought      *bool            `json:"thought"`
}

// promptFeedback is the typed prompt-block signal
// (promptFeedback.blockReason). It makes a blocked prompt provable from
// structure, never from parsing free text.
type promptFeedback struct {
	BlockReason string `json:"blockReason"`
}

// decodeGenerateResponse parses raw body bytes fail-closed and returns only
// the assistant text plus a completeness decision derived from the typed
// finishReason and promptFeedback fields. Extra endpoint fields (token counts,
// safetyRatings, citationMetadata, modelVersion) are ignored by design:
// exposing them would require a ResponseMetadata contract change (#79), not
// silent smuggling into sanitized fields.
//
// Classification contract:
//   - promptFeedback.blockReason present: the prompt was blocked; a typed
//     refusal/safety outcome, never an empty response and never text.
//   - finishReason "STOP": the natural complete generation; text is returned.
//   - finishReason "MAX_TOKENS": the generation hit its token limit; the turn
//     is TRUNCATED and must never be treated as a complete
//     runstead.protocol.v1 turn.
//   - finishReason "SAFETY"/"RECITATION"/"BLOCKLIST"/"PROHIBITED_CONTENT"/
//     "LANGUAGE"/"SPII": candidate-level safety/refusal states, classified
//     from typed structure; any accompanying text stays off the wire.
//   - finishReason "FUNCTION_CALL"/"MALFORMED_FUNCTION_CALL": tool-shape
//     finishes; with native tools disabled the response format is unsupported.
//   - missing/unknown finishReason: the completeness of the turn cannot be
//     proven; invalid envelope.
func decodeGenerateResponse(body []byte) (string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var envelope generateResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", &Error{Kind: ErrorMalformedResponse}
	}
	if envelope.PromptFeedback != nil && envelope.PromptFeedback.BlockReason != "" {
		// A blocked prompt is typed refusal/safety evidence, never an empty
		// response and never a retry candidate.
		return "", &Error{Kind: ErrorRefusal}
	}
	if envelope.Candidates == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	if len(envelope.Candidates) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	first := envelope.Candidates[0]
	if first.Content == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	switch first.FinishReason {
	case "STOP":
		// Supported path: continue below.
	case "MAX_TOKENS":
		return "", &Error{Kind: ErrorIncompleteCompletion}
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "LANGUAGE", "SPII":
		return "", &Error{Kind: ErrorRefusal}
	case "FUNCTION_CALL", "MALFORMED_FUNCTION_CALL":
		return "", &Error{Kind: ErrorUnsupportedResponseFormat}
	default:
		// Empty, missing or unrecognized finish reason: the completeness of the
		// turn cannot be proven, so it fails closed as an invalid envelope.
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	if first.Content.Parts == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	if len(first.Content.Parts) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var text strings.Builder
	for _, candidatePart := range first.Content.Parts {
		if candidatePart.FunctionCall != nil {
			// A functionCall part (or a part carrying one) can never be
			// interpreted as text: the response as a whole is not a supported
			// text-only turn.
			return "", &Error{Kind: ErrorUnsupportedResponseFormat}
		}
		if candidatePart.Thought != nil && *candidatePart.Thought {
			// A thought part carries reasoning/summary content, never the
			// model's normal completion text. The baseline does not support
			// thought summaries, so this fails closed instead of
			// reinterpreting unknown wire semantics as Runstead task truth.
			// thoughtSignature and other unused metadata never leave this
			// package.
			return "", &Error{Kind: ErrorUnsupportedResponseFormat}
		}
		if candidatePart.Text == nil {
			// The part carries no text key at all: it is not a text part, so
			// the response cannot be proven as a text turn.
			return "", &Error{Kind: ErrorUnsupportedResponseFormat}
		}
		text.WriteString(*candidatePart.Text)
	}
	result := text.String()
	if strings.TrimSpace(result) == "" {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	return result, nil
}

// encodeGenerateRequest renders the minimal wire payload. The exact model is
// carried by the URL resource path (models/{exact-model}:generateContent);
// the body carries only the rendered prompt as a single explicit user content
// with one text part, deterministically.
func encodeGenerateRequest(prompt string) ([]byte, error) {
	payload, err := json.Marshal(generateRequest{
		Contents: []wireContent{{Role: "user", Parts: []wirePart{{Text: prompt}}}},
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}
