package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenAICompatibleRateLimitObservationSurvivesInspect(t *testing.T) {
	const bodyMarker = "synthetic_provider_body_marker"
	const headerMarker = "synthetic_unlisted_header_marker"
	workspace := t.TempDir()
	stateDir := t.TempDir()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Retry-After", "29")
		w.Header().Set("X-RateLimit-Limit-Requests", "60")
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
		w.Header().Set("X-RateLimit-Reset-Requests", "1m")
		w.Header().Set("X-RateLimit-Limit-Tokens", "100000")
		w.Header().Set("X-RateLimit-Remaining-Tokens", "90000")
		w.Header().Set("X-RateLimit-Reset-Tokens", "30s")
		w.Header().Set("X-Provider-Secret", headerMarker)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"` + bodyMarker + `"}}`))
	}))
	defer server.Close()

	providersFile := writeProvidersFile(t, e2eFamilies[0], map[string]string{
		"offline-provider": server.URL + "/v1",
	})
	args := []string{
		"run", "--task", "record one provider rate-limit observation",
		"--workspace", workspace,
		"--providers", providersFile,
		"--provider-id", "offline-provider",
		"--state-dir", stateDir,
		"--min-start-interval", "1ms",
		"--log-level", "error",
	}
	var runOut, runErr strings.Builder
	if code := run(context.Background(), args, &runOut, &runErr); code == exitSuccess {
		t.Fatalf("429 provider response unexpectedly completed the task:\n%s", runOut.String())
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("provider physical requests = %d, want one governed attempt", got)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatalf("open SQLite state: %v", err)
	}
	var taskID string
	if err := db.QueryRow("SELECT task_id FROM tasks ORDER BY created_at DESC LIMIT 1").Scan(&taskID); err != nil {
		_ = db.Close()
		t.Fatalf("read task id from durable state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close SQLite state before inspect: %v", err)
	}

	var inspectOut, inspectErr strings.Builder
	if code := run(context.Background(), []string{"inspect", taskID, "--state-dir", stateDir}, &inspectOut, &inspectErr); code != exitSuccess {
		t.Fatalf("runstead inspect exit = %d\nstderr:\n%s", code, inspectErr.String())
	}
	for _, want := range []string{
		"status_code=429",
		"observed_retry_after=29s",
		"limit_requests=60",
		"remaining_requests=0",
		"reset_requests=1m0s",
		"limit_tokens=100000",
		"remaining_tokens=90000",
		"reset_tokens=30s",
		"delivery_state=completed",
		"provider_failure_class=rate_or_capacity",
		"selected_backoff=29s",
		"debited=1",
		"requests_per_minute: unknown",
	} {
		if !strings.Contains(inspectOut.String(), want) {
			t.Errorf("runstead inspect missing %q:\n%s", want, inspectOut.String())
		}
	}
	if strings.Contains(inspectOut.String(), bodyMarker) || strings.Contains(inspectOut.String(), headerMarker) ||
		strings.Contains(runOut.String(), bodyMarker) || strings.Contains(runErr.String(), bodyMarker) ||
		strings.Contains(inspectErr.String(), bodyMarker) || strings.Contains(inspectErr.String(), headerMarker) {
		t.Fatalf("provider-controlled body/header text escaped into CLI output:\nrun stdout:\n%s\nrun stderr:\n%s\ninspect:\n%s\ninspect stderr:\n%s",
			runOut.String(), runErr.String(), inspectOut.String(), inspectErr.String())
	}
	database, err := os.ReadFile(filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatalf("read closed SQLite database: %v", err)
	}
	if strings.Contains(string(database), bodyMarker) || strings.Contains(string(database), headerMarker) {
		t.Fatal("provider-controlled body/header text persisted in SQLite")
	}
	if !strings.Contains(inspectOut.String(), "selected_backoff=29s") || !strings.Contains(inspectOut.String(), "observed_retry_after=29s") {
		t.Fatal("provider observation and governor decision must remain separately inspectable")
	}
	if !strings.Contains(inspectOut.String(), "receipt_error") {
		t.Fatal("outcome event must keep receipt error separate from provider failure class")
	}
	if strings.Contains(inspectOut.String(), "observed_reset_at=0001-01-01T00:00:00Z") {
		t.Fatal("unknown reset-at must not be fabricated")
	}
}
