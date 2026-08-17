package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
)

func TestFakeReturnsPredefinedResponsesAndRecordsRequests(t *testing.T) {
	fake := NewFake(
		ProviderResponse{Content: "first", Metadata: ProviderResponseMetadata{StatusCode: 200}},
		ProviderResponse{Content: "second", Metadata: ProviderResponseMetadata{StatusCode: 200}},
	)
	request := Request{Protocol: protocol.Current, Prompt: "inspect"}

	first, err := fake.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("first Complete() error = %v", err)
	}
	second, err := fake.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("second Complete() error = %v", err)
	}
	if first.Text != "first" || second.Text != "second" {
		t.Fatalf("responses = %#v, %#v", first, second)
	}
	if first.Metadata.DeliveryState != DeliveryCompleted || second.Metadata.DeliveryState != DeliveryCompleted {
		t.Fatalf("delivery states = %v, %v; want completed", first.Metadata.DeliveryState, second.Metadata.DeliveryState)
	}
	if fake.Attempts() != 2 {
		t.Fatalf("Attempts() = %d, want 2", fake.Attempts())
	}
	if got := fake.Requests(); len(got) != 2 || got[0].ClientRequestID != request.ClientRequestID || got[1].ClientRequestID != request.ClientRequestID {
		t.Fatalf("Requests() = %#v, want two copies of %#v", got, request)
	}
}

func TestFakePropagatesConfiguredErrorWithoutRetry(t *testing.T) {
	wantErr := errors.New("upstream unavailable")
	fake := NewErrorFake(wantErr)

	_, err := fake.Complete(context.Background(), Request{Protocol: protocol.Current})

	if !errors.Is(err, wantErr) {
		t.Fatalf("Complete() error = %v, want %v", err, wantErr)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
}

func TestBlockingFakeRespectsCancellation(t *testing.T) {
	fake := NewBlockingFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fake.Complete(ctx, Request{Protocol: protocol.Current})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete() error = %v, want context.Canceled", err)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
}