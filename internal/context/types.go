package context

import (
	"errors"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/state"
)

// CompilerVersion identifies this context compiler build. It is part of the
// determinism contract: equal version + equal input + equal budget produce
// byte-identical output.
const CompilerVersion = "context-compiler-v0.1"

// ErrBudgetExhausted reports that the mandatory/pinned content of the task
// cannot fit inside the configured budget. The resume must fail explicitly
// before any provider dispatch; no truncated projection is produced.
var ErrBudgetExhausted = errors.New("context budget exhausted: mandatory content does not fit")

var errNegativeBudget = errors.New("context budget: MaxContextBytes must not be negative")

// Input is the authoritative material the compiler may project. It is built
// exclusively from persisted state and typed read models; nothing in it comes
// from a provider conversation.
type Input struct {
	// Snapshot is the authoritative persisted history (state.RecoverySnapshot).
	Snapshot *state.RecoverySnapshot
	// PendingApprovals are the typed pending approval rows (nil when the
	// pipeline could not load them; they are then rendered as explicitly
	// absent, never invented).
	PendingApprovals []state.PendingApproval
	// CurrentWorkspaceSignature is the workspace signature known at compile
	// time. Empty means the current signature is unknown and workspace-derived
	// facts render as UnverifiedCurrent.
	CurrentWorkspaceSignature string
	// NonAuthoritativeNotes are model-authored summaries/inferences accepted
	// for navigation only. They are structurally separated, explicitly
	// marked, and can never satisfy verification or become facts.
	NonAuthoritativeNotes []string
	// Budget bounds the projection. Zero uses DefaultBudget.
	Budget Budget
}

// FactKind is the typed kind of one authoritative fact.
type FactKind string

const (
	FactObjective       FactKind = "objective"
	FactStatus          FactKind = "task_status"
	FactConstraint      FactKind = "constraint"
	FactAction          FactKind = "action"
	FactAttempt         FactKind = "attempt"
	FactEvidence        FactKind = "evidence"
	FactFailure         FactKind = "failure"
	FactUncertainEffect FactKind = "uncertain_effect"
	FactApproval        FactKind = "pending_approval"
	FactAcceptanceCheck FactKind = "acceptance_check"
	FactVerification    FactKind = "verification_result"
	FactWorkspace       FactKind = "workspace_fact"
)

// Freshness classifies a workspace-derived fact against the current workspace
// signature. Classification is presentation only; verification remains the
// authority on acceptance.
type Freshness string

const (
	FreshnessCurrent           Freshness = "current"
	FreshnessNeedsRefresh      Freshness = "needs_refresh"
	FreshnessUnverifiedCurrent Freshness = "unverified_current"
)

// Fact is one authoritative item of the compiled projection. Origin traces it
// to persisted state or environment evidence (evidence/execution/action id,
// plan digest, approval row, snapshot task).
type Fact struct {
	Kind      FactKind
	Origin    string
	Value     string
	Signature string // recorded workspace signature for workspace-derived facts
	Freshness Freshness
}

// Note is one non-authoritative model-authored item. It is structurally
// separated from Facts and explicitly marked in the render.
type Note struct {
	Text string
}

// OmittedItem records one degradable item skipped deterministically due to a
// cap or byte budget. IDs of pinned content never appear here.
type OmittedItem struct {
	Kind FactKind
	ID   string
}

// Diagnostics is the sanitized construction metadata exposed through the
// recovery/trace path. It never contains prompts, response bodies, secrets or
// arbitrary workspace content.
type Diagnostics struct {
	CompilerVersion  string
	Budget           Budget
	Counts           map[FactKind]int
	Omitted          []OmittedItem
	ExhaustionReason string
}

// Compiled is the deterministic typed projection of the task for the model.
// Render is the bounded text form; the typed sections carry authority and
// provenance so consumers never need to parse prose to know what is
// authoritative.
type Compiled struct {
	Authoritative    []Fact
	NonAuthoritative []Note
	Diagnostics      Diagnostics
	render           string
	evidenceIDs      []string
}

// Text returns the deterministic rendered context.
func (c Compiled) Text() string { return c.render }

// EvidenceIDs returns every pinned citable evidence ID in deterministic
// order, for grounding without re-execution.
func (c Compiled) EvidenceIDs() []string { return c.evidenceIDs }

// String renders the compiled context (used by %v and tests).
func (c Compiled) String() string { return c.render }

// RenderDiagnostics renders a compact sanitized diagnostics summary for trace
// output: version, budget bytes, per-kind counts and omission counts. It
// intentionally excludes item values.
func (c Compiled) RenderDiagnostics() string {
	counts := ""
	first := true
	for _, kind := range sortedFactKinds {
		if n := c.Diagnostics.Counts[kind]; n > 0 {
			if !first {
				counts += ","
			}
			counts += fmt.Sprintf("%s=%d", kind, n)
			first = false
		}
	}
	return fmt.Sprintf("version=%s budget_bytes=%d omitted=%d counts=%s",
		c.Diagnostics.CompilerVersion, c.Diagnostics.Budget.MaxContextBytes,
		len(c.Diagnostics.Omitted), counts)
}

var sortedFactKinds = []FactKind{
	FactObjective, FactStatus, FactConstraint, FactAction, FactAttempt,
	FactEvidence, FactFailure, FactUncertainEffect, FactApproval,
	FactAcceptanceCheck, FactVerification, FactWorkspace,
}
