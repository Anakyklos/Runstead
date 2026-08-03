package policy

import "time"

type ledgerEvent struct {
	at     time.Time
	taskID string
}

type rollingLedger struct {
	events []ledgerEvent
	head   int
}

func (l *rollingLedger) add(at time.Time, taskID string) {
	l.events = append(l.events, ledgerEvent{at: at, taskID: taskID})
}

func (l *rollingLedger) expire(now time.Time, maxWindow time.Duration) {
	cutoff := now.Add(-maxWindow)
	for l.head < len(l.events) && !l.events[l.head].at.After(cutoff) {
		l.head++
	}
	if l.head > 0 && (l.head >= 256 || l.head*2 >= len(l.events)) {
		l.events = append([]ledgerEvent(nil), l.events[l.head:]...)
		l.head = 0
	}
}

func (l *rollingLedger) count(now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	count := 0
	for _, event := range l.events[l.head:] {
		if event.at.After(cutoff) {
			count++
		}
	}
	return count
}

func (l *rollingLedger) next(now time.Time, window time.Duration, ceiling int) time.Time {
	if l.count(now, window) < ceiling {
		return time.Time{}
	}
	cutoff := now.Add(-window)
	for _, event := range l.events[l.head:] {
		if event.at.After(cutoff) {
			return event.at.Add(window)
		}
	}
	return time.Time{}
}

func (l *rollingLedger) copyEvents() []ledgerEvent {
	return append([]ledgerEvent(nil), l.events[l.head:]...)
}
