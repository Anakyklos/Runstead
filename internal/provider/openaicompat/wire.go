package openaicompat

import (
	"encoding/json"
	"errors"
	"strings"
)

// chatCompletionRequest is the minimal OpenAI-compatible Chat Completions
// request this adapter sends. Streaming is disabled at the baseline; the
// adapter never negotiates capabilities it does not implement.
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// wireMessage is one Chat Completions message. The adapter preserves the
// Runstead prompt contract exactly as delivered in provider.Request.Prompt:
// the transcript is rendered upstream of this package and must not be
// redesigned here.
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var errResponseTooLarge = errors.New("response body exceeds configured limit")

// completionEnvelope accepts ONLY the minimal explicitly supported response
// shape: an object whose choices array contains at least one entry carrying a
// message object with a non-null content string. Everything else fails closed
// instead of being guessed into success.
type completionEnvelope struct {
	Choices []completionChoice `json:"choices"`
}

type completionChoice struct {
	Message *completionMessage `json:"message"`
}

type completionMessage struct {
	Content *string `json:"content"`
}

// decodeChatCompletionResponse parses raw body bytes fail-closed and returns
// only the assistant text. Extra endpoint fields (ids, usage, finish reasons)
// are ignored by design: exposing them would require a ResponseMetadata
// contract change (#79), not silent smuggling into sanitized fields.
func decodeChatCompletionResponse(body []byte) (string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	var envelope completionEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", &Error{Kind: ErrorMalformedResponse}
	}
	if envelope.Choices == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	if len(envelope.Choices) == 0 {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	first := envelope.Choices[0]
	if first.Message == nil || first.Message.Content == nil {
		return "", &Error{Kind: ErrorInvalidEnvelope}
	}
	text := *first.Message.Content
	if strings.TrimSpace(text) == "" {
		return "", &Error{Kind: ErrorEmptyResponse}
	}
	return text, nil
}
