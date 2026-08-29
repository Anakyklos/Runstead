package context

// Budget bounds the compiled model-facing task context (issue #51). The
// budget is deterministic and fail-closed: mandatory/pinned content is
// accounted before rendering and must fit inside MaxContextBytes, otherwise
// Compile fails with ErrBudgetExhausted (never a silent truncation).
// Degradable content (observation details, per-item detail lines) is selected
// in fixed order until the remaining budget is exhausted; every skipped item
// is recorded in Diagnostics.Omitted. The zero value means "use
// DefaultBudget".
type Budget struct {
	// MaxContextBytes caps the total rendered context bytes. It is enforced
	// before render: oversized mandatory content is a compile failure.
	MaxContextBytes int
	// MaxObservationCount caps how many observation content lines are
	// rendered (newest-first). Evidence IDs are always pinned regardless.
	MaxObservationCount int
	// MaxObservationChars caps one observation content line.
	MaxObservationChars int
	// MaxFailureLines caps detailed unresolved-failure lines. All failure
	// IDs stay pinned in the mandatory list regardless.
	MaxFailureLines int
	// MaxUncertainLines caps detailed uncertain-effect lines. All uncertain
	// IDs stay pinned regardless.
	MaxUncertainLines int
	// MaxApprovalLines caps detailed pending-approval lines. All approval
	// IDs stay pinned regardless.
	MaxApprovalLines int
	// MaxVerificationLines caps detailed verification lines. The latest
	// decision stays pinned regardless.
	MaxVerificationLines int
}

// DefaultBudget is the deterministic compiler budget used by the CLI. Values
// carry over from the recovery context budget (issue #9) with the pinned
// semantics of #51.
func DefaultBudget() Budget {
	return Budget{
		MaxContextBytes:      32 << 10,
		MaxObservationCount:  8,
		MaxObservationChars:  4 << 10,
		MaxFailureLines:      32,
		MaxUncertainLines:    16,
		MaxApprovalLines:     16,
		MaxVerificationLines: 8,
	}
}

// Validate rejects an explicitly non-positive context ceiling. The zero value
// is the documented "use defaults" fallback and is not a validation error.
func (b Budget) Validate() error {
	if b.MaxContextBytes < 0 {
		return errNegativeBudget
	}
	return nil
}
