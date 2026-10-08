package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// V2 refuses duplicate keys at all depths before constructing provider
// declarations. Existing v1 parsing and canonical compatibility are unchanged.
func rejectDuplicateProviderKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := scanUniqueJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func scanUniqueJSON(dec *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds limit")
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid object key")
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if keys[name] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			keys[name] = true
			if err := scanUniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("unterminated JSON object")
		}
	case '[':
		for dec.More() {
			if err := scanUniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("unterminated JSON array")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	return nil
}
