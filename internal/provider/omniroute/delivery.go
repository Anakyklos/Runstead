package omniroute

import (
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

// deliveryObservation records only the narrow transport observations needed by
// issue #38. A successful local HTTP write is not proof that OmniRoute
// dispatched a model attempt, so WroteRequest never produces sent_confirmed.
type deliveryObservation struct {
	mu              sync.Mutex
	writeObserved   bool
	responseStarted bool
	firstByteAt     time.Time
	now             func() time.Time
}

func (o *deliveryObservation) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			o.mu.Lock()
			o.writeObserved = true
			o.mu.Unlock()
		},
		GotFirstResponseByte: func() { o.recordFirstResponseByte() },
	}
}

// recordFirstResponseByte keeps the first observed byte time: the
// first-byte latency may never be guessed, so only the first proven
// observation counts.
func (o *deliveryObservation) recordFirstResponseByte() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.firstByteAt.IsZero() && o.now != nil {
		o.firstByteAt = o.now()
	}
	o.responseStarted = true
}

// firstByteLatency returns the proven started-to-first-byte delta, or zero
// when no first byte was observed. The latency is the HTTP response-header
// arrival, never a model-token claim (#39 maintainer review).
func (o *deliveryObservation) firstByteLatency(started time.Time) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.firstByteAt.IsZero() {
		return 0
	}
	latency := o.firstByteAt.Sub(started)
	if latency < 0 {
		return 0
	}
	return latency
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
