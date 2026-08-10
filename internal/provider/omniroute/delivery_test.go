package omniroute

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

func TestCompleteOnceCanceledBeforeDoIsNotSent(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("chat handler was reached after pre-dispatch cancellation")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := client.completeOnce(ctx, provider.Request{Prompt: "prompt"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("completeOnce() error = %v, want context cancellation", err)
	}
	if response.Metadata.DeliveryState != provider.DeliveryNotSent {
		t.Fatalf("delivery state = %v, want not_sent", response.Metadata.DeliveryState)
	}
}

func TestCompleteOnceRoundTripErrorWithoutAuthorityIsSentUnconfirmed(t *testing.T) {
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("connection reset by peer")
		},
	}
	client, err := New(testConfig("http://127.0.0.1:1"), Options{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}

	response, callErr := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	if callErr == nil {
		t.Fatal("completeOnce() error = nil, want transport error")
	}
	if response.Metadata.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("delivery state = %v, want sent_unconfirmed", response.Metadata.DeliveryState)
	}
}

func TestWroteRequestDoesNotConfirmUpstreamModelDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("server does not support hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	response, callErr := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	if callErr == nil {
		t.Fatal("completeOnce() error = nil, want connection error")
	}
	if response.Metadata.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("delivery state = %v, want sent_unconfirmed", response.Metadata.DeliveryState)
	}
}

func TestPartialResponseIsResponseStarted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("server does not support hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 50\r\n\r\n{\"choices\":[")
		_ = connection.Close()
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	response, callErr := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	if callErr == nil {
		t.Fatal("completeOnce() error = nil, want partial response read error")
	}
	if response.Metadata.DeliveryState != provider.DeliveryResponseStarted {
		t.Fatalf("delivery state = %v, want response_started", response.Metadata.DeliveryState)
	}
}

func TestCompleteValidResponseIsCompleted(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()

	response, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery state = %v, want completed", response.Metadata.DeliveryState)
	}
}

func TestCompleteHTTPFailureBodyIsCompleted(t *testing.T) {
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"upstream failure"}`)
	}))
	defer server.Close()

	response, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
	if err == nil {
		t.Fatal("completeOnce() error = nil, want HTTP error")
	}
	if response.Metadata.DeliveryState != provider.DeliveryCompleted {
		t.Fatalf("delivery state = %v, want completed", response.Metadata.DeliveryState)
	}
}

func TestCompleteEmptyAndMalformedBodiesAreCompleted(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "malformed", body: "not-json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			response, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt"})
			if err == nil {
				t.Fatal("completeOnce() error = nil, want response classification error")
			}
			if response.Metadata.DeliveryState != provider.DeliveryCompleted {
				t.Fatalf("delivery state = %v, want completed", response.Metadata.DeliveryState)
			}
		})
	}
}

func TestCancelBeforeAndAfterPossibleDispatchHaveDifferentStates(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan provider.Response, 1)
	errorsCh := make(chan error, 1)
	go func() {
		response, callErr := client.completeOnce(ctx, provider.Request{Prompt: "prompt"})
		result <- response
		errorsCh <- callErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not observe the possible dispatch")
	}
	cancel()
	response := <-result
	callErr := <-errorsCh
	if callErr == nil {
		t.Fatal("completeOnce() error = nil, want cancellation")
	}
	if response.Metadata.DeliveryState != provider.DeliverySentUnconfirmed {
		t.Fatalf("delivery state = %v, want sent_unconfirmed", response.Metadata.DeliveryState)
	}
}

func TestClientRequestIDPropagatesWithoutIdempotencyKey(t *testing.T) {
	var requestID, idempotencyKey string
	client, server := newTransportClient(t, safeHandler(func(w http.ResponseWriter, r *http.Request) {
		requestID = r.Header.Get(clientRequestIDHeader)
		idempotencyKey = r.Header.Get("Idempotency-Key")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"response"}}]}`)
	}))
	defer server.Close()

	if _, err := client.completeOnce(context.Background(), provider.Request{Prompt: "prompt", ClientRequestID: "request-38"}); err != nil {
		t.Fatal(err)
	}
	if requestID != "request-38" {
		t.Fatalf("client request ID = %q, want request-38", requestID)
	}
	if strings.TrimSpace(idempotencyKey) != "" {
		t.Fatalf("unexpected Idempotency-Key header = %q", idempotencyKey)
	}
}
