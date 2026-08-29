package governor_test

import (
	"context"
	"testing"
	"time"

	policy "github.com/RenyEnnos/Runstead/internal/governor"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

type telemetryProbeClient struct{}

func (c *telemetryProbeClient) Complete(_ context.Context, _ provider.Request) (provider.Response, error) {
	return provider.Response{Text: "ok", Metadata: provider.ResponseMetadata{
		AdapterVersion:    provider.CompatAdapterVersion,
		Transport:         "test-http",
		StatusCode:        200,
		Duration:          12 * time.Millisecond,
		FirstTokenLatency: 3 * time.Millisecond,
		DeliveryState:     provider.DeliveryCompleted,
	}}, nil
}

func (c *telemetryProbeClient) RouteSafety() provider.RouteSafety {
	return provider.SafeRouteSafety()
}

// TestExecuteEventCarriesAttemptMetadata proves the attempt_finished event
// carries the sanitized metadata the adapter proved (issue #39).
func TestExecuteEventCarriesAttemptMetadata(t *testing.T) {
	g, _, events := instantGovernor(t)
	result := g.Execute(context.Background(), policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, &telemetryProbeClient{}, nil)
	if result.Err != nil || result.Completion.Err != nil {
		t.Fatalf("Execute() = %#v", result)
	}
	g.DrainEvents()
	var found bool
	for _, event := range events.Events() {
		if event.Kind != policy.EventAttemptFinished {
			continue
		}
		found = true
		if event.AttemptMetadata.AdapterVersion != provider.CompatAdapterVersion {
			t.Fatalf("AttemptMetadata.AdapterVersion = %q, want %q", event.AttemptMetadata.AdapterVersion, provider.CompatAdapterVersion)
		}
		if event.AttemptMetadata.Transport != "test-http" {
			t.Fatalf("AttemptMetadata.Transport = %q, want test-http", event.AttemptMetadata.Transport)
		}
		if event.AttemptMetadata.FirstTokenLatency != 3*time.Millisecond {
			t.Fatalf("AttemptMetadata.FirstTokenLatency = %v, want 3ms", event.AttemptMetadata.FirstTokenLatency)
		}
		if event.AttemptMetadata.DeliveryState != provider.DeliveryCompleted {
			t.Fatalf("AttemptMetadata.DeliveryState = %v, want completed", event.AttemptMetadata.DeliveryState)
		}
	}
	if !found {
		t.Fatal("no attempt_finished event was emitted")
	}
}

// TestRefusedExecutionNeverFabricatesAttemptMetadata proves the no-evidence
// rule: when no attempt is dispatched, emitted attempt_finished events must
// carry zero metadata instead of invented values.
func TestRefusedExecutionNeverFabricatesAttemptMetadata(t *testing.T) {
	g, _, events := instantGovernor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := g.Execute(ctx, policy.AttemptRequest{
		TaskID:          "task-1",
		ClientRequestID: "request-1",
		ProviderRequest: provider.Request{Prompt: "private prompt"},
	}, &telemetryProbeClient{}, nil)
	if result.Err == nil {
		t.Fatal("expected refusal for a canceled context")
	}
	g.DrainEvents()
	for _, event := range events.Events() {
		if event.Kind != policy.EventAttemptFinished {
			continue
		}
		if event.AttemptMetadata.AdapterVersion != "" || event.AttemptMetadata.Transport != "" ||
			event.AttemptMetadata.FirstTokenLatency != 0 || event.AttemptMetadata.RetryCount != 0 ||
			event.AttemptMetadata.Fallback || event.AttemptMetadata.UsageEstimated {
			t.Fatalf("refused attempt fabricated metadata: %#v", event.AttemptMetadata)
		}
	}
}

// TestOutcomeCarriesMetadataZeroByDefault pins the zero-value contract of the
// new Outcome field.
func TestOutcomeCarriesMetadataZeroByDefault(t *testing.T) {
	var outcome policy.Outcome
	if outcome.Metadata.AdapterVersion != "" || outcome.Metadata.Transport != "" || outcome.Metadata.FirstTokenLatency != 0 {
		t.Fatalf("zero outcome metadata = %+v, want empty", outcome.Metadata)
	}
}
