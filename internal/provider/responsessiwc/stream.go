package responsessiwc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

type delivery struct{ sent, response atomic.Bool }

func (d *delivery) trace(_ func() time.Time) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { d.sent.Store(true) },
		GotFirstResponseByte: func() { d.response.Store(true) },
	}
}
func (d *delivery) state() provider.DeliveryState {
	if d.response.Load() {
		return provider.DeliveryResponseStarted
	}
	if d.sent.Load() {
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
	state := &streamState{output: &output, outputLimit: outputLimit}
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
				_, done, parseErr := consumeEvent(name, data, state)
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

type streamState struct {
	output          *strings.Builder
	outputLimit     int
	outputTextDone  bool
	outputTextValue string
}

func consumeEvent(event string, data []byte, state *streamState) (string, bool, error) {
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
		if state.outputTextDone {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if err := rejectUnknown(envelope, "type", "sequence_number", "delta", "item_id", "output_index", "content_index", "logprobs"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		var delta string
		if raw, ok := envelope["delta"]; !ok || json.Unmarshal(raw, &delta) != nil || requireString(envelope, "item_id") != nil || validateIndices(envelope, "output_index", "content_index") != nil || validateLogprobs(envelope["logprobs"]) != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if state.output.Len()+len(delta) > state.outputLimit {
			return "", false, &Error{Kind: ErrorResponseTooLarge}
		}
		state.output.WriteString(delta)
		return "", false, nil
	case "response.output_text.done":
		if state.outputTextDone {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if err := rejectUnknown(envelope, "type", "sequence_number", "item_id", "output_index", "content_index", "text", "logprobs"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		var text string
		if json.Unmarshal(envelope["text"], &text) != nil || requireString(envelope, "item_id") != nil || validateIndices(envelope, "output_index", "content_index") != nil || validateLogprobs(envelope["logprobs"]) != nil || text != state.output.String() {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		state.outputTextDone = true
		state.outputTextValue = text
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
			text, hasOutput, err := validateCompletedOutput(raw)
			if err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
			if hasOutput && text != state.output.String() || state.outputTextDone && state.outputTextValue != state.output.String() {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
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
		if strings.HasPrefix(event, "response.reasoning_summary_part.") {
			allowed := []string{"type", "sequence_number", "item_id", "output_index", "summary_index", "part"}
			if strings.HasSuffix(event, ".done") {
				allowed = append(allowed, "status")
			}
			if err := rejectUnknown(envelope, allowed...); err != nil || requireString(envelope, "item_id") != nil || validateIndices(envelope, "output_index", "summary_index") != nil || validateReasoningSummaryPart(envelope["part"]) != nil || validateOptionalIncompleteStatus(envelope) != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			return "", false, nil
		}
		if strings.HasPrefix(event, "response.reasoning_summary_text.") {
			field := "delta"
			if strings.HasSuffix(event, ".done") {
				field = "text"
			}
			if err := rejectUnknown(envelope, "type", "sequence_number", "item_id", "output_index", "summary_index", field); err != nil || requireString(envelope, "item_id") != nil || validateIndices(envelope, "output_index", "summary_index") != nil || validateStringField(envelope, field) != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			return "", false, nil
		}
		if strings.HasPrefix(event, "response.reasoning_text.") {
			field := "delta"
			if strings.HasSuffix(event, ".done") {
				field = "text"
			}
			if err := rejectUnknown(envelope, "type", "sequence_number", "item_id", "output_index", "content_index", field); err != nil || requireString(envelope, "item_id") != nil || validateIndices(envelope, "output_index", "content_index") != nil || validateStringField(envelope, field) != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope}
			}
			return "", false, nil
		}
		if err := rejectUnknown(envelope, "type", "sequence_number", "response", "item", "part", "item_id", "output_index", "content_index"); err != nil {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if strings.HasPrefix(event, "response.refusal.") {
			return "", false, &Error{Kind: ErrorInvalidEnvelope}
		}
		if strings.HasPrefix(event, "response.output_item.") {
			if err := validateOutputItemEvent(envelope); err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
		}
		if strings.HasPrefix(event, "response.content_part.") {
			if err := validateContentPartEvent(envelope); err != nil {
				return "", false, &Error{Kind: ErrorInvalidEnvelope, Cause: err}
			}
		}
		return "", false, nil
	}
}

func validateCompletedOutput(raw json.RawMessage) (string, bool, error) {
	var response struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", false, err
	}
	if len(response.Output) == 0 {
		return "", false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(response.Output, &items); err != nil {
		return "", true, err
	}
	var text string
	textItems := 0
	for _, item := range items {
		kind, err := validateOutputItem(item)
		if err != nil {
			return "", true, err
		}
		if kind == "reasoning" {
			continue
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(item, &decoded); err != nil {
			return "", true, err
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(decoded["content"], &parts); err != nil {
			return "", true, err
		}
		for _, part := range parts {
			partText, err := validateOutputTextPart(part)
			if err != nil {
				return "", true, err
			}
			text += partText
		}
		textItems++
	}
	if textItems != 1 {
		return "", true, errors.New("expected one assistant text item")
	}
	return text, true, nil
}

func validateOutputItem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("output item missing")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", err
	}
	var kind string
	if json.Unmarshal(item["type"], &kind) != nil {
		return "", errors.New("output item type missing")
	}
	switch kind {
	case "message":
		if rejectUnknown(item, "id", "type", "status", "role", "content", "phase") != nil {
			return "", errors.New("unknown message item field")
		}
		var role string
		if json.Unmarshal(item["role"], &role) != nil || role != "assistant" {
			return "", errors.New("unsupported message role")
		}
		return kind, nil
	case "reasoning":
		if rejectUnknown(item, "id", "type", "status", "summary", "content", "encrypted_content") != nil {
			return "", errors.New("unknown reasoning item field")
		}
		if raw, ok := item["summary"]; ok {
			var summaries []json.RawMessage
			if json.Unmarshal(raw, &summaries) != nil {
				return "", errors.New("invalid reasoning summary")
			}
			for _, summary := range summaries {
				var part map[string]json.RawMessage
				var typ, text string
				if json.Unmarshal(summary, &part) != nil || rejectUnknown(part, "type", "text") != nil || json.Unmarshal(part["type"], &typ) != nil || typ != "summary_text" || json.Unmarshal(part["text"], &text) != nil {
					return "", errors.New("invalid reasoning summary part")
				}
			}
		}
		if raw, ok := item["content"]; ok {
			var content []json.RawMessage
			if json.Unmarshal(raw, &content) != nil {
				return "", errors.New("invalid reasoning content")
			}
			for _, piece := range content {
				var fields map[string]json.RawMessage
				var typ, text string
				if json.Unmarshal(piece, &fields) != nil || rejectUnknown(fields, "type", "text") != nil || json.Unmarshal(fields["type"], &typ) != nil || typ != "reasoning_text" || json.Unmarshal(fields["text"], &text) != nil {
					return "", errors.New("invalid reasoning content part")
				}
			}
		}
		if raw, ok := item["encrypted_content"]; ok {
			var encrypted string
			if json.Unmarshal(raw, &encrypted) != nil {
				return "", errors.New("invalid encrypted reasoning content")
			}
		}
		return kind, nil
	default:
		return "", errors.New("unsupported output item")
	}
}

func validateOutputTextPart(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("output content part missing")
	}
	var part map[string]json.RawMessage
	if err := json.Unmarshal(raw, &part); err != nil {
		return "", err
	}
	var kind, text string
	if rejectUnknown(part, "type", "text", "annotations", "logprobs") != nil || json.Unmarshal(part["type"], &kind) != nil || kind != "output_text" || json.Unmarshal(part["text"], &text) != nil || validateLogprobs(part["logprobs"]) != nil {
		return "", errors.New("unsupported output content part")
	}
	return text, nil
}

func validateOutputItemEvent(envelope map[string]json.RawMessage) error {
	if err := rejectUnknown(envelope, "type", "sequence_number", "output_index", "item"); err != nil {
		return err
	}
	if err := validateIndices(envelope, "output_index"); err != nil {
		return err
	}
	_, err := validateOutputItem(envelope["item"])
	return err
}

func validateContentPartEvent(envelope map[string]json.RawMessage) error {
	if err := rejectUnknown(envelope, "type", "sequence_number", "item_id", "output_index", "content_index", "part"); err != nil {
		return err
	}
	if err := requireString(envelope, "item_id"); err != nil {
		return err
	}
	if err := validateIndices(envelope, "output_index", "content_index"); err != nil {
		return err
	}
	_, err := validateOutputTextPart(envelope["part"])
	return err
}

func validateReasoningSummaryPart(raw json.RawMessage) error {
	var part map[string]json.RawMessage
	var typ, text string
	if json.Unmarshal(raw, &part) != nil || rejectUnknown(part, "type", "text") != nil || json.Unmarshal(part["type"], &typ) != nil || typ != "summary_text" || json.Unmarshal(part["text"], &text) != nil {
		return errors.New("invalid reasoning summary part")
	}
	return nil
}

func validateOptionalIncompleteStatus(envelope map[string]json.RawMessage) error {
	raw, ok := envelope["status"]
	if !ok {
		return nil
	}
	var status string
	if json.Unmarshal(raw, &status) != nil || status != "incomplete" {
		return errors.New("invalid reasoning summary status")
	}
	return nil
}

func validateStringField(fields map[string]json.RawMessage, name string) error {
	var value string
	raw, ok := fields[name]
	if !ok || json.Unmarshal(raw, &value) != nil {
		return errors.New("required text missing")
	}
	return nil
}

func requireString(fields map[string]json.RawMessage, name string) error {
	var value string
	raw, ok := fields[name]
	if !ok || json.Unmarshal(raw, &value) != nil || value == "" {
		return errors.New("required string missing")
	}
	return nil
}

func validateIndices(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		var value uint64
		raw, ok := fields[name]
		if !ok || json.Unmarshal(raw, &value) != nil {
			return errors.New("invalid event index")
		}
	}
	return nil
}

func validateLogprobs(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return errors.New("invalid logprobs")
	}
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || rejectUnknown(fields, "token", "logprob", "top_logprobs") != nil {
			return errors.New("invalid logprob entry")
		}
		if _, ok := fields["token"]; !ok {
			return errors.New("logprob token missing")
		}
		if raw, ok := fields["logprob"]; ok {
			var value float64
			if json.Unmarshal(raw, &value) != nil {
				return errors.New("invalid logprob value")
			}
		}
		if raw, ok := fields["top_logprobs"]; ok {
			var top []json.RawMessage
			if json.Unmarshal(raw, &top) != nil {
				return errors.New("invalid top logprobs")
			}
			for _, candidate := range top {
				var fields map[string]json.RawMessage
				if json.Unmarshal(candidate, &fields) != nil || rejectUnknown(fields, "token", "logprob") != nil {
					return errors.New("invalid top logprob entry")
				}
			}
		}
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
	case "response.created", "response.in_progress", "response.queued", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_text.delta", "response.reasoning_text.done":
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
