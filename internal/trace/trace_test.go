package trace

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLoggerWritesStructuredRecords(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelInfo)

	logger.Info("provider attempt", "provider", "fake", "attempt", 1)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("log is not JSON: %v; output=%q", err, output.String())
	}
	if record["msg"] != "provider attempt" || record["provider"] != "fake" || record["attempt"] != float64(1) {
		t.Fatalf("record = %#v", record)
	}
}
