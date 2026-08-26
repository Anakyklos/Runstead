package anthropiccompat

import (
	"encoding/json"
	"errors"
	"strings"
)

// messagesRequest is the minimal Anthropic Messages-style request this adapter
// sends. Baseline is text-only: no streaming, no tools, no thinking, no system
// prompt, no stop sequences. The model travels with every request so a silently
// diverging model name is impossible; max_tokens is REQUIRED by the Messages
// wire and comes from the validated non-secret protocol options propagated via
// provider.Resolved (#88).
type messagesRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []wireMessage `json:"messages"`
	Stream    bool          `json:"stream"`
}

// wireMessage is one Messages-style message. The adapter preserves the
// Runstead prompt contract exactly as delivered in provider.Request.Prompt:
// the transcript is rendered upstream of this package and must not be
// redesigned here. A plain string content is the shorthand for one text block
// and is the only input form the baseline sends.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var errResponseTooLarge = errors.New("response body exceeds configured limit")

// messagesEnvelope accepts ONLY the minimal explicitly supported response
// shape: a Messages-style message object whose content array carries exactly
// the text blocks this adapter supports. Everything else fails closed instead
// of being guessed into success.
type messagesEnvelope struct {
	Content     []contentBlock `json:"content"`
	StopReason  string         `json:"stop_reason"`
	StopDetails *stopDetails   `json:"stop_details"`
}

// contentBlock is ONE response content block. Only type "text" is supported;
// any other block type (tool_use, thinking, redacted_thinking, server tools,
// ...) fails closed because the adapter cannot prove or consume it as a text
// turn and must never reinterpret it as Runstead task truth.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// stopDetails is the typed refusal signal (stop_details.type == "refusal").
// It makes refusal provable from structure, never from parsing free text.
type stopDetails struct {
	Type string `json:"type"`
}

// decodeMessagesResponse parses raw body bytes fail-closed and returns only
// the assistant text plus a completeness decision derived from the typed
// stop_reason. Extra endpoint fields (ids, usage, model, role) are ignored by
// design: exposing them would require a ResponseMetadata contract change
// (#79), not silent smuggling into sanitized fields.
//
// Classification contract:
//   - stop_reason "end_turn": the natural complete turn; text is returned.
//   - stop_reason "refusal" (or stop_details.type "refusal"): typed refusal.
//   - stop_reason "max_tokens": the generation hit its token limit; the turn
//     is TRUNCATED and must never be treated as a complete
//     runstead.protocol.v1 turn.
//   - stop_reason "tool_use": tool-use-only turn; unsupported format.
//   - stop_reason "stop_sequence"/"pause_turn"/"model_context_window_exceeded":
//     provably NOT a natural end; incomplete completion.
//   - missing/unknown stop_reason: the shape cannot be proven; invalid
//     envelope.
func decodeMessagesResponse(body []byte) (string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var envelope messagesEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", &Error{Kind: ErrorMalformedResponse}
	}
	if envelope.StopDetails != nil && envelope.StopDetails.Type == "refusal" {
		return "", &Error{Kind: ErrorRefusal}
	}
	switch envelope.StopReason {
	case "refusal":
		return "", &Error{Kind: ErrorRefusal}
	case "end_turn":
		// Supported path: continue below.
	case "max_tokens", "stop_sequence", "pause_turn", "model_context_window_exceeded":
		return "", &Error{Kind: ErrorIncompleteCompletion}
	case "tool_use":
		return "", &Error{Kind: ErrorUnsupportedResponseFormat}
	default:
		// Empty, missing or unrecognized stop reason: the completeness of the
		// turn cannot be proven, so it fails closed as an invalid envelope.
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}

	if envelope.Content == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	if len(envelope.Content) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var text strings.Builder
	for _, block := range envelope.Content {
		if block.Type != "text" {
			// A tool_use, thinking or other unsupported block can never be
			// interpreted as text. The response as a whole is not a supported
			// text-only turn.
			return "", &Error{Kind: ErrorUnsupportedResponseFormat}
		}
		text.WriteString(block.Text)
	}
	result := text.String()
	if strings.TrimSpace(result) == "" {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	return result, nil
}

// encodeMessagesRequest renders the minimal wire payload. maxTokens is the
// validated generation limit from the resolved protocol options; the exact
// configured model travels with every request.
func encodeMessagesRequest(model string, maxTokens int, prompt string) ([]byte, error) {
	payload, err := json.Marshal(messagesRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []wireMessage{{Role: "user", Content: prompt}},
		Stream:    false,
	})
	if err != nil {
		return nil, err
	}
	return payload, nil
}
