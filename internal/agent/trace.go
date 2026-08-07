package agent

import "time"

// TraceLine is one sanitized lifecycle event. It deliberately excludes
// prompts, response bodies, credentials, tokens, cookies and account
// identifiers; only sequence, kind, status, duration, evidence ID,
// classification and stop reason are carried.
type TraceLine struct {
	Sequence         int
	Kind             string
	Status           string
	Duration         time.Duration
	EvidenceID       string
	Classification   string
	StopReason       string
	Tool             string
	Code             string
	RetriesRemaining int
}

// Trace kinds: one lifecycle line per provider attempt, action, observation,
// correction, protocol deviation and terminal stop.
const (
	TraceAttempt     = "attempt"
	TraceAction      = "action"
	TraceObservation = "observation"
	TraceCorrection  = "correction"
	TraceDeviation   = "deviation"
	TraceStop        = "stop"
)

// TraceSink receives lifecycle lines. A nil sink is a no-op.
type TraceSink func(TraceLine)

func nopTrace(TraceLine) {}
