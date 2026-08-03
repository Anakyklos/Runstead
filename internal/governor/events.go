package governor

func (g *Governor) queueEventLocked(event Event) {
	if g.events == nil {
		return
	}
	g.pendingEvents = append(g.pendingEvents, event)
}

// DrainEvents delivers the pending batch without holding the governor lock.
// Governor operations only enqueue events; callers choose when to perform
// potentially blocking I/O. All current event kinds are mandatory, so a
// re-entrant sink leaves newly queued events for a later drain rather than
// dropping them.
func (g *Governor) DrainEvents() int {
	g.mu.Lock()
	if g.events == nil || g.drainingEvents || len(g.pendingEvents) == 0 {
		g.mu.Unlock()
		return 0
	}
	g.drainingEvents = true
	g.mu.Unlock()

	drained := 0
	completed := false
	defer func() {
		if !completed {
			g.mu.Lock()
			g.drainingEvents = false
			g.mu.Unlock()
		}
	}()
	for {
		g.mu.Lock()
		if len(g.pendingEvents) == 0 {
			g.drainingEvents = false
			completed = true
			g.mu.Unlock()
			return drained
		}
		events := append([]Event(nil), g.pendingEvents...)
		g.pendingEvents = nil
		sink := g.events
		g.mu.Unlock()

		for _, event := range events {
			sink.Emit(event)
		}
		drained += len(events)
	}
}
