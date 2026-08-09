package agent

import "time"

// TraceLine is one sanitized lifecycle event. It deliberately excludes
// prompts, response bodies, credentials, tokens, cookies and account
// identifiers; only sequence, kind, status, duration, evidence ID,
// classification, stop reason and the real process exit code are carried.
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
	// ExitCode preserves the real process exit code of a run_recipe attempt
	// (negative when the process was terminated by a signal). Zero is unset.
	ExitCode int
}

// Trace kinds: one lifecycle line per provider attempt, action, observation,
// correction, protocol deviation, recovery transition and terminal stop.
const (
	TraceAttempt     = "attempt"
	TraceAction      = "action"
	TraceObservation = "observation"
	TraceCorrection  = "correction"
	TraceDeviation   = "deviation"
	TraceStop        = "stop"
	// TraceVerification is the control-plane verification of one completion
	// proposal (issue #11). Status is the typed verifier decision (passed,
	// failed, blocked, uncertain).
	TraceVerification = "verification"

	// Recovery trace kinds (issue #9): the pre-loop pipeline emits the
	// interruption, reconciliation and context-reconstruction lines, and the
	// loop emits the recovery boundary line when the resumed execution begins.
	TraceRecoveryStart     = "recovery_start"
	TraceRecoveryReconcile = "reconcile"
	TraceRecoveryUncertain = "recovery_uncertain"
	TraceRecoveryContext   = "recovery_context"
	TraceRecoveryBoundary  = "recovery_boundary"
	TraceRecoveryBlocked   = "recovery_blocked"
)

// TraceSink receives lifecycle lines. A nil sink is a no-op.
type TraceSink func(TraceLine)

func nopTrace(TraceLine) {}
