# Feature Specification: Evidence-Preserving Context Compiler

**Feature Branch**: `001-context-compiler`

**Created**: 2026-08-29

**Status**: Draft

**Input**: Issue #51 — Build an evidence-preserving context compiler from authoritative task state. Runstead-native, bounded, deterministic projection of durable task state into model context; provider conversations stay disposable. Spec Kit flow: constitution → spec → plan → tasks → implement.

## User Scenarios & Testing

### User Story 1 - Resume reconstructs material context without the original conversation (Priority: P1)

A task runs, the process dies, the provider session disappears. The operator resumes with a brand-new provider conversation. The model must receive enough authoritative material (objective, progress, evidence IDs, failures, pending approvals, remaining checks) to continue without "remembering" anything, and without re-executing completed effects.

**Why this priority**: This is the core acceptance criterion of #51 (recovery without transcript dependency).

**Independent Test**: Integration scenario with real SQLite: run a scripted task, interrupt, resume with a new scripted conversation; assert the compiled context contains the same mandatory material (objective, evidence IDs, unresolved state) and that no historical effect re-executes.

### User Story 2 - Retiring narrative prose: typed projection with explicit authority (Priority: P1)

The recovery context is no longer a hand-written narrative blob appended to the transcript; it is the deterministic render of a typed projection that separates authoritative facts (with provenance) from non-authoritative notes.

**Why this priority**: The issue requires distinguishing authority in the representation itself, not only in comments.

**Independent Test**: Unit tests assert the typed structure: authoritative sections carry origins (evidence ID / action ID / attempt / plan / approval), and model-authored narrative injected only into the non-authoritative input never appears in any authoritative section.

### User Story 3 - Bounded, deterministic, fail-closed context (Priority: P1)

Same durable state + same budget + same compiler version produce byte-identical context. When the budget cannot hold mandatory content, the resume fails explicitly before any provider dispatch instead of truncating silently.

**Why this priority**: Invariants 8-9 of #51; current `truncate()` silently cuts with a marker.

**Independent Test**: Determinism test (same input → identical render), budget-exhaustion test (tiny budget → typed error, no provider dispatch, no silent cut), degradable-sections test (optional items drop deterministically newest-first).

### User Story 4 - Stale workspace facts are explicitly classified (Priority: P2)

A workspace-derived fact recorded under a workspace signature is presented as current only when the current signature matches; otherwise it is classified needs-refresh or unverified-current. Classification is presentation; verification stays the authority on acceptance.

**Why this priority**: Requirement 9 of #51; no silent staleness.

**Independent Test**: Unit test with matching/mismatching/absent current signature yields the three classes; verifier behavior is untouched.

### User Story 5 - Sanitized construction diagnostics (Priority: P2)

The compiler exposes sanitized metadata (version, budget, per-section counts, omitted optional items, exhaustion reason) through the existing recovery/trace path without dumping full context, content, or secrets.

**Why this priority**: Issue diagnostics requirement; redaction invariants.

**Independent Test**: Diagnostics output contains no secret/prompt/body content; grep-level assertion in trace tests.

### Edge Cases

- Empty snapshot / task with no attempts: renders objective + status + explicit "no evidence" without error.
- Budget smaller than mandatory set: typed `ErrBudgetExhausted`; resume aborts before dispatch; diagnostics carry the reason.
- Pending approvals present: included in mandatory section with action/tool identity but no arguments content beyond what state exposes.
- Workspace signature absent: workspace facts classified `unverified-current`, never silently presented as fresh.
- Model narrative input: goes only to the non-authoritative section; cannot appear in authoritative sections or satisfy checks.
- Duplicate evidence IDs / unstable map ordering: all rendering paths use explicit sort keys; no map iteration order leaks.

## Requirements

### Functional Requirements

- **FR-001**: Compiler consumes the authoritative `state.RecoverySnapshot` (plus optional typed pending approvals and current workspace signature) and produces a typed, deterministic, bounded projection.
- **FR-002**: Projection separates authoritative facts (objective, lifecycle/status, constraints, actions+attempts, evidence IDs and bounded content, failures, uncertain effects, pending approvals, remaining acceptance checks, verification results, workspace facts with signatures) from non-authoritative notes, with explicit markers and provenance.
- **FR-003**: Evidence IDs required by acceptance checks and unresolved actions are pinned; observation content is degradable newest-first with an explicit cap; byte budget is enforced before render; budget exhaustion fails explicitly (typed error) rather than truncating.
- **FR-004**: Rendering is deterministic under equal input/budget/version: explicit sort keys (evidence ID, execution ID, action ID, approval order), no map-order dependence, no dependence on wall clock.
- **FR-005**: Workspace-derived facts carry their recorded workspace signature and a staleness classification (current / needs-refresh / unverified-current) derived from the provided current signature; classification never alters verifier authority.
- **FR-006**: Model-authored summaries/inferences are accepted only as non-authoritative notes, never promoted; absence of notes is explicit.
- **FR-007**: `internal/recovery.BuildContext` becomes the adapter over the compiler, preserving the `Context{Text, EvidenceIDs, Chars}` seed contract and exposing `Diagnostics`; the loop consumption point (`transcript.recovery(...)`) is unchanged.
- **FR-008**: Diagnostics are sanitized (version, budget, counts, omitted items, exhaustion reason) and surfaced through the existing recovery path; full context is never dumped; no secrets/prompts/bodies enter diagnostics, tests or traces.
- **FR-009**: No new SQL, no migration unless a justified need appears; typed state contracts are consumed as-is.
- **FR-010**: No new Go dependency; no vector store, RAG, REPL, multi-agent, retry/fallback/policy changes; Chronological scope strictly #51 (out-of-scope items listed in #51 are not implemented).

### Key Entities

- **Compiled**: typed deterministic projection (authoritative sections + non-authoritative section + diagnostics + rendering).
- **Budget**: per-section deterministic caps (total bytes enforced before render; observation content count/chars; failure/uncertain/provenance line caps).
- **Provenance**: authoritative items carry their origin (evidence ID / execution ID / action ID / plan digest / approval row), enabling traceability to persisted state.
- **Diagnostics**: sanitized construction metadata for the recovery/trace path.

## Success Criteria

### Measurable Outcomes

- **SC-001**: Same snapshot + same budget + same version → byte-identical render (unit test, plus race-invariant runs).
- **SC-002**: Budget below the mandatory set → typed fail-before-dispatch; zero provider dispatches in the failing scenario.
- **SC-003**: Real SQLite restart/recovery integration test reconstructs equivalent mandatory material with a new provider conversation and no effect re-execution.
- **SC-004**: 16 required test classes of #51 all present and green, including `go test -race`.
- **SC-005**: Recovery narrative path (`BuildContext`) fully delegates to the compiler; no stale `truncate`-based silent cutting remains.

## Assumptions

- The `RecoverySnapshot` and typed state accessors are the authoritative source (confirmed on `main`); no migration is needed.
- Pending approvals are provided as typed `state.PendingApproval` rows by the recovery pipeline (store accessor exists); when unavailable, the compiler lists them as absent/explicit rather than inventing them.
- The current workspace signature is provided by the pipeline when known (recovery reconciliation computes it); otherwise facts render as `unverified-current`.
- CLI budget flags already wired to `recovery.Budget` continue to work via the type alias; no flag surface change.
- Non-authoritative input is empty in today's pipeline; the section exists and is rendered explicitly so the boundary is structural, not documented-only.