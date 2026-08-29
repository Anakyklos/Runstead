# Implementation Plan: Evidence-Preserving Context Compiler

**Branch**: `001-context-compiler` | **Date**: 2026-08-29 | **Spec**: `specs/001-context-compiler/spec.md`

**Input**: Issue #51 — deterministic, bounded, evidence-preserving model-context projection from authoritative persisted task state, replacing the narrative recovery summary.

## Summary

Create a small Runstead-native `internal/context` package: a deterministic compiler that projects `state.RecoverySnapshot` (plus optional typed pending approvals and the current workspace signature) into a typed, bounded, authority-marked `Compiled` structure. `internal/recovery.BuildContext` becomes a thin adapter over the compiler, preserving the `Context{Text, EvidenceIDs, Chars}` seed contract and the `transcript.recovery(...)` consumption point in `internal/agent/loop.go`. Budget exhaustion fails the resume explicitly before any provider dispatch; silent truncation is removed.

## Technical Context

**Language/Version**: Go 1.22.2 (module `github.com/RenyEnnos/Runstead`).

**Primary Dependencies**: Go stdlib only (sort, strings, bytes, fmt). No new dependency. Existing typed contracts: `internal/state` (`RecoverySnapshot`, `PendingApprovals`), `internal/recovery` (Budget/Context/TraceSink), `internal/agent` (RecoverySeed, TraceLine kinds, tools.Observation).

**Storage**: SQLite through the existing `internal/state` typed layer. No migration, no new table, no SQL inside the compiler.

**Testing**: `go test` (unit + integration with real SQLite via the existing store), `go test -race`, repo gates (gofmt/vet/build/protocol/quality growth+errcheck+live-convention).

**Target Platform**: Linux/amd64 local CLI (`runstead resume`), deterministic offline.

**Project Type**: compiler/projection component inside a modular monolith.

**Performance Goals**: O(n log n) worst case over snapshot slices; rendering linear in output; no wall-clock dependence.

**Constraints**: invariants 1-16 of #51 (authority, disposability, evidence preservation, boundedness, determinism, fail-closed, no hidden amplification, no new frameworks).

**Scale/Scope**: one snapshot per resume; budget default 32 KiB rendered context (existing `DefaultBudget` values, semantics changed from truncated-with-marker to fail-before-dispatch).

## Constitution Check

- I. Local durable state authoritative — compiler source is `RecoverySnapshot`/typed accessors only. PASS.
- II. Model output never authority — non-authoritative section is structurally separate and explicitly marked; narrative never reaches authoritative sections. PASS.
- III. Evidence before claims — evidence IDs pinned; pending-check evidence never dropped. PASS.
- IV. Bounded, deterministic, fail-closed — explicit sort keys, byte budget enforced before render, typed exhaustion error. PASS.
- V. No hidden amplification — no new request paths; recovery semantics unchanged (no re-execution). PASS.
- VI. Minimal native architecture — one small package; no new dependency. PASS.
- Fail-closed rules (secrets, stale classification, explicit absence) — covered in FR-005/FR-006/FR-008. PASS.

## Architecture

### New package `internal/context` (small, specific per #51)

Files (new):
- `internal/context/budget.go` — `Budget` type with per-section caps; moved canonical definition; `DefaultBudget()`.
- `internal/context/compiler.go` — `Input`, `Compiler.Compile`, `Compiled` (typed sections), provenance kinds, `ErrBudgetExhausted`, diagnostics.
- `internal/context/select.go` — deterministic selection helpers: explicit sort keys, mandatory-vs-degradable tiers, byte-accounted render plan.
- `internal/context/render.go` — deterministic renderer (authority-marked sections).
- `internal/context/*_test.go` — unit tests (see Tasks).

Typed shape (conceptually):

```go
type Budget struct {
    MaxContextBytes     int // enforced BEFORE render; mandatory overflow -> ErrBudgetExhausted
    MaxObservationCount int // degradable: newest-first
    MaxObservationChars int // per-observation content cap
    MaxFailureLines     int
    MaxUncertainLines   int
    MaxApprovalLines    int
    MaxVerificationLines int
}

type Input struct {
    Snapshot                *state.RecoverySnapshot
    PendingApprovals        []state.PendingApproval // nil = absent (explicit)
    CurrentWorkspaceSignature string                // "" = unverified
    NonAuthoritativeNotes   []string                // never promoted
    Budget                  Budget
}

type Compiled struct {
    Authoritative []Fact      // typed facts + Provenance + Kind
    NonAuthoritative []Note   // marked, never satisfies anything
    Diagnostics   Diagnostics
    render        string      // computed deterministically
    evidenceIDs   []string
}

type Fact struct {
    Kind        FactKind // Objective|Status|Constraint|Action|Attempt|Evidence|Failure|UncertainEffect|Approval|AcceptanceCheck|VerificationResult|WorkspaceFact
    Origin      string   // evidence/execution/action id, plan digest, approval row, snapshot task
    Value       string   // sanitized bounded text
    Signature   string   // workspace signature when the fact is workspace-derived
    Freshness   Freshness // Current|NeedsRefresh|UnverifiedCurrent (workspace-derived only)
}

type Note struct { Text string } // non-authoritative

type Diagnostics struct {
    CompilerVersion string
    Budget          Budget
    Counts          map[FactKind]int   // rendered counts per kind (deterministic keys)
    Omitted         []OmittedItem      // degradable items skipped (kind + id)
    ExhaustionReason string            // non-empty only when budget failed
}
```

### Recovery adapter

`internal/recovery`:
- `type Budget = context.Budget`; `func DefaultBudget() Budget { return context.DefaultBudget() }` (existing callers/tests compile).
- `BuildContext(snapshot, budget)` → builds `Input` with snapshot, calls `Compile`, renders; on exhaustion returns `Context{Err: context.ErrBudgetExhausted}` (new field or handled by caller). Keep `Context{Text, EvidenceIDs, Chars}` and add `Diagnostics context.Diagnostics` + `Compiled *context.Compiled`.
- `recovery.Resume`: fetch `PendingApprovals` via store when available; pass current workspace signature if reconciliation knows it (keep minimal: use "" unless already computed — see Limits); emit a sanitized diagnostics trace line (new `agent.TraceRecoveryContext` kind with status carrying sanitized counts only). Budget exhaustion becomes a typed recovery failure (DecisionBlocked-ish via existing error path) — no loop, no dispatch.
- `internal/agent/loop.go`: unchanged (`transcript.recovery(l.recovery.Context)`).

### Render contract (deterministic)

Section order fixed: Objective → Task status → Constraints → Actions/attempts (summarized deterministically, newest attempts with content caps) → Evidence (all IDs pinned; bounded content newest-first) → Failures → Uncertain effects → Pending approvals → Acceptance checks (remaining) → Verification result (latest) → Workspace facts (with freshness) → NON-AUTHORITATIVE section (explicit even when empty). Hard byte accounting: pin mandatory first; if mandatory > budget → ErrBudgetExhausted; else fill degradable in fixed order until budget; every skipped degradable item recorded in Diagnostics.Omitted (kind+id, sanitized). Render is a pure function of Compiled; Compiled build never iterates maps for output order.

## Determinism rules

- Evidence: ID descending (allocation order, existing convention).
- Tool attempts/actions: execution/action ID ascending (creation order).
- Verification rows: newest-first (already the persisted order).
- Pending approvals: store order (ORDER BY d.id) preserved.
- Failures/uncertain: derived from attempts by creation order (existing helpers reused where possible).
- All strings rendered verbatim from sanitized persisted values (state.Redact applied at the boundary, as today).

## Staleness (presentation-only)

Workspace-derived facts (their recorded `WorkspaceSignature`) are classified against `Input.CurrentWorkspaceSignature`: equal → `current`; both non-empty and different → `needs-refresh`; signature absent → `unverified-current`. No verification/acceptance behavior is touched.

## Diagnostics surface

`agent.TraceRecoveryContext` trace line emitted by the recovery pipeline carrying sanitized counts (compiler version, budget bytes, counts per kind, omitted ids) — never content. Optionally also returned in `recovery.Context.Diagnostics` for callers. No persistence, no inspect change.

## Risks / Decisions

- Budget semantics change (truncate → fail) may affect any resumed task whose mandatory content exceeds 32 KiB. Mitigation: mandatory set is compact (IDs + status lines); default budget is generous; tests cover exhaustion explicitly.
- `BuildContext` signature stays; callers (`recovery.Resume`) updated to pass approvals/workspace through new `Options` fields.
- No migration; `AcceptancePlanSpec` JSON parsed with the existing verifier plan contract for check IDs only (remaining = plan checks not yet verified passed per latest verification attempt — presentation of "remaining acceptance checks" derived from plan + latest verification decision; if the plan is unparseable, render as explicit `unavailable`).

## Verification plan

1. Unit: determinism (byte-identical render, 10 runs), pinned/degradable tiers, exhaustion error, staleness classes, non-authoritative isolation, provenance completeness, evidence-ID preservation, map-order independence (shuffled input construction), redaction (no secrets in render/diagnostics).
2. Integration (real SQLite): full run → interrupt → resume with a NEW provider conversation via existing scripted seam → equivalent mandatory material + continuity of evidence IDs + no effect re-execution (existing resume suites stay green).
3. Gates: gofmt, build, vet, `go test ./...`, `go test -race ./...`, protocol suite, `git diff --check`, quality gates (growth/errcheck/live-convention).
4. Docs: `docs/architecture.md` (context compiler section), README status pointer, roadmap #51 note; spec/plan/tasks committed.