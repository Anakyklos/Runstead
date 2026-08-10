package omniroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// The mock deliberately exposes only the completion route and management
// endpoints consumed by this adapter. It is not an OmniRoute emulator.
type contractMockResponse struct {
	status    int
	body      []byte
	headers   http.Header
	receipt   []byte
	transport contractTransportSpec
}

type contractMockConfig struct {
	completion         contractMockResponse
	management         map[string]contractMockResponse
	managementDefaults map[string]contractMockResponse
	completionHandler  http.HandlerFunc
}

type contractMockCounts struct {
	total           atomic.Int32
	chatPosts       atomic.Int32
	managementGets  atomic.Int32
	redirectReplays atomic.Int32
}

type contractRequestRecord struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

type contractMock struct {
	completion        contractMockResponse
	management        map[string]contractMockResponse
	completionHandler http.HandlerFunc
	counts            contractMockCounts
	mu                sync.Mutex
	requests          []contractRequestRecord
}

type contractMockServer struct {
	server *httptest.Server
	mock   *contractMock
}

var safeResilienceResponse = mustReadContractFixture("management/resilience-safe.json")

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL + "/v1",
		APIKey:           "fixture-api-key",
		Model:            "chatgpt-web/model",
		Timeout:          time.Second,
		MaxRequestBytes:  1 << 20,
		MaxResponseBytes: 1 << 20,
		RouteSafety:      provider.SafeRouteSafety(),
	}
}

func newTransportClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := New(testConfig(server.URL), Options{HTTPClient: server.Client()})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func safeHandler(chat http.HandlerFunc) http.Handler {
	mock := &contractMock{
		completionHandler: chat,
		management:        safeContractManagementResponses(),
	}
	return mock
}

func newContractMockServer(t *testing.T, config contractMockConfig) *contractMockServer {
	t.Helper()
	management := config.managementDefaults
	if management == nil {
		management = safeContractManagementResponses()
	}
	for endpoint, response := range config.management {
		management[endpoint] = response
	}
	if config.completion.status == 0 {
		config.completion.status = http.StatusOK
	}
	mock := &contractMock{
		completion:        config.completion,
		management:        management,
		completionHandler: config.completionHandler,
	}
	return &contractMockServer{server: httptest.NewServer(mock), mock: mock}
}

func safeContractManagementResponses() map[string]contractMockResponse {
	manifest, err := loadContractManifest(contractFixtureFS())
	if err != nil {
		panic(fmt.Sprintf("load contract management defaults: %v", err))
	}
	responses := make(map[string]contractMockResponse, len(manifest.ManagementDefaults))
	for endpoint, fixture := range manifest.ManagementDefaults {
		responses[endpoint] = contractMockResponse{
			status: http.StatusOK,
			body:   []byte(mustReadContractFixture(fixture)),
		}
	}
	return responses
}

func (s *contractMockServer) URL() string { return s.server.URL }

func (s *contractMockServer) Client() *http.Client { return s.server.Client() }

func (s *contractMockServer) Close() { s.server.Close() }

func (s *contractMockServer) Counts() map[string]int {
	return map[string]int{
		"total":            int(s.mock.counts.total.Load()),
		"chat_posts":       int(s.mock.counts.chatPosts.Load()),
		"management_gets":  int(s.mock.counts.managementGets.Load()),
		"redirect_replays": int(s.mock.counts.redirectReplays.Load()),
	}
}

func (s *contractMockServer) Requests() []contractRequestRecord {
	s.mock.mu.Lock()
	defer s.mock.mu.Unlock()
	requests := make([]contractRequestRecord, len(s.mock.requests))
	for index, request := range s.mock.requests {
		requests[index] = contractRequestRecord{
			Method:  request.Method,
			Path:    request.Path,
			Headers: request.Headers.Clone(),
			Body:    append([]byte(nil), request.Body...),
		}
	}
	return requests
}

func (m *contractMock) recordRequest(r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	m.mu.Lock()
	m.requests = append(m.requests, contractRequestRecord{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: r.Header.Clone(),
		Body:    append([]byte(nil), body...),
	})
	m.mu.Unlock()
}

func (m *contractMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.recordRequest(r)
	m.counts.total.Add(1)
	if r.URL.Path == "/v1/chat/completions" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.counts.chatPosts.Add(1)
		if m.completionHandler != nil {
			m.completionHandler.ServeHTTP(w, r)
			return
		}
		m.serveResponse(w, r, m.completion)
		return
	}
	if r.URL.Path == "/v1/chat/completions-replayed" {
		m.counts.redirectReplays.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"replayed response"}}]}`)
		return
	}
	for endpoint, path := range contractManagementEndpoints {
		if r.URL.Path != path {
			continue
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m.counts.managementGets.Add(1)
		response, ok := m.management[endpoint]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.serveResponse(w, r, response)
		return
	}
	http.NotFound(w, r)
}

func (m *contractMock) serveResponse(w http.ResponseWriter, r *http.Request, response contractMockResponse) {
	for key, values := range response.headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if response.receipt != nil {
		w.Header().Set(attemptReceiptHeader, string(response.receipt))
	}
	if response.transport.Kind == "redirect" {
		location := response.transport.Location
		if location == "" {
			location = w.Header().Get("Location")
		}
		if location != "" {
			w.Header().Set("Location", location)
		}
	}
	if response.transport.Kind == "delay" {
		timer := time.NewTimer(time.Duration(response.transport.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
	}
	if response.transport.Kind == "close_connection" {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	body := response.body
	if response.transport.Kind == "oversized_response" {
		body = []byte(strings.Repeat("x", response.transport.Bytes))
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func contractDialTransport(kind string, attempts *atomic.Int32) *http.Transport {
	return &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			attempts.Add(1)
			if kind == "connection_reset" {
				return nil, syscall.ECONNRESET
			}
			return nil, errors.New("synthetic transport failure")
		},
	}
}

func providerRequestForContractTest() provider.Request {
	return provider.Request{Prompt: "synthetic redirect prompt"}
}

func TestContractMockCountsRedirectWithoutReplay(t *testing.T) {
	server := newContractMockServer(t, contractMockConfig{
		completion: contractMockResponse{
			status:  http.StatusTemporaryRedirect,
			headers: http.Header{"Location": []string{"/v1/chat/completions-replayed"}},
		},
	})
	defer server.Close()

	client, err := New(testConfig(server.URL()), Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.completeOnce(context.Background(), providerRequestForContractTest())
	counts := server.Counts()
	if counts["chat_posts"] != 1 || counts["redirect_replays"] != 0 {
		t.Fatalf("redirect request counts = %#v, want one POST and no replay", counts)
	}
	requests := server.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodPost || requests[0].Path != "/v1/chat/completions" {
		t.Fatalf("recorded requests = %#v, want one completion POST", requests)
	}
}
