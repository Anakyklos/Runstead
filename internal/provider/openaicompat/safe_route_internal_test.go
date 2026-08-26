package openaicompat

import (
	"context"
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
//  2. model-effect requests are non-replayable: bytes.Reader bodies keep
//     Request.GetBody nil, so no stdlib path can re-emit them from buffered
//     bytes without a new governor-admitted Complete call.
func TestSafeRouteTransportHasNoHiddenRetryPath(t *testing.T) {
	resolved := provider.Resolved{
		ProviderID:      "gateway-a",
		ProtocolFamily:  provider.FamilyOpenAICompatible,
		BaseURL:         "http://127.0.0.1:1",
		Model:           "model-a",
		AuthRequirement: provider.AuthNone,
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

	transport := client.HTTPTransport()
	if transport.ForceAttemptHTTP2 {
		t.Fatal("adapter transport enables ForceAttemptHTTP2; the stdlib h2 internal retry loop must stay unreachable")
	}
	for _, proto := range transport.TLSClientConfig.NextProtos {
		if proto == "h2" {
			t.Fatal("adapter transport advertises h2 via ALPN; HTTP/2 negotiation must stay disabled on the safe route")
		}
	}

	payload, payloadErr := encodeChatCompletionRequest(resolved.Model, "prompt")
	if payloadErr != nil {
		t.Fatal(payloadErr)
	}
	request, requestErr := newModelEffectRequest(context.Background(), "http://127.0.0.1:1/chat/completions", payload)
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
