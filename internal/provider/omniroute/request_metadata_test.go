package omniroute

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

// TestTransportMetadataTelemetryShape is the live-path metadata-shape test
// (issue #39): through a real HTTP response the metadata must carry the
// pinned identity, a hash-formatted session fingerprint and zero
// protected-lane fields.
func TestTransportMetadataTelemetryShape(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(sessionIDHeader, "live-session-123")
		w.Header().Set(requestIDHeader, "req-123")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"runstead reply"}}]}`)
	}))
	defer server.Close()

	response, err := client.completeOnce(context.Background(), provider.Request{Protocol: protocol.Current, Prompt: "private prompt"})
	if err != nil {
		t.Fatalf("completeOnce: %v", err)
	}
	if response.Metadata.AdapterVersion != AdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, AdapterVersion)
	}
	if response.Metadata.Transport != transportID {
		t.Fatalf("Transport = %q, want %q", response.Metadata.Transport, transportID)
	}
	if !strings.HasPrefix(response.Metadata.SessionID, "sha256:") {
		t.Fatalf("SessionID = %q, want sha256 fingerprint, never the raw session identity", response.Metadata.SessionID)
	}
	if strings.Contains(response.Metadata.SessionID, "live-session-123") {
		t.Fatalf("SessionID leaks the raw session identity: %q", response.Metadata.SessionID)
	}
	if response.Metadata.FirstByteLatency < 0 {
		t.Fatalf("FirstByteLatency = %v, want >= 0", response.Metadata.FirstByteLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

// TestPreDispatchRefusalStampsIdentityAndKeepsLatencyZero mirrors the compat
// adapters: identity is stamped even when nothing was dispatched.
func TestPreDispatchRefusalStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(nil))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := client.completeOnce(ctx, provider.Request{Protocol: protocol.Current, Prompt: "private prompt"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != AdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, AdapterVersion)
	}
	if response.Metadata.Transport != transportID {
		t.Fatalf("Transport = %q, want %q", response.Metadata.Transport, transportID)
	}
	if response.Metadata.FirstByteLatency != 0 {
		t.Fatalf("FirstByteLatency = %v, want 0 (nothing observed)", response.Metadata.FirstByteLatency)
	}
}

// gatedClock is an injected adapter clock that the test advances between the
// header and body gates. The transport readLoop reads it concurrently, so
// access is mutex-protected (deterministic values come from the channel
// gates; the mutex only orders the reads).
type gatedClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *gatedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *gatedClock) advance(delta time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(delta)
	c.mu.Unlock()
}

// TestTransportMetadataFirstByteAndDurationAreHeaderAndBodyGated proves the
// #39 maintainer-review semantics on the live transport path: FirstByteLatency
// reflects only the HTTP response-header arrival and Duration covers the whole
// attempt including body consumption.
func TestTransportMetadataFirstByteAndDurationAreHeaderAndBodyGated(t *testing.T) {
	clock := &gatedClock{current: time.Unix(1700000000, 0)}
	entered := make(chan struct{})
	headersGate := make(chan struct{})
	headersSent := make(chan struct{})
	bodyGate := make(chan struct{})
	server := httptest.NewServer(safeHandler(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-headersGate
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// HTTP/1.1 defers the header flush to the first body write, so the
		// flush is explicit: the client observes the header event only after
		// this gate opens.
		w.(http.Flusher).Flush()
		close(headersSent)
		<-bodyGate
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"runstead reply"}}]}`)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client(), Now: clock.now})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.completeOnce(context.Background(), provider.Request{Protocol: protocol.Current, Prompt: "private prompt"})
		close(done)
	}()
	<-entered
	clock.advance(20 * time.Millisecond) // headers gate: earliest header arrival
	close(headersGate)
	<-headersSent
	clock.advance(20 * time.Millisecond) // body gate: body lands at +40ms
	close(bodyGate)
	<-done
	if response.Metadata.FirstByteLatency < 20*time.Millisecond || response.Metadata.FirstByteLatency > 40*time.Millisecond {
		t.Fatalf("FirstByteLatency = %v, want within [20ms, 40ms] (header-gated arrival)", response.Metadata.FirstByteLatency)
	}
	if response.Metadata.Duration != 40*time.Millisecond {
		t.Fatalf("Duration = %v, want 40ms (includes body consumption)", response.Metadata.Duration)
	}
	if response.Metadata.Duration < response.Metadata.FirstByteLatency {
		t.Fatalf("Duration %v precedes FirstByteLatency %v", response.Metadata.Duration, response.Metadata.FirstByteLatency)
	}
}
