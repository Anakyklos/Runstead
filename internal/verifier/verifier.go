package verifier

import (
	"time"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// Decision is the typed completion decision of one verification attempt. It is
// not a bool: the distinction between failed, blocked and uncertain is
// durable and explains why completion was refused.
type Decision string

const (
	// DecisionPassed means every structural check and every mandatory
	// acceptance check passed: completion is permitted.
	DecisionPassed Decision = "passed"
	// DecisionFailed means at least one mandatory check failed against
	// authoritative state: completion is refused and the structured result is
	// returned to execution as a bounded observation.
	DecisionFailed Decision = "failed"
	// DecisionBlocked means completion is refused by a control-plane
	// dependency that is not a model-correctable failure: a pending operator
	// approval or an acceptance check that cannot be evaluated yet.
	DecisionBlocked Decision = "blocked"
	// DecisionUncertain means an authoritative effect is uncertain (an
	// interrupted/uncertain attempt or human-review-required state):
	// completion is refused until the effect is reconciled.
	DecisionUncertain Decision = "uncertain"
)

// CheckStatus is the typed status of one check evaluation.
type CheckStatus string

const (
	CheckPassed    CheckStatus = "passed"
	CheckFailed    CheckStatus = "failed"
	CheckBlocked   CheckStatus = "blocked"
	CheckUncertain CheckStatus = "uncertain"
)

// CheckResult is the result of one check evaluation. Expected and Observed
// are bounded, sanitized descriptions; they never carry raw file contents or
// model text.
type CheckResult struct {
	// ID is the stable check id (plan check id, or a structural check id).
	ID string `json:"id"`
	// Type is the typed check kind.
	Type string `json:"type"`
	// Status is the typed evaluation status.
	Status CheckStatus `json:"status"`
	// Expected is the bounded description of the condition required.
	Expected string `json:"expected,omitempty"`
	// Observed is the bounded description of what the environment showed.
	Observed string `json:"observed,omitempty"`
	// EvidenceIDs are the persisted evidence IDs used by this check.
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	// Reason is the typed failure/block reason.
	Reason string `json:"reason,omitempty"`
}

// ChangedFile is one authoritative changed-file observation derived from the
// real git observation (never from the model response).
type ChangedFile struct {
	// Path is the normalized relative path.
	Path string `json:"path"`
	// Status is the git status code pair (for example " M", "??", "A ").
	Status string `json:"status,omitempty"`
}

// GitObservation is the bounded real git state observed at verification time
// plus the baseline captured at task start. It distinguishes pre-existing
// changes from changes produced during the task "where practical": git cannot
// attribute a concurrent external edit, so the report is honest about what
// the baseline shows.
type GitObservation struct {
	// CurrentStatus is the raw bounded `git status --short` output.
	CurrentStatus string `json:"current_status"`
	// CurrentDiff is the raw bounded `git diff` output.
	CurrentDiff string `json:"current_diff,omitempty"`
	// Truncated reports that one of the bounded git observations was
	// truncated.
	Truncated bool `json:"truncated"`
	// Available reports whether the workspace is a git repository and git
	// observation succeeded.
	Available bool `json:"available"`
	// Failure is the typed reason when git observation failed.
	Failure string `json:"failure,omitempty"`
	// PreExisting are the files already changed when the task started
	// (baseline), never attributed to the task.
	PreExisting []ChangedFile `json:"pre_existing,omitempty"`
	// DuringTask are the files changed during the task: present now but not
	// in the baseline, or in the baseline with a different status now.
	DuringTask []ChangedFile `json:"during_task,omitempty"`
	// BaselineTruncated reports that the bounded git baseline captured at task
	// start was truncated, so pre-existing changes outside the truncated
	// baseline window may be attributed as during_task. The limitation is
	// recorded explicitly, never silently ignored (issue #11 review).
	BaselineTruncated bool `json:"baseline_truncated,omitempty"`
}

// Report is the structured verification report of one verification attempt.
// It is persisted and inspectable; the human CLI output is a bounded summary
// of this structure.
type Report struct {
	// TaskID is the verified task.
	TaskID string `json:"task_id"`
	// AttemptID is the durable verification attempt identity (verif-NNNNNN).
	AttemptID string `json:"attempt_id"`
	// Decision is the typed completion decision.
	Decision Decision `json:"decision"`
	// Summary is the bounded one-line decision summary.
	Summary string `json:"summary"`
	// Checks are the per-check results, structural checks first then plan
	// checks, in deterministic order.
	Checks []CheckResult `json:"checks"`
	// CitedEvidence are the evidence IDs the final response cited, each
	// resolved against persisted evidence with its tool type.
	CitedEvidence []CitedEvidence `json:"cited_evidence,omitempty"`
	// UncertainAttempts are the authoritative attempts that block completion.
	UncertainAttempts []AttemptRef `json:"uncertain_attempts,omitempty"`
	// PendingApprovals are the pending operator approvals that block
	// completion.
	PendingApprovals []string `json:"pending_approvals,omitempty"`
	// TruncatedEvidence lists persisted recipe evidence whose output was
	// truncated, so truncation is never silently ignored.
	TruncatedEvidence []string `json:"truncated_evidence,omitempty"`
	// Git is the authoritative git observation and change attribution.
	Git *GitObservation `json:"git,omitempty"`
	// WriteReconciliation lists every write evidence checked against the
	// current filesystem.
	WriteReconciliation []WriteReconciled `json:"write_reconciliation,omitempty"`
	// Limitations are the bounded honest limitations of this verification.
	Limitations []string `json:"limitations,omitempty"`
	// CreatedAt is the RFC 3339 UTC verification time.
	CreatedAt string `json:"created_at"`
}

// CitedEvidence is one evidence citation of the final response, resolved
// against persisted evidence. The citation declares the tool the model claims
// produced the evidence; the verifier checks that claim against the persisted
// tool (issue #11 review: cited evidence must match the claimed type).
type CitedEvidence struct {
	// EvidenceID is the cited identifier.
	EvidenceID string `json:"evidence_id"`
	// ClaimedTool is the tool the final response claims produced the evidence.
	ClaimedTool string `json:"claimed_tool"`
	// Tool is the persisted tool of the evidence row (empty when missing).
	Tool string `json:"tool,omitempty"`
	// Exists reports whether the identifier exists in the task's persisted
	// evidence.
	Exists bool `json:"exists"`
	// ToolMatches reports whether the claimed tool equals the persisted tool
	// (only meaningful when Exists).
	ToolMatches bool `json:"tool_matches"`
}

// AttemptRef is one authoritative attempt that blocks completion.
type AttemptRef struct {
	ExecutionID string `json:"execution_id"`
	Tool        string `json:"tool"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

// WriteReconciled is the result of reconciling one persisted write evidence
// against the current filesystem.
type WriteReconciled struct {
	// Path is the normalized relative path of the write target.
	Path string `json:"path"`
	// ChangeKind is "created", "modified" or "unchanged" from the evidence.
	ChangeKind string `json:"change_kind"`
	// AfterHash is the expected after-state hash from the evidence.
	AfterHash string `json:"after_hash"`
	// ObservedHash is the current complete-file hash ("absent" when missing).
	ObservedHash string `json:"observed_hash"`
	// Matches reports whether the current filesystem matches the evidence.
	Matches bool `json:"matches"`
	// EvidenceID is the write evidence used.
	EvidenceID string `json:"evidence_id,omitempty"`
	// Superseded lists the evidence IDs of EARLIER writes to the same path
	// that this write replaced. A corrective write in the #12 coding loop
	// legitimately overwrites a previous attempt, so only the latest write of
	// a path must match the current filesystem; the superseded intermediate
	// states are recorded honestly and never silently dropped (issue #12).
	Superseded []string `json:"superseded,omitempty"`
}

// EvidenceClaim is one typed evidence citation from the final response: the
// model declares the tool that produced the evidence it cites. Model prose
// never enters it; the verifier resolves the claim against persisted evidence
// (existence and tool type).
type EvidenceClaim struct {
	// EvidenceID is the cited identifier.
	EvidenceID string
	// Tool is the tool the final response claims produced the evidence.
	Tool string
}

// Input is the authoritative, bounded input of one verification attempt. It
// is built by the agent loop from persisted task history and the final
// response; model prose never enters it.
type Input struct {
	// TaskID is the verified task.
	TaskID string
	// FinalEvidence are the typed evidence citations of the final response:
	// every cited id must exist AND its claimed tool must match the persisted
	// tool (issue #11).
	FinalEvidence []EvidenceClaim
	// Actions are the persisted logical actions of the task.
	Actions []state.RecoveryAction
	// ToolAttempts are the persisted concrete tool attempts of the task.
	ToolAttempts []state.RecoveryToolAttempt
	// Evidence are the persisted citable observations (tool_results).
	Evidence []state.RecoveryEvidence
	// PendingApprovals are the pending operator approval action ids.
	PendingApprovals []string
	// Plan is the operator acceptance plan (nil means none is configured:
	// completion is refused blocked, issue #11 review).
	Plan *Plan
	// BaselineGitStatus is the bounded `git status --short` captured at task
	// start (empty when unavailable or not captured).
	BaselineGitStatus string
	// BaselineGitDiff is the bounded `git diff` captured at task start.
	BaselineGitDiff string
	// BaselineGitStatusTruncated / BaselineGitDiffTruncated report that the
	// bounded git baseline observations were truncated at task start. The
	// verifier records the limitation so pre-existing changes outside the
	// truncated baseline window are never silently attributed as during_task
	// (issue #11 review).
	BaselineGitStatusTruncated bool
	BaselineGitDiffTruncated   bool
	// Now is the deterministic verification time (RFC 3339 UTC text).
	Now string
}

// Observer is the narrow authoritative-environment seam of the verifier. The
// tools.Registry implements it; tests inject a deterministic fake. The
// verifier has no other access to the filesystem, git or processes.
type Observer interface {
	// FileSHA256 returns the sha256 of the complete file at the relative
	// path, and whether it exists. Missing paths are not failures.
	FileSHA256(relative string) (hash string, present bool, failure error)
	// GitStatusText returns the bounded authoritative git status output.
	GitStatusText() (text string, truncated bool, failure error)
	// GitDiffText returns the bounded authoritative git diff output.
	GitDiffText() (text string, truncated bool, failure error)
}

// Verifier is the control-plane verification boundary. It is constructed once
// per task with an authoritative observer and the operator plan, and invoked
// for every final-completion proposal. It performs no process execution and
// no SQLite transaction.
type Verifier struct {
	observer Observer
	plan     *Plan
	// limits bound the report content (structural strings and check counts).
	limits Limits
}

// Limits bound the verification report so it stays inspectable and bounded.
type Limits struct {
	// MaxChecks caps the number of checks evaluated in one attempt.
	MaxChecks int
	// MaxChangedFiles caps the changed-file lists in the report.
	MaxChangedFiles int
	// MaxObservedChars caps one expected/observed/reason description.
	MaxObservedChars int
}

// DefaultLimits are the verifier report bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxChecks:        256,
		MaxChangedFiles:  512,
		MaxObservedChars: 512,
	}
}

// New constructs a verifier. A nil observer fails every check closed. A nil
// plan means no operator acceptance criteria exist: completion is refused
// blocked (fail closed, issue #11 review), because without task-specific
// acceptance checks the model's completion proposal cannot be proven against
// the task objective.
func New(observer Observer, plan *Plan) *Verifier {
	limits := DefaultLimits()
	if limits.MaxChecks <= 0 {
		limits.MaxChecks = 256
	}
	if limits.MaxChangedFiles <= 0 {
		limits.MaxChangedFiles = 512
	}
	if limits.MaxObservedChars <= 0 {
		limits.MaxObservedChars = 512
	}
	return &Verifier{observer: observer, plan: plan, limits: limits}
}

// WithPlan returns a copy of the verifier with the operator plan replaced
// (used by resume when the persisted plan is loaded from state). A nil plan
// means no operator acceptance criteria: completion is refused blocked.
func (v *Verifier) WithPlan(plan *Plan) *Verifier {
	if v == nil {
		return New(nil, plan)
	}
	copy := *v
	copy.plan = plan
	return &copy
}

// Plan returns the operator acceptance plan (nil when none is configured).
// Without a plan, completion is refused blocked: no task-specific acceptance
// criterion exists (issue #11 review).
func (v *Verifier) Plan() *Plan {
	if v == nil {
		return nil
	}
	return v.plan
}

// planChecks returns the operator checks in deterministic id order.
func (v *Verifier) planChecks() []Check {
	if v == nil || v.plan == nil {
		return nil
	}
	checks := append([]Check(nil), v.plan.Checks...)
	// Stable id order so the report is deterministic regardless of input
	// order.
	for i := 1; i < len(checks); i++ {
		for j := i; j > 0 && checks[j].ID < checks[j-1].ID; j-- {
			checks[j], checks[j-1] = checks[j-1], checks[j]
		}
	}
	return checks
}

// verifyNow is the deterministic time seam for tests.
var verifyNow = func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
