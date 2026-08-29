package googlecompat_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
)

// TestResponseMetadataTelemetryOnSuccess proves the success path stamps the
// pinned version and transport and measures a first-token latency.
func TestResponseMetadataTelemetryOnSuccess(t *testing.T) {
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	client, _ := newTestClient(t, nil, nil, recorder)
	response, err := client.Complete(context.Background(), provider.Request{Prompt: "hi", Model: "model-a"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Metadata.AdapterVersion != provider.CompatAdapterVersion {
		t.Fatalf("AdapterVersion = %q, want %q", response.Metadata.AdapterVersion, provider.CompatAdapterVersion)
	}
	if response.Metadata.Transport != "googlecompat-http" {
		t.Fatalf("Transport = %q, want googlecompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
	}
	if response.Metadata.RetryCount != 0 || response.Metadata.Fallback || response.Metadata.UsageEstimated {
		t.Fatalf("protected lane telemetry nonzero: retry=%d fallback=%v usage_estimated=%v",
			response.Metadata.RetryCount, response.Metadata.Fallback, response.Metadata.UsageEstimated)
	}
}

// TestFirstTokenLatencyUsesInjectedClock proves FirstTokenLatency equals the
// proven started-to-first-byte delta under the deterministic injected clock.
func TestFirstTokenLatencyUsesInjectedClock(t *testing.T) {
	current := time.Unix(1700000000, 0)
	clock := func() time.Time { return current }
	entered := make(chan struct{})
	firstByteGate := make(chan struct{})
	recorder := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-firstByteGate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(validGenerateBody))
	})
	resolved := resolvedForBase(t, recorder.server.URL)
	client, err := googlecompat.New(resolved, nil, googlecompat.Options{Now: clock})
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
	current = current.Add(50 * time.Millisecond)
	close(firstByteGate)
	<-done
	if response.Metadata.FirstTokenLatency != 50*time.Millisecond {
		t.Fatalf("FirstTokenLatency = %v, want 50ms", response.Metadata.FirstTokenLatency)
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
	if response.Metadata.Transport != "googlecompat-http" {
		t.Fatalf("Transport = %q, want googlecompat-http", response.Metadata.Transport)
	}
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
