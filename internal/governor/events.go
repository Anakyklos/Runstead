package governor

func (g *Governor) queueEventLocked(event Event) {
	if g.events == nil {
		return
	}
	g.pendingEvents = append(g.pendingEvents, event)
}

// flushEvents drains complete event batches without holding the governor lock.
// A re-entrant sink can enqueue more events; the active drainer emits those
// events after the current batch and preserves state-transition order.
func (g *Governor) flushEvents() {
	for {
		g.mu.Lock()
		if g.events == nil || g.emittingEvents || len(g.pendingEvents) == 0 {
			g.mu.Unlock()
			return
		}
		g.emittingEvents = true
		events := append([]Event(nil), g.pendingEvents...)
		g.pendingEvents = nil
		sink := g.events
		g.mu.Unlock()

		for _, event := range events {
			sink.Emit(event)
		}

		g.mu.Lock()
		g.emittingEvents = false
		more := len(g.pendingEvents) != 0
		g.mu.Unlock()
		if !more {
			return
		}
	}
}
