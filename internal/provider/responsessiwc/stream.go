package responsessiwc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type delivery struct{ sent, response bool }

func (d *delivery) trace(_ func() time.Time) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { d.sent = true }, GotFirstResponseByte: func() { d.response = true }}
}
func (d *delivery) state() provider.DeliveryState {
	if d.response {
		return provider.DeliveryResponseStarted
	}
	if d.sent {
		return provider.DeliverySentUnconfirmed
	}
	// Do is entered only after one model request is constructed. If the
	// transport cannot prove whether it wrote the request, preserve ambiguity.
	return provider.DeliverySentUnconfirmed
}
func (d *delivery) stateAfterBody(ok bool) provider.DeliveryState {
	if ok {
		return provider.DeliveryCompleted
	}
	return provider.DeliveryResponseStarted
}

func parseSSE(body io.Reader, outputLimit int) (string, error) {
	limited := io.LimitReader(body, maxStream+1)
	r := bufio.NewReaderSize(limited, maxLine+1)
	var output strings.Builder
	var eventName string
	var data []byte
	var total, eventBytes int
	terminal := false
	completed := false
	for {
		line, err := r.ReadSlice('\n')
		if len(line) > maxLine {
			return "", &Error{Kind: ErrorResponseTooLarge}
		}
		total += len(line)
		eventBytes += len(line)
		if total > maxStream {
			return "", &Error{Kind: ErrorResponseTooLarge}
		}
		if eventBytes > maxEvent {
			return "", &Error{Kind: ErrorResponseTooLarge}
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if err == io.EOF && len(line) == 0 {
			if eventName != "" || len(data) > 0 {
				return "", &Error{Kind: ErrorStreamIncomplete}
			}
			break
		}
		if len(line) == 0 {
			if eventName != "" || len(data) > 0 {
				if terminal {
					return "", &Error{Kind: ErrorInvalidEnvelope}
				}
				name := eventName
				text, done, parseErr := consumeEvent(name, data, &output, outputLimit)
				_ = text
				if parseErr != nil {
					return "", parseErr
				}
				if done {
					terminal = true
					completed = name == "response.completed"
				}
			}
			eventName = ""
			data = data[:0]
			eventBytes = 0
		} else if line[0] != ':' {
			field, value, ok := bytes.Cut(line, []byte{':'})
			if !ok {
				field = line
				value = nil
			} else if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch string(field) {
			case "event":
				if eventName != "" || len(value) == 0 || len(value) > 128 {
					return "", &Error{Kind: ErrorInvalidEnvelope}
				}
				eventName = string(value)
			case "data":
				if len(data) > 0 {
					return "", &Error{Kind: ErrorInvalidEnvelope}
				}
				if len(value) > maxEvent {
					return "", &Error{Kind: ErrorResponseTooLarge}
				}
				data = append(data, value...)
			default:
				return "", &Error{Kind: ErrorInvalidEnvelope}
			}
		}
		if err == io.EOF {
			if eventName != "" || len(data) > 0 {
				return "", &Error{Kind: ErrorStreamIncomplete}
			}
			break
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return "", &Error{Kind: ErrorResponseTooLarge}
			}
			return "", &Error{Kind: ErrorStreamIncomplete, Cause: err}
		}
	}
	if !completed {
		return "", &Error{Kind: ErrorStreamIncomplete}
	}
	return output.String(), nil
}

func consumeEvent(event string, data []byte, output *strings.Builder, outputLimit int) (string, bool, error) {
	if event == "" || len(data) == 0 || string(data) == "[DONE]" {
		return "", false, &Error{Kind: ErrorMalformedResponse}
	}
	if rejectDuplicateJSONKeys(data) != nil {
		return "", false, &Error{Kind: ErrorMalformedResponse}
	}
	var envelope map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil {
		return "", false, &Error{Kind: ErrorMalformedResponse}
	}
	if dec.Decode(new(any)) != io.EOF {
		return "", false, &Error{Kind: ErrorMalformedResponse}
	}
	var typ string
	if raw, ok := envelope["type"]; !ok || json.Unmarshal(raw, &typ) != nil || typ != event {
		return "", false, &Error{Kind: ErrorInvalidEnvelope}
	}
	if raw, ok := envelope["sequence_number"]; ok {
		var sequence uint64
		if json.Unmarshal(raw, &sequence) != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
	}
	switch event {
	case "response.output_text.delta":
		if err := rejectUnknown(envelope, "type", "sequence_number", "delta", "item_id", "output_index", "content_index"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		var delta string
		if raw, ok := envelope["delta"]; !ok || json.Unmarshal(raw, &delta) != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if output.Len()+len(delta) > outputLimit {
			return "", false, &Error{Kind: ErrorResponseTooLarge}
		}
		output.WriteString(delta)
		return "", false, nil
	case "response.completed", "response.failed", "response.incomplete":
		if err := rejectUnknown(envelope, "type", "sequence_number", "response"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		var response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
			IncompleteDetails json.RawMessage `json:"incomplete_details"`
		}
		raw, ok := envelope["response"]
		if !ok || json.Unmarshal(raw, &response) != nil || response.ID == "" {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		switch event {
		case "response.completed":
			if response.Status != "completed" {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			if err := validateCompletedOutput(raw); err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
			return "", true, nil
		case "response.failed":
			if response.Status != "failed" {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			code := ""
			if response.Error != nil && knownCode(response.Error.Code) {
				code = response.Error.Code
			}
			return "", true, &Error{Kind: ErrorResponseFailed, Code: code}
		default:
			if response.Status != "incomplete" {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			return "", true, &Error{Kind: ErrorResponseIncomplete}
		}
	default:
		if !knownProgressEvent(event) {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if err := rejectUnknown(envelope, "type", "sequence_number", "response", "item", "part", "output_index", "content_index"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if strings.HasPrefix(event, "response.refusal.") || strings.HasPrefix(event, "response.reasoning_") {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if strings.HasPrefix(event, "response.output_item.") {
			if err := validateOutputItem(envelope["item"]); err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
		}
		if strings.HasPrefix(event, "response.content_part.") {
			if err := validateOutputTextPart(envelope["part"]); err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
		}
		return "", false, nil
	}
}

func validateCompletedOutput(raw json.RawMessage) error {
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return err
	}
	for _, item := range response.Output {
		if err := validateOutputItem(item); err != nil {
			return err
		}
		var decoded struct {
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(item, &decoded); err != nil {
			return err
		}
		for _, part := range decoded.Content {
			if err := validateOutputTextPart(part); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOutputItem(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("output item missing")
	}
	var item struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.Type != "message" || item.Role != "assistant" {
		return errors.New("unsupported output item")
	}
	return nil
}

func validateOutputTextPart(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("output content part missing")
	}
	var part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &part); err != nil || part.Type != "output_text" {
		return errors.New("unsupported output content part")
	}
	return nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value func() error
	value = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("duplicate or invalid JSON key")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func knownProgressEvent(name string) bool {
	switch name {
	case "response.created", "response.in_progress", "response.queued", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return true
	}
	return false
}

func rejectUnknown(fields map[string]json.RawMessage, allowed ...string) error {
	set := map[string]bool{}
	for _, s := range allowed {
		set[s] = true
	}
	for key := range fields {
		if !set[key] {
			return errors.New("unknown event field")
		}
	}
	return nil
}
