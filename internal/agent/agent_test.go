package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/protocol"
	"github.com/RenyEnnos/Runstead/internal/provider"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClientReturnsRawProviderTextAndMakesOneAttempt(t *testing.T) {
	fake := provider.NewFake(provider.Response{Text: "provider response"})
	client := New(fake, discardLogger())

	got, err := client.Ask(context.Background(), "inspect the repository")

	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if got != "provider response" {
		t.Fatalf("Ask() = %q, want provider response", got)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
	requests := fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("Requests() length = %d, want 1", len(requests))
	}
	if requests[0].Protocol != protocol.Current || requests[0].Prompt != "inspect the repository" {
		t.Fatalf("request = %#v", requests[0])
	}
}

func TestClientPropagatesProviderErrorWithoutAutomaticRetry(t *testing.T) {
	wantErr := errors.New("provider failed")
	fake := provider.NewErrorFake(wantErr)
	client := New(fake, discardLogger())

	_, err := client.Ask(context.Background(), "inspect")

	if !errors.Is(err, wantErr) {
		t.Fatalf("Ask() error = %v, want %v", err, wantErr)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
}

func TestClientPropagatesContextCancellation(t *testing.T) {
	fake := provider.NewBlockingFake()
	client := New(fake, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Ask(ctx, "inspect")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask() error = %v, want context.Canceled", err)
	}
	if fake.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", fake.Attempts())
	}
}
