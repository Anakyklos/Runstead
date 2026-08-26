package anthropiccompat

import (
	"net/http/httptrace"
	"sync"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// deliveryObservation records only the observable transport facts that the
// standard library exposes through httptrace. It never interprets absence of
// evidence as proof of absence: a local write is not proof that the request
// reached the upstream, so WroteRequest yields sent_confirmed while any
// earlier transport progress yields sent_unconfirmed.
type deliveryObservation struct {
	mu               sync.Mutex
	wroteHeaders     bool
	wroteRequestBody bool
	responseStarted  bool
}

func (o *deliveryObservation) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() {
			o.mu.Lock()
			o.wroteHeaders = true
			o.mu.Unlock()
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			o.mu.Lock()
			o.wroteRequestBody = true
			o.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			o.markResponseStarted()
		},
	}
}

func (o *deliveryObservation) markResponseStarted() {
	o.mu.Lock()
	o.responseStarted = true
	o.mu.Unlock()
}

// stateAfterError conservatively maps an error after dispatch became possible
// onto the strongest delivery state the observed evidence supports.
func (o *deliveryObservation) stateAfterError() provider.DeliveryState {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.responseStarted:
		return provider.DeliveryResponseStarted
	case o.wroteRequestBody || o.wroteHeaders:
		return provider.DeliverySentConfirmed
	default:
		return provider.DeliverySentUnconfirmed
	}
}

// stateAfterBody maps body consumption onto the final delivery states.
func (o *deliveryObservation) stateAfterBody(readComplete bool) provider.DeliveryState {
	if readComplete {
		return provider.DeliveryCompleted
	}
	return o.stateAfterError()
}
