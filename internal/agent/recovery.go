package agent

import "github.com/RenyEnnos/Runstead/internal/tools"

// RecoverySeed carries the reconstructed durable state of an interrupted task
// into a resumed loop run (issue #9). The loop consumes it as its initial
// state: it never replays historical model calls and never re-executes
// completed effects. The seed is produced by the recovery pipeline from
// authoritative persisted state plus environment reconciliation.
type RecoverySeed struct {
	// Turns and Attempts seed the run counters so the loop budgets (max
	// steps, provider budget) continue across restart instead of resetting.
	Turns    int
	Attempts int
	// Repeated seeds the repeated-action counter from historically rejected
	// actions so the repeated-action budget continues.
	Repeated int

	// Evidence seeds the grounding set from persisted citable observations
	// (tool_results). A final that cites a historical evidence ID is grounded
	// without re-executing the completed observation.
	Evidence []tools.Observation

	// Guard seeds the repeat guard with fingerprints of actions that were
	// actually executed, paired with the workspace signature recorded when the
	// action was accepted. An identical proposal is rejected only while the
	// workspace signature is unchanged; fingerprint equality remains loop
	// evidence, never an idempotency or result-reuse key.
	Guard map[string]string

	// Context is the bounded model-facing reconstruction summary appended to
	// the transcript under the recovery role. It carries verified progress,
	// unresolved failures, uncertain attempts, evidence IDs and recovery
	// constraints; it never contains hidden provider reasoning.
	Context string

	// TraceSequence continues the recovery trace numbering so the resumed
	// execution trace is contiguous with the pre-loop recovery lines.
	TraceSequence int
}
