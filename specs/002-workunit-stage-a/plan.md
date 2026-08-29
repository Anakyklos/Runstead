# Implementation Plan: Durable Serial Work Units with Recovery (Stage A)

**Branch**: `002-workunit-stage-a` | **Date**: 2026-08-29 | **Spec**: `specs/002-workunit-stage-a/spec.md`

**Input**: Issue #106 — persist Runstead-owned Work Units, execute serially through the existing trust path, recover without replay, verify before completion, gate the parent task, and prove all of it with real SQLite/recovery tests. Stage B (concurrency) is blocked.

## Summary

A versioned persisted `WorkUnit` (task-scoped subtask) with deterministic lifecycle, dependency graph validation, capability containment and per-Work-Unit verification, executed at most one at a time by reusing `agent.Loop` (no second agent engine). Interruption/restart and provider/session replacement reconstruct Work Unit state from SQLite + the #51 context compiler; completed units/effects are never replayed; uncertain effects stay blocking until reconciled; the parent task cannot complete while any required unit is open.

## Technical Context

**Language/Version**: Go 1.22.2 (module `github.com/RenyEnnos/Runstead`).

**Primary Dependencies**: stdlib + existing packages only: `internal/state` (SQLite store, migrations embed), `internal/agent` (Loop with `Config{Verifier, Limits, Recovery, ...}`), `internal/governor` (attempt accounting), `internal/verifier`, `internal/context` (#51 compiler), `internal/recovery`. No new dependency.

**Storage**: SQLite through `internal/state`. New migration `0014_work_units.sql`; typed APIs in `internal/state/workunits.go`; provenance columns (`work_unit_id TEXT NOT NULL DEFAULT ''`) added to `actions`, `tool_attempts`, `provider_attempts`, `verification_attempts`.

**Testing**: `go test` with real SQLite (store + recovery integration), scripted provider executor for loop/driver tests (existing pattern), `go test -race`, repo gates (gofmt/diff-check/build/vet/protocol/quality growth+errcheck+live-convention/provider-abstraction).

**Target Platform**: Linux CLI (`runstead run`/`resume --workunits FILE`), deterministic offline.

**Project Type**: persistence + serial scheduler + driver inside the modular monolith.

**Performance Goals**: O(V+E) graph validation; one loop run per Work Unit; no background loops or daemons.

**Constraints**: invariants 1-15 of #106 (authority, governor, no hidden retry/fallback, evidence before claims, recovery without blind replay, verifier authority, capability containment, boundedness, redaction, no Stage B).

**Scale/Scope**: N Work Units per task, serial; parent gate at finalization; existing single-task path untouched when no Work Units exist.

## Constitution Check

- Local durable state authoritative: Work Unit truth lives only in SQLite; model/session disposable. PASS.
- Model only proposes: Work Units are operator-defined via `--workunits`; model never creates/completes them. PASS.
- Capability containment: envelope ⊆ task contract validated before any effect. PASS.
- Governor/attempt accounting: every provider attempt flows through the existing executor/governor; `work_unit_id` is provenance only. PASS.
- Verifier authority: unit completion requires its own passed verification; parent gate fails closed. PASS.
- No hidden amplification / no Stage B: at most one unit at a time; no pools/sessions. PASS.
- Redaction: Work Unit state stores only sanitized refs/digests; never prompts/bodies/credentials. PASS.

## Architecture

### Migration 0014 (`internal/state/migrations/0014_work_units.sql`)

```sql
CREATE TABLE work_units (
    work_unit_id      TEXT PRIMARY KEY,
    task_id           TEXT NOT NULL REFERENCES tasks(task_id),
    parent_work_unit_id TEXT NOT NULL DEFAULT '',
    objective         TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN
      ('created','ready','running','completed','failed','blocked','uncertain')),
    tools_json        TEXT NOT NULL DEFAULT '[]',   -- allowed tool envelope ([] = task default)
    workspace_scope   TEXT NOT NULL DEFAULT '',     -- write ownership scope ('' = task scope)
    acceptance_plan   TEXT NOT NULL DEFAULT '{}',   -- versioned typed plan spec for this unit
    acceptance_digest TEXT NOT NULL DEFAULT '',
    context_budget    INTEGER NOT NULL DEFAULT 0,   -- 0 = DefaultBudget (context compiler)
    provider_budget   INTEGER NOT NULL DEFAULT 0,   -- 0 = task default
    step_budget       INTEGER NOT NULL DEFAULT 0,   -- 0 = task default
    evidence_refs     TEXT NOT NULL DEFAULT '[]',   -- derived durable evidence refs at completion
    failure_reason    TEXT NOT NULL DEFAULT '',
    blocking_reason   TEXT NOT NULL DEFAULT '',
    version           INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (task_id, work_unit_id)
);
CREATE TABLE work_unit_dependencies (
    work_unit_id         TEXT NOT NULL REFERENCES work_units(work_unit_id),
    depends_on_work_unit_id TEXT NOT NULL REFERENCES work_units(work_unit_id),
    PRIMARY KEY (work_unit_id, depends_on_work_unit_id)
);
ALTER TABLE actions             ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tool_attempts       ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_attempts   ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE verification_attempts ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
```

### Lifecycle (deterministic transition map, store-enforced)

`created → ready` (scheduler/dependency satisfaction) · `ready → running` (driver start; deps satisfied + envelope OK) · `running → completed` (ONLY after a `passed` verification decision for this unit) · `running → failed | blocked | uncertain` (typed reasons) · `blocked → ready` (approval resolved) · `running → ready` (recovery-only reset of an interrupted unit; prior attempts reconciled by the existing pipeline). Every other transition is rejected. Each transition appends a journal event (`workunit_transition`) in the same transaction as the projection change.

### State APIs (`internal/state/workunits.go`)

- `CreateWorkUnit(ctx, WorkUnitCreate) (string, error)` — validates: task exists, parent exists+same task, dependencies exist+same task, no cycle (deterministic DFS over persisted deps), objective bounded (<= 4KiB), version == 1, envelope well-formed (bounded JSON array).
- `GetWorkUnit` / `ListWorkUnits(taskID)` (ordered by created_at, work_unit_id tie-break) / `ListWorkUnitsByStatus`.
- `TransitionWorkUnit(ctx, taskID, id, from, to, reason)` — applies the map, rejects invalid, updates updated_at, journals event, updates evidence_refs on `completed` (derived from tagged tool_results/verification).
- `ReadyWorkUnits(ctx, taskID)` — status in (created, ready) AND all deps completed, ordered deterministically.
- `HasOpenWorkUnits(ctx, taskID)` — any status in (created, ready, running, blocked, uncertain) or (failed = open for parent completion? A failed unit blocks parent completion too — parent can't complete while a REQUIRED unit failed/unverified. failed counts as open with reason.) → parent gate.
- `ResetInterruptedWorkUnits(ctx, taskID, reason)` — running → ready (recovery path).
- `LoadRecoverySnapshot` extension: `WorkUnits []RecoveryWorkUnit` (+ deps, statuses, reasons, envelopes, digests).
- `InspectWorkUnits` — bounded render for `runstead inspect` (no secrets).

### Provenance threading

`state.ActionRecord`, `state.ToolAttemptPrepared`, `state.VerificationAttemptRecord`, `governor.AttemptRequest` + `governor.ProviderPrepared/ProviderFinished` gain `WorkUnitID string` (default '' = task-level). `agent.Task` gains `WorkUnitID`; the loop threads it into `RecordAction`/`PrepareToolAttempt`/`SaveVerificationAttempt`/`AttemptRequest` (executor). Existing tests keep compiling (field additions, zero defaults).

### Serial driver (`internal/workunit/driver.go`)

`Driver{Store, Registry, ExecutorFactory}` where `ExecutorFactory` builds the loop config pieces (limits, verifier, seed) so tests inject scripted executors and real CLI wiring passes the real ones. Phase loop:

1. `EnsureDefinitions(task, defs)` — idempotent create of missing units; validate envelope ⊆ task tools + workspace scope ⊆ task workspace; reject cycles/missing deps.
2. While `ReadyWorkUnits` non-empty:
   - validate envelope again against the current registry (fail closed before running);
   - `TransitionWorkUnit ready → running`;
   - build loop: `agent.Task{Prompt: unit.Objective, WorkUnitID: unit.ID}`, `Verifier: verifier.New(registry, unitPlan)`, `Limits` from unit budgets, `Recovery: seed` (evidence + guard from the task snapshot for grounding; nil on fresh runs), policy/tools from the parent task wiring;
   - `loop.Run(ctx, task)`; outcome + latest verification decision (task+unit) drive the transition: passed → `completed` (evidence refs snapshot), failed/blocked/uncertain outcomes → corresponding transitions with typed reasons.
3. Parent gate: `HasOpenWorkUnits` → finalization refused with the open units listed.
4. No Work Units → zero driver behavior change; composition root keeps current flow.

### Recovery integration

- `recovery.Resume` — after attempt reconciliation: `ResetInterruptedWorkUnits(running → ready, "interrupted; attempts reconciled")`; snapshot.WorkUnits included in the built context.
- `internal/context` — `Input.WorkUnits []state.RecoveryWorkUnit`; pinned `work units:` line (id: status [reason]) + degradable per-unit detail (objective capped, deps, tools, digest); deterministic sort by id; `recovery.BuildContext` passes them. Provider/session continuity never required.
- Replay safety is inherited: unit-attempt rows are ordinary task rows tagged with `work_unit_id`; existing reconciliation applies (no blind replay), completed units stay `completed` and are skipped by `ReadyWorkUnits`.

### CLI (`run`/`resume --workunits FILE`)

Operator JSON: `[{"work_unit_id","objective","dependencies":[],"tools":[],"workspace_scope","acceptance_plan":{"version":1,"checks":[]},"context_budget","provider_budget","step_budget"}]`. `run`: ensure → execute → gate → finalize (existing output). `resume`: recovery resume → same driver loop. Tasks without `--workunits` behave exactly as today.

### Inspect/trace

`runstead inspect` gains a `Work Units` section (id/parent/objective capped/status+reason/deps/evidence+verification refs/budgets). Driver emits `workunit_transition` trace lines via the existing sink. No secrets anywhere.

## Determinism rules

- Ready selection: `created_at` ascending, `work_unit_id` tie-break; deps from `completed` only.
- Cycle detection: iterative DFS with explicit visited/on-stack maps over `(unit → deps)`; deterministic result.
- Context section: sorted by `work_unit_id`.
- Evidence refs at completion: sorted ids from tagged rows.

## Risks / Decisions

- Threading `work_unit_id` through governor records adds surface; mitigated by zero defaults and provenance-only semantics (no behavior change for '').
- Per-unit verifier plan replaces the task plan during unit runs; task-level acceptance still governs the parent final (both are operator-provided; digest drift rejected as today).
- Per-unit loop counters are seeded from the task snapshot (evidence/guard grounding); task-level attempt accounting stays authoritative via the governor.
- No new tables beyond work_units/dependencies; evidence is JOINED by tag, never copied.

## Verification plan

1. Store: migration round-trip, lifecycle matrix, cycle/missing-dep rejection, escalation rejection, ready ordering, parent gate.
2. Driver (real store + scripted executor): serial dependency order, strict one-at-a-time, governor admission per attempt, verification gating, parent gate, no-workunit compatibility.
3. Recovery integration (real SQLite): 10-step interruption scenario + session replacement; completed unit/effects not replayed; partial unit reconciled; uncertain blocks.
4. Context: work units section fixtures (pinned/degradable, no secrets).
5. Inspect: render assertions without secrets.
6. Gates: gofmt/diff-check/build/vet/test/race/protocol/quality/provider-abstraction; `git diff --check`.

## Limitations (documented in PR)

- Stage A is strictly serial; no concurrency machinery exists.
- Unit attempts share the task's governor budget ceilings (accounting authoritative at task level; per-unit provider_budget bounds the loop's Limits only).
- `blocked → ready` is driver/operator-initiated (approval resolution); no automatic re-execution.