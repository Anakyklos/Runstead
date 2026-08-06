package governor

import (
	"math/rand"
	"sync"
	"time"
)

type waiter struct {
	request  AttemptRequest
	notify   chan struct{}
	checking bool
	removed  bool
}

type circuitData struct {
	state           CircuitState
	reason          OutcomeClass
	openUntil       time.Time
	refreshRequired bool
	refreshInFlight bool
	lastRateReset   time.Time
	rateEvents      []time.Time
}

type telemetryState struct {
	available         *int
	resetAt           time.Time
	cooldownUntil     time.Time
	rateLimited       bool
	capacityExhausted bool
	upstreamCircuit   UpstreamCircuitState
	unsafe            bool
}

type Governor struct {
	mu              sync.Mutex
	config          Config
	clock           Clock
	jitter          Jitter
	telemetrySource TelemetrySource
	events          EventSink

	closed              bool
	inFlight            bool
	queue               []*waiter
	currentTask         string
	consecutiveTurns    int
	lastStart           time.Time
	nextAttempt         int
	ledger              rollingLedger
	tasks               map[string]*taskState
	requestIDs          map[string]requestRecord
	attemptIDs          map[string]time.Time
	completedRequestIDs []completedRequest
	activeTaskID        string
	circuit             circuitData
	cooldownUntil       time.Time
	telemetry           telemetryState
	pendingEvents       []Event
	drainingEvents      bool
}

func New(config Config, options Options) (*Governor, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = realClock{}
	}
	jitter := options.Jitter
	if jitter == nil {
		jitter = &randomJitter{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	}
	return &Governor{
		config:          config,
		clock:           clock,
		jitter:          jitter,
		telemetrySource: options.Telemetry,
		events:          options.Events,
		nextAttempt:     1,
		tasks:           make(map[string]*taskState),
		requestIDs:      make(map[string]requestRecord),
		attemptIDs:      make(map[string]time.Time),
		circuit:         circuitData{state: CircuitClosed},
	}, nil
}

func (g *Governor) Config() Config {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.config
}

func (g *Governor) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	for _, entry := range g.queue {
		entry.removed = true
		g.signalLocked(entry)
	}
	g.queue = nil
	g.mu.Unlock()
}
