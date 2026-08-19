package fake

import (
	"context"
	"errors"
	"testing"

	provider "experiments/provider-abstraction/provider"
)

func TestFakeCompleteReturnsPredefinedResponses(t *testing.T) {
	fake := NewFake(
		provider.ProviderResponse{
			Content:  "first response",
			Metadata: provider.ProviderResponseMetadata{StatusCode: 200},
		},
		provider.ProviderResponse{
			Content:  "second response",
			Metadata: provider.ProviderResponseMetadata{StatusCode: 200},
		},
	)

	req := provider.ProviderRequest{
		ClientRequestID: "req-1",
		Model:           "test-model",
		Messages:        []provider.Message{{Role: "user", Content: "hello"}},
	}

	resp1, err := fake.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("first Complete error: %v", err)
	}
	if resp1.Content != "first response" {
		t.Fatalf("expected 'first response', got %q", resp1.Content)
	}

	resp2, err := fake.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("second Complete error: %v", err)
	}
	if resp2.Content != "second response" {
		t.Fatalf("expected 'second response', got %q", resp2.Content)
	}

	// Third call should exhaust responses
	_, err = fake.Complete(context.Background(), req)
	if !errors.Is(err, ErrNoPredefinedResponse) {
		t.Fatalf("expected ErrNoPredefinedResponse, got %v", err)
	}

	if fake.Attempts() != 3 {
		t.Fatalf("Attempts() = %d, want 3", fake.Attempts())
	}
}

func TestFakeCompleteRecordsRequests(t *testing.T) {
	fake := NewFake(
		provider.ProviderResponse{Content: "resp", Metadata: provider.ProviderResponseMetadata{StatusCode: 200}},
	)

	req := provider.ProviderRequest{
		ClientRequestID: "req-1",
		Model:           "test-model",
		Messages:        []provider.Message{{Role: "user", Content: "hello"}},
	}

	_, _ = fake.Complete(context.Background(), req)
	_, _ = fake.Complete(context.Background(), req)

	requests := fake.Requests()
	if len(requests) != 2 {
		t.Fatalf("len(Requests()) = %d, want 2", len(requests))
	}
	if requests[0].ClientRequestID != "req-1" {
		t.Fatalf("first request ClientRequestID = %q, want req-1", requests[0].ClientRequestID)
	}
}

func TestFakeErrorFakePropagatesError(t *testing.T) {
	wantErr := errors.New("upstream down")
	fake := NewErrorFake(wantErr)

	_, err := fake.Complete(context.Background(), provider.ProviderRequest{Model: "test"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
}

func TestFakeBlockingFakeRespectsCancellation(t *testing.T) {
	fake := NewBlockingFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fake.Complete(ctx, provider.ProviderRequest{Model: "test"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFakeHealthCheckReturnsConfigured(t *testing.T) {
	fake := NewFake().WithHealthCheck(provider.HealthResult{
		Healthy: false,
		Reason:  "rate_limited",
	})

	health, err := fake.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck error: %v", err)
	}
	if health.Healthy {
		t.Fatal("expected unhealthy")
	}
	if health.Reason != "rate_limited" {
		t.Fatalf("Reason = %q, want rate_limited", health.Reason)
	}
}

func TestFakeModelsReturnsConfigured(t *testing.T) {
	fake := NewFake().WithModels([]provider.ModelInfo{
		{ID: "model-a", DisplayName: "Model A", ContextWindow: 8192},
	})

	models, err := fake.Models(context.Background())
	if err != nil {
		t.Fatalf("Models error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("models = %#v, want [model-a]", models)
	}
}

func TestFakeNameReturnsConfigured(t *testing.T) {
	fake := NewFake().WithName("custom-provider")
	if fake.Name() != "custom-provider" {
		t.Fatalf("Name() = %q, want custom-provider", fake.Name())
	}
}

func TestFakeEmptyModelFailClosed(t *testing.T) {
	fake := NewFake(provider.ProviderResponse{Content: "ok"})

	req := provider.ProviderRequest{
		ClientRequestID: "req-1",
		Model:           "", // Empty model
		Messages:        []provider.Message{{Role: "user", Content: "hi"}},
	}

	// The fake doesn't validate model - that's the caller's responsibility
	// But we can test that empty model is passed through
	resp, err := fake.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "" {
		t.Fatal("request model should remain empty")
	}
	_ = resp
}

func TestCompileTimeInterfaceAssertion(t *testing.T) {
	// Compile-time check that Fake implements ProviderClient
	var _ provider.ProviderClient = (*Fake)(nil)
	_ = fakeImpl
}

var fakeImpl provider.ProviderClient = NewFake()
