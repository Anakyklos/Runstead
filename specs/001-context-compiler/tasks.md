---

description: "Task list for the evidence-preserving context compiler (#51)"
---

# Tasks: Evidence-Preserving Context Compiler

**Input**: Design documents from `/specs/001-context-compiler/`

**Prerequisites**: plan.md (required), spec.md (required for user stories)

**Tests**: Tests are REQUIRED by the feature specification (issue #51 mandates 16 test classes). Every behavior task below follows TDD: failing test first, then implementation, then `go test` green.

**Organization**: Tasks are grouped by user story. Paths are relative to the repository root.

## Phase 1: Foundational (Blocking Prerequisites)

**Purpose**: The new `internal/context` package skeleton and its deterministic core types. Nothing in later phases can start before this phase.

- [x] T001 [US2] Create `internal/context/budget.go`: `Budget` (MaxContextBytes, MaxObservationCount, MaxObservationChars, MaxFailureLines, MaxUncertainLines, MaxApprovalLines, MaxVerificationLines) + `DefaultBudget()` with the current recovery values (32<<10, 8, 4<<10, 32, 16, 16, 8). Test: `internal/context/budget_test.go` — zero budget falls back to default; field values pinned.
- [x] T002 [US2] Create `internal/context/types.go`: `Input{Snapshot *state.RecoverySnapshot, PendingApprovals []state.PendingApproval, CurrentWorkspaceSignature string, NonAuthoritativeNotes []string, Budget Budget}`, `FactKind` enum (Objective, Status, Constraint, Action, Attempt, Evidence, Failure, UncertainEffect, Approval, AcceptanceCheck, VerificationResult, WorkspaceFact), `Freshness` enum (Current, NeedsRefresh, UnverifiedCurrent), `Fact{Kind, Origin, Value, Signature, Freshness}`, `Note{Text}`, `Compiled` (authoritative facts + non-authoritative notes + diagnostics + evidenceIDs + render), `OmittedItem{Kind, ID}`, `Diagnostics{CompilerVersion, Budget, Counts, Omitted, ExhaustionReason}`.
- [x] T003 [US2] Create `internal/context/compiler.go`: `Compiler` with `Compile(Input) (Compiled, error)` and `ErrBudgetExhausted` sentinel; deterministic pipeline: extract authoritative facts (pinned first) → account mandatory bytes → if over budget return `ErrBudgetExhausted` with diagnostics → select degradable items in fixed order until budget → build diagnostics. Test (red first): `internal/context/compiler_test.go` — tiny budget fails with `ErrBudgetExhausted`; determinism across repeated compiles; explicit sort keys (evidence ID descending, execution/action ID ascending, approvals in given order).

## Phase 2: User Story 1 - Resume reconstructs material context (Priority: P1) 🎯 MVP

**Goal**: The compiler projects the authoritative snapshot so a resumed run (new provider conversation) receives the same mandatory material.

**Independent Test**: `TestCompileReconstructsMaterialState` plus the real-SQLite recovery integration scenario in Phase 4.

- [x] T004 [US1] Implement evidence extraction in `internal/context/evidence.go`: pinned evidence IDs for every citable ID (from `RecoverySnapshot.Evidence` joined with completed/reconciled tool attempts, reusing the recovery package's semantics); degradable observation content (newest-first by evidence ID descending, capped by MaxObservationCount/MaxObservationChars); provenance = evidence ID + execution ID + tool + compact arguments. Test: `internal/context/evidence_test.go`.
- [x] T005 [US1] Implement fact extraction in `internal/context/facts.go`: objective, task status/lifecycle, constraints (provider turns/attempts consumed, repeated rejected — from snapshot counts), actions (+fingerprint), tool attempts (+classification, effect hashes), unresolved failures (creation order), uncertain effects (provider attempts uncertain + tool attempts interrupted), pending approvals (from Input, order preserved), remaining acceptance checks (plan checks minus checks proven by the latest verification decision; unparseable plan → explicit `unavailable` fact), latest verification result, workspace facts with recorded signatures. Test: `internal/context/facts_test.go`.
- [x] T006 [US1] Implement deterministic renderer in `internal/context/render.go`: fixed section order (Objective → Status → Constraints → Actions/attempts → Evidence → Failures → Uncertain → Approvals → Acceptance checks → Verification → Workspace facts → NON-AUTHORITATIVE), authority markers per section, explicit non-authoritative section (even when empty), byte accounting before render. Test: `internal/context/render_test.go` — byte-identical outputs over repeated runs; section order pinned.
- [x] T007 [US1] Prove restart-equivalence without the original conversation at unit level: `internal/context/compiler_test.go` — a snapshot built from two different "conversation shapes" yields the same compiled material (evidence IDs, counts) when the persisted state is equal.

## Phase 3: User Story 2 - Typed projection with explicit authority (Priority: P1)

**Goal**: Authority is structural, not documented-only.

**Independent Test**: `TestNonAuthoritativeIsolation` + `TestProvenanceComplete`.

- [x] T008 [US2] Non-authoritative isolation: `Compile` accepts `NonAuthoritativeNotes` and places them ONLY in the non-authoritative section; renderer marks them; a fabricated fact present only in notes never appears among authoritative facts. Test: `internal/context/authority_test.go` (red first).
- [x] T009 [US2] Provenance completeness: every authoritative fact carries a non-empty origin (evidence/execution/action ID, plan digest, approval row, snapshot task). Test: `internal/context/authority_test.go`.

## Phase 4: User Story 3 - Bounded, deterministic, fail-closed (Priority: P1)

**Goal**: Budget exhaustion fails explicitly before provider dispatch; optional items drop deterministically.

**Independent Test**: `TestBudgetExhaustionFailsExplicitly` + `TestDegradableSelectionDeterministic`.

- [x] T010 [US3] Budget enforcement tests: `internal/context/budget_exhaustion_test.go` — mandatory-overflow returns `ErrBudgetExhausted` (never a truncated render); `Diagnostics.ExhaustionReason` populated; render is never returned on failure.
- [x] T011 [US3] Degradable selection tests: `internal/context/budget_exhaustion_test.go` — with a tight-but-sufficient budget, optional items drop newest-first/none deterministically; Omitted records list kind+id; evidence IDs never appear in Omitted.

## Phase 5: User Story 4 - Stale workspace facts classified (Priority: P2)

**Goal**: Workspace-derived facts carry explicit staleness classification; verifier untouched.

- [x] T012 [US4] `internal/context/staleness_test.go` — matching signature → Current; both non-empty and different → NeedsRefresh; absent signature → UnverifiedCurrent; presentation-only (no verifier/state calls).

## Phase 6: User Story 5 - Sanitized diagnostics + recovery adapter (Priority: P2)

**Goal**: Diagnostics surface through the existing recovery/trace path without leaking.

- [x] T013 [US5] `internal/context/redaction_test.go` — render and diagnostics contain no prompt/response body/authorization/cookie/raw header content; secrets absent.
- [x] T014 [US5] `internal/recovery` adapter: `type Budget = context.Budget`, `DefaultBudget()` delegates; `BuildContext(snapshot, budget)` delegates to `Compiler.Compile` + render, preserving `Context{Text, EvidenceIDs, Chars}` and adding `Diagnostics context.Diagnostics` and `Compiled *context.Compiled`; `Context.Err` field for exhaustion; `recovery.Options` gains `PendingApprovals func(ctx, taskID) ([]state.PendingApproval, error)` and `CurrentWorkspaceSignature string`; `recovery.Resume` fetches approvals (when provided), passes signature, emits a sanitized `agent.TraceRecoveryContext` trace line (new kind) with counts/omissions only, and returns a typed error on exhaustion (no loop, no dispatch). Tests: `internal/recovery/context_test.go` (existing suite updated) + new exhaustion/approvals tests.

## Phase 7: Integration & Documentation

**Goal**: Prove recovery behavior with real SQLite and document the feature.

- [x] T015 [US1] Real-SQLite recovery integration test (pattern of existing resume suites): run a scripted task to a checkpoint with persisted evidence and a pending approval; interrupt; resume with a NEW scripted conversation (new provider session) through `cmd/runstead`/`internal/recovery`; assert: mandatory material reconstructed (objective, evidence IDs, pending approval), no historical effect re-executed (attempt counts), and the compiled context is deterministic for the same snapshot. File: follow existing resume test location (`cmd/runstead/resume_test.go` or `internal/recovery/recovery_test.go`).
- [x] T016 [US1] Full verification: `test -z "$(gofmt -l .)"`, `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`, `bash experiments/protocol/test.sh`, `git diff --check`, quality gates (growth/errcheck/live-convention via `tools/quality`). Distinguish pre-existing vs introduced failures if any appear.
- [x] T017 Docs: `docs/architecture.md` context-compiler section (authority model, provenance, boundedness, determinism, staleness, diagnostics, limitations), README project-status pointer, roadmap #51 note. No behavior claimed beyond covered code/tests.

## Dependencies & Execution Order

### Phase Dependencies

- **Foundational (Phase 1)**: no dependencies — must finish first (types used everywhere).
- **Phases 2-4 (P1)**: depend on Phase 1; can proceed sequentially (shared compiler core).
- **Phases 5-6 (P2)**: depend on Phase 1; Phase 6 adapter depends on Phase 2-4 compiler behavior.
- **Integration (Phase 7)**: depends on all compiler phases.

### Within Each User Story

- Tests written and failing BEFORE implementation (TDD), committed per task or logical group.
- Core types → selection → render → adapter → integration.

## Notes

- No new Go dependency; no migration; no SQL inside `internal/context`.
- Every task ends with `go test` for the touched packages green.
- The `recovery.Budget` alias keeps existing callers (`recovery.Resume`, CLI resume path) compiling unchanged.
---

## Maintainer review fixes (PR #105, all completed)

- [x] R01 [US2] Emit typed `FactAttempt` facts for every `snapshot.ToolAttempts` and `snapshot.ProviderAttempts` (execution id origin, action/tool/provider identity, status/outcome, evidence id relation, client request id); tests: `TestFactAttemptsForToolAndProviderAttempts`, strengthened `TestProvenanceComplete`.
- [x] R02 [US4] Emit typed `FactWorkspace` facts (origin, recorded `Signature`, `Freshness` current/needs-refresh/unverified-current) so render and `Compiled.Authoritative` share one authority boundary; tests: `TestFactWorkspaceStructural`.
- [x] R03 [US3] Fix `renderCompiled` omission algorithm: record only the failing line and genuinely-not-selected lines after it, each once; tests: `TestOmittedNeverContainsRenderedNorDuplicates`, `TestOmittedDiscardedItemsAppearExactlyOnce`.
- [x] R04 [US4] Feed the current workspace signature through the real resume path (`agent.WorkspaceSignature` exported helper; `recovery.Options.WorkspaceSignature` observer wired by `cmd/runstead/resume.go`; compiler stays pure); integration test `TestResumeContextWorkspaceFreshnessThroughRealPipeline` proves current/needs-refresh/unverified-current across `recovery.Resume` with real signatures.

## Maintainer review fix round 2 (PR #105 head, all completed)

- [x] R05 [US1] Concrete attempts reach the model-facing context: attempt lines (execution ID, action ID, tool/provider identity, status, provider outcome, evidence ID, client request ID) now render as a deterministic degradable section (`concrete attempts: N tool, M provider`), and provider `FactAttempt` values preserve Status AND Outcome. Tests: `TestAttemptsReachModelFacingContext` (Compile render) and `TestAttemptsReachRecoverySeedContext` (recovery.Resume(...).Seed.Context).
- [x] R06 [US2] Fix byte-budget double charging: `renderCompiled` now writes ALL pinned lines in pass 1 (single charge, output <= MaxContextBytes) and selects degradable lines in pass 2 with the remaining budget; preflight fail-closed unchanged. Exact boundary tests: `TestBudgetBoundarySingleDegradableLineExactFit` (mandatory + one line == budget: rendered, not omitted, output == budget; one byte below: omitted exactly once).
