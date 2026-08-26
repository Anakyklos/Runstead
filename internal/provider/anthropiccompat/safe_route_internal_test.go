package anthropiccompat

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// TestSafeRouteTransportHasNoHiddenRetryPath pins the structural facts that
// make a stdlib hidden retry impossible on the safe route (see the external
// end-to-end coverage in delivery_test.go):
//
//  1. HTTP/2 can never be negotiated: NextProtos pinned to http/1.1 and
//     ForceAttemptHTTP2 false, so the h2_bundle.go internal retry loop is
//     unreachable;
//  2. model-effect requests are non-replayable: wrapped bodies keep
//     Request.GetBody nil, so no stdlib path can re-emit them from buffered
//     bytes without a new governor-admitted Complete call;
//  3. no ambient proxy is inherited, so HTTP_PROXY/HTTPS_PROXY cannot insert
//     an opaque retry/fallback/routing hop this single-attempt route cannot
//     observe.
func TestSafeRouteTransportHasNoHiddenRetryPath(t *testing.T) {
	resolved := provider.Resolved{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyAnthropicCompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
		Options: map[string]string{
			"max_tokens":        "1024",
			"anthropic_version": "2023-06-01",
		},
		Profile: provider.CapabilityProfile{
			ProfileVersion: "v1",
			RouteSafety:    provider.SafeRouteSafety(),
		},
		ConfigIdentity: "identity",
	}
	client, err := New(resolved, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Package-private inspection: the production surface never exposes this
	// pointer (see TestNoExportedSurfaceCanMutateOrReplaceTheTransport), so
	// mutability cannot escape New.
	transport := client.httpClient.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("adapter transport inherits an implicit proxy; ambient HTTP_PROXY/HTTPS_PROXY could insert opaque retry/fallback/routing that this single-attempt route cannot observe")
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("adapter transport enables ForceAttemptHTTP2; the stdlib h2 internal retry loop must stay unreachable")
	}
	for _, proto := range transport.TLSClientConfig.NextProtos {
		if proto == "h2" {
			t.Fatal("adapter transport advertises h2 via ALPN; HTTP/2 negotiation must stay disabled on the safe route")
		}
	}
	if client.httpClient.CheckRedirect == nil {
		t.Fatal("adapter client follows redirects; a 3xx could replay the model-effect request without admission")
	}

	payload, payloadErr := encodeMessagesRequest(resolved.Model, 1024, "prompt")
	if payloadErr != nil {
		t.Fatal(payloadErr)
	}
	request, requestErr := newModelEffectRequest(context.Background(), "http://127.0.0.1:1/v1/messages", payload)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if request.GetBody != nil {
		t.Fatal("model-effect request is replayable (GetBody != nil); the stdlib could re-emit it without governor admission")
	}
	if request.Body == nil {
		t.Fatal("model-effect request lost its body; wire contract broken")
	}
}

// TestNoExportedSurfaceCanMutateOrReplaceTheTransport pins the #87 review
// blocker carried into #88: Client must not export ANY way to read, mutate or
// replace the adapter-owned dispatch transport (which would let a caller
// re-open amplification knobs after New while the client keeps announcing
// SafeRouteSafety). The proof lives in the internal package, where the
// unexported field itself is inspected instead.
func TestNoExportedSurfaceCanMutateOrReplaceTheTransport(t *testing.T) {
	clientType := reflect.TypeOf((*Client)(nil))
	for i := 0; i < clientType.NumMethod(); i++ {
		method := clientType.Method(i)
		if !method.IsExported() {
			continue
		}
		methodType := method.Type
		for param := 0; param < methodType.NumIn(); param++ {
			if isMutableTransportType(methodType.In(param)) {
				t.Fatalf("exported method %s accepts a mutable transport type %v; injection surface must never exist", method.Name, methodType.In(param))
			}
		}
		for result := 0; result < methodType.NumOut(); result++ {
			if isMutableTransportType(methodType.Out(result)) {
				t.Fatalf("exported method %s returns a mutable transport type %v; the adapter-owned stack must stay private after New", method.Name, methodType.Out(result))
			}
		}
	}
	clientFields := reflect.TypeOf(Client{})
	for i := 0; i < clientFields.NumField(); i++ {
		field := clientFields.Field(i)
		if field.IsExported() {
			t.Fatalf("Client exposes exported field %s; a caller could mutate adapter state", field.Name)
		}
	}
}

func isMutableTransportType(typ reflect.Type) bool {
	switch {
	case typ == reflect.TypeOf((*http.Transport)(nil)):
		return true
	case typ == reflect.TypeOf((*http.Client)(nil)):
		return true
	case typ == reflect.TypeOf((*http.RoundTripper)(nil)).Elem():
		return true
	case typ.Implements(reflect.TypeOf((*http.RoundTripper)(nil)).Elem()):
		return true
	default:
		return false
	}
}

// TestResolveProtocolOptionsClosedVocabulary pins the fail-closed option
// contract at the unit level: only the two strictly necessary option keys
// exist, values are validated, and nothing can be silently defaulted.
func TestResolveProtocolOptionsClosedVocabulary(t *testing.T) {
	maxTokens, version, err := resolveProtocolOptions(map[string]string{
		"max_tokens":        "2048",
		"anthropic_version": "2023-06-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if maxTokens != 2048 || version != "2023-06-01" {
		t.Fatalf("resolved (%d, %q), want (2048, 2023-06-01)", maxTokens, version)
	}
	for _, invalid := range []map[string]string{
		nil,
		{"max_tokens": "1024"},
		{"anthropic_version": "2023-06-01"},
		{"max_tokens": "1024", "anthropic_version": "2023-06-01", "stream": "true"},
		{"max_tokens": "0", "anthropic_version": "2023-06-01"},
		{"max_tokens": "1024", "anthropic_version": "not-a-version"},
	} {
		if _, _, err := resolveProtocolOptions(invalid); err == nil {
			t.Fatalf("resolveProtocolOptions accepted %v", invalid)
		}
	}
}
