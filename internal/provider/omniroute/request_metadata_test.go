package omniroute

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
	if response.Metadata.FirstTokenLatency < 0 {
		t.Fatalf("FirstTokenLatency = %v, want >= 0", response.Metadata.FirstTokenLatency)
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
	if response.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("FirstTokenLatency = %v, want 0 (nothing observed)", response.Metadata.FirstTokenLatency)
	}
}
