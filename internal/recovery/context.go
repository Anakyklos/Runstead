package recovery

import (
	"errors"

	taskcontext "github.com/RenyEnnos/Runstead/internal/context"
	"github.com/RenyEnnos/Runstead/internal/state"
)

// Budget bounds the reconstructed model-facing recovery context. It is an
// alias of the context-compiler budget (issue #51): the compiler owns the
// deterministic budget semantics; recovery exposes the same type so existing
// callers keep their surface. The zero value means "use DefaultBudget".
type Budget = taskcontext.Budget

// DefaultBudget is the recovery context budget used by the CLI (issue #9
// values, #51 semantics).
func DefaultBudget() Budget {
	return taskcontext.DefaultBudget()
}

// Context is the bounded model-facing reconstruction summary produced by the
// evidence-preserving context compiler (issue #51). Text is the deterministic
// render of the typed projection; EvidenceIDs are the pinned citable ids used
// to seed grounding; Err is non-nil only when the mandatory content did not
// fit the budget (compile fail-closed, never a truncated projection).
type Context struct {
	// Text is the rendered context appended to the transcript under the
	// recovery role.
	Text string
	// EvidenceIDs are the citable evidence IDs available for grounding.
	EvidenceIDs []string
	// Chars is the rendered length in bytes; it is always <= Budget.
	Chars int
	// Err is set when the mandatory content could not fit inside the budget.
	// The caller must treat this as a fail-closed recovery failure before
	// any provider dispatch.
	Err error
	// Diagnostics carries the sanitized construction metadata of the
	// compiled projection for the recovery/trace path.
	Diagnostics taskcontext.Diagnostics
	// Compiled is the full typed projection. It is exposed for consumers
	// needing the authority/provenance structure; the model-facing context
	// is Text.
	Compiled *taskcontext.Compiled
}

// BuildContext projects the authoritative persisted snapshot into a bounded,
// deterministic model-facing recovery context (issue #51). The narrative
// prose summary of issue #9 is gone: Text is the deterministic render of the
// typed projection, and mandatory content that does not fit the budget fails
// explicitly (Context.Err) instead of truncating silently.
func BuildContext(snapshot *state.RecoverySnapshot, budget Budget, optional ...InputOption) Context {
	base := Input{Snapshot: snapshot, Budget: budget, WorkUnits: snapshot.WorkUnits}
	for _, apply := range optional {
		apply(&base)
	}
	compiled, err := (&taskcontext.Compiler{}).Compile(base)
	if err != nil {
		return Context{
			Err:         err,
			Diagnostics: compiled.Diagnostics,
		}
	}
	return Context{
		Text:        compiled.Text(),
		EvidenceIDs: compiled.EvidenceIDs(),
		Chars:       len(compiled.Text()),
		Diagnostics: compiled.Diagnostics,
		Compiled:    &compiled,
	}
}

// Input is the context-compiler input type.
type Input = taskcontext.Input

// InputOption configures the compiler input for one BuildContext call.
type InputOption func(*Input)

// WithPendingApprovals supplies the typed pending approval rows. A load error
// must be handled by the caller (fail closed), not silently converted into
// "none".
func WithPendingApprovals(approvals []state.PendingApproval) InputOption {
	return func(input *Input) {
		input.PendingApprovals = approvals
	}
}

// WithCurrentWorkspaceSignature supplies the workspace signature known at
// compile time so workspace-derived facts can be classified (current,
// needs-refresh or unverified-current).
func WithCurrentWorkspaceSignature(signature string) InputOption {
	return func(input *Input) {
		input.CurrentWorkspaceSignature = signature
	}
}

// IsBudgetExhausted reports whether err is the fail-closed context budget
// exhaustion sentinel.
func IsBudgetExhausted(err error) bool {
	return errors.Is(err, taskcontext.ErrBudgetExhausted)
}
