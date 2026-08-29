package anthropiccompat_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
)

// TestResponseMetadataTelemetryOnSuccess proves the success path stamps the
// pinned version and transport and measures a first-token latency.
func TestResponseMetadataTelemetryOnSuccess(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validMessagesBody))
	})
	client, _ := newTestClient(t, nil, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "anthropiccompat-http" {
		t.Fatalf("Transport = %q, want anthropiccompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstByteLatency < 0 {
		t.Fatalf("FirstByteLatency = %v, want >= 0", response.Metadata.FirstByteLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
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

// TestFirstByteLatencyAndTotalDurationAreHeaderAndBodyGated proves the #39
// maintainer-review semantics: FirstByteLatency reflects only the HTTP
// response-header arrival (never a guessed model-token latency), and
// Duration covers the whole attempt including body consumption.
func TestFirstByteLatencyAndTotalDurationAreHeaderAndBodyGated(t *testing.T) {
	clock := &gatedClock{current: time.Unix(1700000000, 0)}
	entered := make(chan struct{})
	headersGate := make(chan struct{})
	headersSent := make(chan struct{})
	bodyGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-headersGate
		w.WriteHeader(http.StatusOK)
		// HTTP/1.1 defers the header flush to the first body write, so the
		// flush is explicit: the client observes the header event only after
		// this gate opens.
		w.(http.Flusher).Flush()
		close(headersSent)
		<-bodyGate
		_, _ = w.Write([]byte(validMessagesBody))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{Now: clock.now})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
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

// TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero proves the
// zero-value rule: a refusal before dispatch carries adapter identity but no
// invented latency.
func TestPreDispatchRefusalStillStampsIdentityAndKeepsLatencyZero(t *testing.T) {
	client, _ := newTestClient(t, nil, nil, nil)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "different-model"})
	if err == nil {
		t.Fatal("expected config refusal for a different model")
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("DeliveryState = %v, want not_sent", response.Metadata.DeliveryState)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "anthropiccompat-http" {
		t.Fatalf("Transport = %q, want anthropiccompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstByteLatency != 0 {
		t.Fatalf("FirstByteLatency = %v, want 0 (nothing observed)", response.Metadata.FirstByteLatency)
	}
}

// TestRedirectBodyGatedTotalsDuration proves the #39 maintainer follow-up:
// a refused 3xx reads its body before returning unsafeRedirectError, so
// Duration must include the body wait, not stop at header arrival.
func TestRedirectBodyGatedTotalsDuration(t *testing.T) {
	clock := &gatedClock{current: time.Unix(1700000000, 0)}
	entered := make(chan struct{})
	headersGate := make(chan struct{})
	headersSent := make(chan struct{})
	bodyGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-headersGate
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
		// HTTP/1.1 defers the header flush to the first body write, so the
		// flush is explicit: the client observes the 3xx headers only after
		// this gate opens.
		w.(http.Flusher).Flush()
		close(headersSent)
		<-bodyGate
		_, _ = w.Write([]byte("redirect body"))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := anthropiccompat.New(resolved, nil, anthropiccompat.Options{Now: clock.now})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	done := make(chan struct{})
	var response provider.Response
	go func() {
		response, _ = client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
		close(done)
	}()
	<-entered
	clock.advance(20 * time.Millisecond) // 3xx headers gate
	close(headersGate)
	<-headersSent
	// Let the transport parse the flushed headers before advancing: without
	// the Duration fix the value is read at Do-return, so this biases the
	// regression test to fail on the unfixed code. With the fix the clock is
	// read only after the body gate, so the green result is deterministic.
	time.Sleep(50 * time.Millisecond)
	clock.advance(20 * time.Millisecond) // 3xx body gate: body lands at +40ms
	close(bodyGate)
	<-done
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("DeliveryState = %v, want completed (redirect body fully read)", response.Metadata.DeliveryState)
	}
	if response.Metadata.FirstByteLatency < 20*time.Millisecond || response.Metadata.FirstByteLatency > 40*time.Millisecond {
		t.Fatalf("FirstByteLatency = %v, want within [20ms, 40ms]", response.Metadata.FirstByteLatency)
	}
	if response.Metadata.Duration != 40*time.Millisecond {
		t.Fatalf("Duration = %v, want 40ms (redirect body consumption included)", response.Metadata.Duration)
	}
}
