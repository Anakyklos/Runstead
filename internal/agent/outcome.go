package agent

// Outcome is a typed terminal loop outcome. Every loop exit maps to exactly one
// Outcome and one stable process exit code; free-form error strings never
// replace the typed classification.
type Outcome string

const (
	// OutcomeCompleted means the model emitted a grounded final with status
	// complete: every cited evidence ID was produced by a successful tool
	// observation in this run.
	OutcomeCompleted Outcome = "completed"
	// OutcomeStepsExhausted means the configured model-turn budget was used
	// before a terminal final was accepted.
	OutcomeStepsExhausted Outcome = "steps_exhausted"
	// OutcomeCorrectionsExhausted means the model exhausted the configured
	// protocol-correction attempts without producing an accepted envelope.
	OutcomeCorrectionsExhausted Outcome = "corrections_exhausted"
	// OutcomeRepeatedAction means the model repeated an identical action
	// without an intervening workspace change beyond the configured guard.
	OutcomeRepeatedAction Outcome = "repeated_action"
	// OutcomeTimeBudgetExhausted means the task deadline elapsed outside
	// account-lane delay (between turns, during provider I/O or tool work).
	OutcomeTimeBudgetExhausted Outcome = "time_budget_exhausted"
	// OutcomeProviderBudgetExhausted means the task provider-attempt budget or
	// the account governor task/rolling budgets were exhausted.
	OutcomeProviderBudgetExhausted Outcome = "provider_budget_exhausted"
	// OutcomeAccountDelayTimeout means the task deadline elapsed while the
	// account governor was delaying admission (lane busy, pacing, cooldown,
	// rolling-window exhaustion or circuit wait).
	OutcomeAccountDelayTimeout Outcome = "account_delay_timeout"
	// OutcomeAccountCircuitOpen means admission was refused because the
	// account circuit was open or required human acknowledgement.
	OutcomeAccountCircuitOpen Outcome = "account_circuit_open"
	// OutcomeCanceled means the task context was canceled (one-shot signal).
	OutcomeCanceled Outcome = "canceled"
	// OutcomeFinalNotGrounded means a final response cited evidence IDs that
	// were not produced by successful observations in this run.
	OutcomeFinalNotGrounded Outcome = "final_not_grounded"
	// OutcomeProviderFailure means the governed provider attempt failed or
	// returned an unusable response; the concrete classification is preserved
	// in the stop reason.
	OutcomeProviderFailure Outcome = "provider_failure"
	// OutcomePersistenceFailure means the run had to stop because durable
	// state could not be maintained (issue #8). Persisted state is
	// authoritative, so a failed projection/journal write mid-run is a
	// terminal condition rather than something to silently continue past.
	OutcomePersistenceFailure Outcome = "persistence_failure"
	// OutcomeFinalIncomplete means the model emitted a grounded final with
	// status incomplete: the run ends honestly without claiming completion.
	OutcomeFinalIncomplete Outcome = "final_incomplete"
)

// exitSuccess, exitCanceled and the usage/unavailable codes are shared with the
// CLI composition root; every other outcome owns a distinct stable code.
// exitOutcomeBase + 10 (30) is the reserved code for unrecognized outcomes;
// persistence_failure uses exitOutcomeBase + 11 (31).
const (
	exitSuccess     = 0
	exitCanceled    = 130
	exitUnknown     = 30
	exitOutcomeBase = 20
)

// ExitCode returns the stable process exit code for a terminal outcome.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeCompleted:
		return exitSuccess
	case OutcomeCanceled:
		return exitCanceled
	case OutcomeStepsExhausted:
		return exitOutcomeBase
	case OutcomeCorrectionsExhausted:
		return exitOutcomeBase + 1
	case OutcomeRepeatedAction:
		return exitOutcomeBase + 2
	case OutcomeTimeBudgetExhausted:
		return exitOutcomeBase + 3
	case OutcomeProviderBudgetExhausted:
		return exitOutcomeBase + 4
	case OutcomeAccountDelayTimeout:
		return exitOutcomeBase + 5
	case OutcomeAccountCircuitOpen:
		return exitOutcomeBase + 6
	case OutcomeFinalNotGrounded:
		return exitOutcomeBase + 7
	case OutcomeProviderFailure:
		return exitOutcomeBase + 8
	case OutcomePersistenceFailure:
		return exitOutcomeBase + 11
	case OutcomeFinalIncomplete:
		return exitOutcomeBase + 9
	default:
		return exitUnknown
	}
}

// StopReason returns the canonical trace stop-reason string for an outcome.
func (o Outcome) StopReason() string {
	switch o {
	case OutcomeCompleted:
		return "grounded final accepted"
	case OutcomeStepsExhausted:
		return "model-turn budget exhausted"
	case OutcomeCorrectionsExhausted:
		return "protocol correction attempts exhausted"
	case OutcomeRepeatedAction:
		return "repeated action guard exceeded"
	case OutcomeTimeBudgetExhausted:
		return "task time budget exhausted"
	case OutcomeProviderBudgetExhausted:
		return "provider attempt budget exhausted"
	case OutcomeAccountDelayTimeout:
		return "account lane delay exceeded task deadline"
	case OutcomeAccountCircuitOpen:
		return "account circuit open"
	case OutcomeCanceled:
		return "task canceled"
	case OutcomeFinalNotGrounded:
		return "final evidence not grounded"
	case OutcomeProviderFailure:
		return "provider attempt failed"
	case OutcomePersistenceFailure:
		return "durable state could not be persisted"
	case OutcomeFinalIncomplete:
		return "grounded final reported incomplete"
	default:
		return "unknown terminal outcome"
	}
}

// Result is the deterministic outcome of one loop run.
type Result struct {
	Outcome        Outcome
	StopReason     string
	Turns          int
	Attempts       int
	Observations   int
	Corrections    int
	Repeated       int
	MixedProse     int
	Summary        string
	Evidence       []string
	Classification string
	Err            error
}

func (r Result) terminal() bool {
	return r.Outcome != ""
}
