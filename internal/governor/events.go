package governor

const maxPendingEvents = 256

func (g *Governor) queueEventLocked(event Event) {
	if g.events == nil {
		return
	}
	if len(g.pendingEvents) >= maxPendingEvents {
		g.droppedEvents++
		return
	}
	g.pendingEvents = append(g.pendingEvents, event)
}

// DrainEvents delivers one bounded batch without holding the governor lock.
// Governor operations only enqueue events; callers choose when to perform
// potentially blocking I/O. A re-entrant sink leaves newly queued events for
// a later drain, preserving the lane's non-blocking behavior.
func (g *Governor) DrainEvents() int {
	g.mu.Lock()
	if g.events == nil || g.drainingEvents || len(g.pendingEvents) == 0 {
		g.mu.Unlock()
		return 0
	}
	g.drainingEvents = true
	events := append([]Event(nil), g.pendingEvents...)
	g.pendingEvents = nil
	sink := g.events
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		g.drainingEvents = false
		g.mu.Unlock()
	}()
	for _, event := range events {
		sink.Emit(event)
	}
	return len(events)
}
