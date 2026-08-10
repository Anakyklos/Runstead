package omniroute

import (
	"net/http/httptrace"
	"sync"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// deliveryObservation records only the narrow transport observations needed by
// issue #38. A successful local HTTP write is not proof that OmniRoute
// dispatched a model attempt, so WroteRequest never produces sent_confirmed.
type deliveryObservation struct {
	mu              sync.Mutex
	writeObserved   bool
	responseStarted bool
}

func (o *deliveryObservation) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			o.mu.Lock()
			o.writeObserved = true
			o.mu.Unlock()
		},
		GotFirstResponseByte: func() { o.markResponseStarted() },
	}
}

func (o *deliveryObservation) markResponseStarted() {
	o.mu.Lock()
	o.responseStarted = true
	o.mu.Unlock()
}

func (o *deliveryObservation) stateAfterError() provider.DeliveryState {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.responseStarted {
		return provider.DeliveryResponseStarted
	}
	return provider.DeliverySentUnconfirmed
}

func (o *deliveryObservation) stateAfterBody(readComplete bool) provider.DeliveryState {
	if readComplete {
		return provider.DeliveryCompleted
	}
	return o.stateAfterError()
}
