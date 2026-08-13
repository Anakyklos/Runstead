package omniroute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestCompleteConsumesFinalAttemptReceiptHeader(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	set := provider.AttemptReceiptSet{
		SchemaVersion:   provider.AttemptReceiptSchemaVersion,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []provider.AttemptReceipt{{
			SchemaVersion:   provider.AttemptReceiptSchemaVersion,
			AttemptID:       "attempt-1",
			ClientRequestID: "request-1",
			Sequence:        1,
			Provider:        "provider",
			Model:           "model",
			AccountLaneHash: "lane",
			StartedAt:       now,
			CompletedAt:     now.Add(time.Second),
			Outcome:         provider.AttemptOutcomeSuccess,
			Trigger:         provider.AttemptTriggerInitial,
			UpstreamReached: true,
		}},
	}
	encoded, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(clientRequestIDHeader); got != "request-1" {
			t.Errorf("client request header = %q, want request-1", got)
		}
		if got := r.Header.Get(attemptReceiptRequestHeader); got != "v1" {
			t.Errorf("receipt request header = %q, want v1", got)
		}
		w.Header().Set(attemptReceiptHeader, string(encoded))
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()
	client.config.EnableAttemptReceipts = true
	client.config.Provider = "provider"
	client.config.AccountLaneHash = "lane"
	client.config.Model = "model"
	client.config.RouteSafety = provider.ReceiptRouteSafety()
	client.gatewayContractHealth = provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "test_fixture"}
	client.now = func() time.Time { return now.Add(2 * time.Second) }

	response, err := client.Complete(context.Background(), provider.Request{
		Prompt:          "prompt",
		ClientRequestID: "request-1",
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if response.Metadata.AttemptReceipts == nil || len(response.Metadata.AttemptReceipts.Receipts) != 1 {
		t.Fatalf("receipts = %#v, want one validated receipt", response.Metadata.AttemptReceipts)
	}
}

func TestCompleteFailsClosedWithoutAttemptReceiptHeader(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()
	client.config.EnableAttemptReceipts = true
	client.config.Provider = "provider"
	client.config.Model = "model"
	client.config.AccountLaneHash = "lane"
	client.config.RouteSafety = provider.ReceiptRouteSafety()
	client.gatewayContractHealth = provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "test_fixture"}
	_, err := client.Complete(context.Background(), provider.Request{Prompt: "prompt", ClientRequestID: "request-1"})
	var receiptErr *Error
	if !errors.As(err, &receiptErr) || receiptErr.Kind != ErrorAttemptReceiptsMissing {
		t.Fatalf("Complete() error = %v, want sanitized missing-receipts error", err)
	}
}

func TestCompleteRejectsInvalidReceiptSet(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(provider.AttemptReceiptSet{
		SchemaVersion:   99,
		ClientRequestID: "request-1",
		Finalized:       true,
		Receipts: []provider.AttemptReceipt{{
			SchemaVersion:   provider.AttemptReceiptSchemaVersion,
			AttemptID:       "attempt-1",
			ClientRequestID: "request-1",
			Sequence:        1,
			Provider:        "provider",
			Model:           "model",
			AccountLaneHash: "lane",
			StartedAt:       now,
			CompletedAt:     now.Add(time.Second),
			Outcome:         provider.AttemptOutcomeSuccess,
			Trigger:         provider.AttemptTriggerInitial,
			UpstreamReached: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(attemptReceiptHeader, string(encoded))
		io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()
	client.config.EnableAttemptReceipts = true
	client.config.Provider = "provider"
	client.config.Model = "model"
	client.config.AccountLaneHash = "lane"
	client.config.RouteSafety = provider.ReceiptRouteSafety()
	client.gatewayContractHealth = provider.GatewayContractHealthResult{State: provider.GatewayContractHealthHealthy, ReasonCode: "test_fixture"}
	client.now = func() time.Time { return now.Add(2 * time.Second) }
	_, err = client.Complete(context.Background(), provider.Request{ClientRequestID: "request-1"})
	var receiptErr *Error
	if !errors.As(err, &receiptErr) || receiptErr.Kind != ErrorAttemptReceiptsInvalid {
		t.Fatalf("invalid receipt error = %v, want typed invalid error", err)
	}
}
