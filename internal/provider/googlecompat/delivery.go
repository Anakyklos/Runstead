package googlecompat

import (
	"net/http/httptrace"
	"sync"
	"time"

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
	firstByteAt      time.Time
	now              func() time.Time
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
		GotFirstResponseByte: func() { o.recordFirstResponseByte() },
	}
}

// recordFirstResponseByte keeps the first observed byte time: the first-token
// latency may never be guessed, so only the first proven observation counts.
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
