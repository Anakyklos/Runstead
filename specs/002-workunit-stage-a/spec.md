# Feature Specification: Durable Serial Work Units with Recovery (Stage A)

**Feature Branch**: `002-workunit-stage-a`

**Created**: 2026-08-29

**Status**: Draft

**Input**: Issue #106 — M9 Stage A: durable Runstead-owned Work Units executed serially and recovered safely, reusing the existing governor/protocol/policy/tool/evidence/verifier spine. First executable slice of #53. Stage B (concurrency/multi-agent) is explicitly out of scope and blocked.

## User Scenarios & Testing

### User Story 1 - Operator decomposes a task into durable subtasks (Priority: P1)

The operator supplies bounded Work Unit definitions (id, objective, dependencies, tool envelope, budgets, acceptance checks) through the existing CLI surface before execution. Work Units persist in SQLite independently of any model/provider session; the model can never create, modify or delete them.

**Why this priority**: The durable object is the work, never the session; control-plane ownership is the foundation.

**Independent Test**: Store round-trip (migration + create/read/list) and CLI `--workunits` ingestion; idempotent re-supply.

### User Story 2 - Work Units execute serially through the existing trust path (Priority: P1)

At most one Work Unit runs at a time, in dependency order determined from durable state (never from session memory). Each execution reuses: provider config, governor (attempt accounting), `runstead.protocol`, tool registry, write policy, approvals, process recipes, evidence, state, recovery, verifier. No direct `provider.Client.Complete` bypass exists.

**Why this priority**: Serial trustworthy execution is the Stage A gate.

**Independent Test**: driver test with real SQLite + scripted executor: multiple Work Units with dependencies execute in order, strictly one at a time; governor accounts every attempt.

### User Story 3 - Interruption/resume preserves partial progress without replay (Priority: P1)

A parent task has multiple Work Units; the first completes with evidence, the second is interrupted. After `resume` with a new provider/session, the first Work Unit and its effects are NOT replayed, the second is reconciled from durable state, and execution continues through verification.

**Why this priority**: The recovery/replay gate of #106.

**Independent Test**: real-SQLite recovery integration (the 10-step scenario in the issue).

### User Story 4 - Verification gates both Work Unit and parent completion (Priority: P1)

A Work Unit becomes `completed` only after its own acceptance/verification decision is `passed` (evidence-backed). Model narrative never marks a Work Unit or the parent task completed. The parent task cannot complete while any required Work Unit is `created/ready/running/blocked/uncertain/unverified`.

**Why this priority**: Verifier authority and the parent completion gate.

**Independent Test**: failed-per-check verification cannot transition a Work Unit to completed; parent finalization refused while a Work Unit is open.

### User Story 5 - Capability containment (Priority: P1)

Every Work Unit carries an explicit tool/workspace envelope that must be a subset of the parent task contract; escalation attempts fail before any effect.

**Why this priority**: "workunit capabilities ⊆ parent task capabilities" + fail-before-effect.

**Independent Test**: creating/running a Work Unit requesting an out-of-envelope tool fails closed.

### User Story 6 - Auditability (Priority: P2)

`runstead inspect` and traces expose Work Unit id/parent/status/reason/dependencies/evidence+verification references/budgets without secrets.

**Why this priority**: Operator audit/recovery needs.

**Independent Test**: inspect render contains the work unit section with sanitized refs; no credentials.

### Edge Cases

- Work Unit whose dependency is missing/unknown at create → fail closed.
- Dependency cycle → deterministic rejection before execution; no recursion.
- Corrupted/unknown persisted version → conservative error, no silent continuation.
- Uncertain provider/tool effect → Work Unit stays `uncertain`/blocked and is reconciled via existing recovery semantics; never auto-retried.
- `--workunits` file with an id already persisted → idempotent (definitions never mutate existing state).
- Task with no Work Units → current behavior unchanged (regression gate).

## Requirements

### Functional Requirements

- **FR-001**: Migration adds `work_units` + `work_unit_dependencies` tables and `work_unit_id` provenance columns (default '') on `actions`, `tool_attempts`, `provider_attempts`, `verification_attempts`.
- **FR-002**: Typed store APIs: create (validates parent/dependencies/tool envelope/objective bound), get, list (task-scoped, status filter), transitions with a deterministic lifecycle map, list-ready (dependencies satisfied), reset-interrupted (recovery), has-open (parent gate), evidence-reference update at completion.
- **FR-003**: Lifecycle: `created → ready → running → completed | failed | blocked | uncertain`, plus recovery-only `running → ready` and approval-resolved `blocked → ready`. All other transitions rejected fail-closed; every transition journals an event; completion transitions require an evidence-backed passed verification decision for that Work Unit.
- **FR-004**: Deterministic serial scheduling: at most one Work Unit at a time, selected from persisted state (creation order tie-break); dependency satisfaction from `completed` state only; missing-dependency and cycle detection (deterministic DFS) before execution.
- **FR-005**: Driver reuses `agent.Loop` per Work Unit with: the Work Unit objective as prompt, its acceptance plan as the verifier plan, its budgets in `Limits`, its tool envelope validated against the registry, and evidence/repeat-guard grounding from the authoritative task snapshot (recovery seed). No second agent loop; no direct provider call.
- **FR-006**: Context integration (#51): `state.RecoverySnapshot` carries Work Units; `internal/context` compiles a pinned `work units:` section (id/status/reason) with degradable per-unit detail; recovery `BuildContext` includes it; upstream conversation continuity is never required.
- **FR-007**: Parent completion gate: finalization refused (typed reason) while `HasOpenWorkUnits`; implemented at the driver and exercised through the real `run`/`resume` path.
- **FR-008**: CLI `--workunits FILE` on `run` and `resume` (operator JSON): idempotent definition ingestion, serial execution, gate, output unchanged for tasks without Work Units.
- **FR-009**: `runstead inspect` renders the Work Unit section (no secrets); transition trace lines added.
- **FR-010**: No new Go dependency; no concurrency, worker pool, parallel writes or Stage B machinery.

### Key Entities

- **WorkUnit**: id, task_id, optional parent, objective, status/reason, dependency set (table), tool envelope JSON, workspace scope, acceptance plan spec+digest, budgets (provider/step/context), evidence refs (derived from tagged rows), version, timestamps.
- **WorkUnitDependency**: (work_unit_id, depends_on) pairs enforced at create/schedule.
- **WorkUnit provenance tag**: `work_unit_id` on actions/tool/provider/verification rows; '' = task-level (existing behavior untouched).

## Success Criteria

### Measurable Outcomes

- **SC-001**: The 14 required deterministic test classes of #106 present and green (store round-trip, lifecycle, dependency order, serial execution, missing dep, cycle, escalation, governor admission, restart no-replay, partial recovery, session replacement, uncertain blocking, failed verification, parent gate, inspect no secrets, cancellation, race, no-workunit compatibility).
- **SC-002**: Real-SQLite recovery integration demonstrates the 10-step interruption scenario with a new provider/session.
- **SC-003**: All repo gates green on the branch (gofmt/diff-check/build/vet/test/race/protocol/quality+provider-abstraction gates).
- **SC-004**: Stage B (any concurrency) is absent; diff review confirms no parallel execution machinery.

## Assumptions

- Work Units are operator/control-plane-defined (file), never model-defined.
- The tool envelope vocabulary is the registry's tool ids. Single intentional
  capability contract: OMITTED tools (field absent / null) = the task default
  surface (no restriction); EXPLICITLY EMPTY tools (`[]`) = a fail-closed
  no-tools envelope. A workspace scope without an explicit tool list is
  fail-closed as well (never grants the full parent surface implicitly). The
  workspace scope is WORKSPACE-RELATIVE (for example `sub`), validated through
  the same canonical resolver every tool uses; absolute paths and `..`
  traversal are rejected before any effect.
- Provider/model budgets per Work Unit default to the task values when zero.
- Evidence references of a Work Unit are derived from rows tagged with its id (no duplicated truth).
- The governor's provider-attempt accounting remains authoritative across Work Units; work_unit_id on attempts is provenance only.