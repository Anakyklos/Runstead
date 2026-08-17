package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// loadScriptedResponses reads a deterministic offline provider script: one JSON
// object per line with a "text" field. The responses are replayed in order
// through the real account governor and read-only tools, so a scripted run
// exercises the full loop without any network access.
func loadScriptedResponses(path string) ([]provider.ProviderResponse, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("scripted responses unavailable: %w", err)
	}
	defer file.Close()

	var responses []provider.ProviderResponse
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("scripted responses line %d: %w", lineNumber, err)
		}
		if strings.TrimSpace(entry.Text) == "" {
			return nil, fmt.Errorf("scripted responses line %d: text must not be empty", lineNumber)
		}
		responses = append(responses, provider.ProviderResponse{
			Content: entry.Text,
			Metadata: provider.ProviderResponseMetadata{
				StatusCode: 200,
				Model:      "scripted",
			},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scripted responses: %w", err)
	}
	if len(responses) == 0 {
		return nil, errors.New("scripted responses file is empty")
	}
	return responses, nil
}
