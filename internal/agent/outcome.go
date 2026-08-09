package agent

// Outcome is a typed terminal loop outcome. Every loop exit maps to exactly one
// Outcome and one stable process exit code; free-form error strings never
// replace the typed classification.
type Outcome string

const (
	// OutcomeCompleted means the model proposed completion and the runtime
	// verifier independently confirmed it: every cited evidence ID exists in
	// the task's persisted evidence, no uncertain effect or pending approval
	// remains, and every mandatory acceptance check passed against the real
	// environment. Completion is decided by the runtime, never by the model's
	// claim alone (issue #11).
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
	// OutcomePersistencePaused means a durable write failed AFTER a
	// potentially executed effect (TX 2 did not commit): the prepared attempt
	// is the uncertain-effect record and must stay reachable by the recovery
	// pipeline, so the task is NOT finalized. It stays durably resumable
	// (status running) and recovery reconciles the attempt from observable
	// state, or escalates it to human_review_required when that is
	// impossible (issue #13 review). It is a control-plane pause, never a
	// completed or failed terminal state.
	OutcomePersistencePaused Outcome = "persistence_paused"
	// OutcomeFinalIncomplete means the model emitted a grounded final with
	// status incomplete: the run ends honestly without claiming completion.
	OutcomeFinalIncomplete Outcome = "final_incomplete"
	// OutcomeApprovalRequired means the run paused because a write proposal
	// requires an operator approval that has not been recorded. The task is
	// NOT finalized: it stays durably resumable in status running, and the
	// operator decides the pending action with `runstead decide` before a
	// normal `runstead resume` continues. This is a control-plane dependency,
	// not a protocol correction: no correction budget is consumed and no
	// further provider attempt is made to wait for the operator.
	OutcomeApprovalRequired Outcome = "approval_required"
	// OutcomeVerificationBlocked means the run paused because completion was
	// refused by a control-plane verification dependency that is not a
	// model-correctable failure: an uncertain effect, a pending operator
	// approval at completion time, or an acceptance check that cannot be
	// evaluated. The task is NOT finalized: it stays durably resumable so the
	// operator can reconcile the effect or decide the approval before a
	// normal `runstead resume` continues.
	OutcomeVerificationBlocked Outcome = "verification_blocked"
	// OutcomeConsecutiveFailuresExhausted means the model produced more
	// consecutive failing tool/process observations (a failed tool
	// observation, or a run_recipe observation whose real exit code is
	// non-zero) than the configured allowance, with no successful observation
	// in between. Each failure already consumed a normal model/tool turn and
	// its correction was free to proceed; the guard stops the unproductive
	// repetition with a typed reason (issue #12). The task is finalized as a
	// terminal failure; a new task (or a corrective operator reset of the
	// workspace) is required.
	OutcomeConsecutiveFailuresExhausted Outcome = "consecutive_failures_exhausted"
	// OutcomeVerificationFailuresExhausted means the model proposed completion
	// more times than the configured allowance while the control-plane
	// verifier kept deciding failed. Each failed verification already returned
	// to execution as a structured observation; the guard stops the repeated
	// premature completion proposals with a typed reason (issue #12).
	OutcomeVerificationFailuresExhausted Outcome = "verification_failures_exhausted"
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
	case OutcomeApprovalRequired:
		return exitOutcomeBase + 12
	case OutcomeVerificationBlocked:
		return exitOutcomeBase + 13
	case OutcomeConsecutiveFailuresExhausted:
		return exitOutcomeBase + 14
	case OutcomeVerificationFailuresExhausted:
		return exitOutcomeBase + 15
	case OutcomePersistencePaused:
		return exitOutcomeBase + 16
	default:
		return exitUnknown
	}
}

// StopReason returns the canonical trace stop-reason string for an outcome.
func (o Outcome) StopReason() string {
	switch o {
	case OutcomeCompleted:
		return "completion verified by the control plane"
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
	case OutcomeApprovalRequired:
		return "write approval required"
	case OutcomeVerificationBlocked:
		return "completion refused by control-plane verification"
	case OutcomeConsecutiveFailuresExhausted:
		return "consecutive tool/process failures exhausted"
	case OutcomeVerificationFailuresExhausted:
		return "repeated verification failures exhausted"
	case OutcomePersistencePaused:
		return "durable write failed after a potentially executed effect; task remains resumable for recovery"
	default:
		return "unknown terminal outcome"
	}
}

// Result is the deterministic outcome of one loop run.
type Result struct {
	Outcome      Outcome
	StopReason   string
	Turns        int
	Attempts     int
	Observations int
	Corrections  int
	Repeated     int
	MixedProse   int
	// Summary is the VERIFIED completion summary of a completed task: it is
	// produced by the control-plane verifier from the acceptance checks and
	// authoritative evidence, never from model prose (issue #11 review). For
	// non-completed outcomes it may carry the model's final text where that is
	// honest (for example an incomplete final).
	Summary string
	// Note is the model's own final-response summary, kept EXPLICITLY
	// SEPARATE from the verified report. It is unverified free text: it never
	// enters the verifier, is never persisted as the task summary, and is
	// surfaced only as a labeled note so it can never be mistaken for a
	// verified completion claim.
	Note           string
	Evidence       []string
	Classification string
	Err            error
	// PendingActionID is set when Outcome is OutcomeApprovalRequired: the
	// write action the operator must decide with `runstead decide <task-id>
	// <action-id> approved|rejected`.
	PendingActionID string
}

func (r Result) terminal() bool {
	return r.Outcome != ""
}
