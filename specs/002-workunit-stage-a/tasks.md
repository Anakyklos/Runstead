---

description: "Task list for durable serial Work Units with recovery (#106)"
---

# Tasks: Durable Serial Work Units with Recovery (Stage A)

**Input**: Design documents from `/specs/002-workunit-stage-a/`

**Prerequisites**: plan.md (required), spec.md (required for user stories)

**Tests**: REQUIRED by the feature specification (issue #106 mandates 14 test classes). TDD: failing test first, then implementation, then `go test` green.

**Organization**: persistence/lifecycle before execution/recovery. Paths relative to repository root.

## Phase 1: Persistence (Blocking Prerequisites)

- [[x]T001 [US1] Migration `internal/state/migrations/0014_work_units.sql` (work_units + work_unit_dependencies + provenance columns on actions/tool_attempts/provider_attempts/verification_attempts). Test: `internal/state/migrations_test.go` pattern — fresh DB reaches the expected schema (extend existing migration tests).
- [[x]T002 [US1] Typed store: `internal/state/workunits.go` — `CreateWorkUnit` (validates task/parent/deps existence, same task, no cycle via deterministic DFS, objective bound, version), `GetWorkUnit`, `ListWorkUnits`, `TransitionWorkUnit` (lifecycle map + journal event + updated_at), `ReadyWorkUnits`, `HasOpenWorkUnits`, `ResetInterruptedWorkUnits`, `WorkUnitEvidenceRefs` (derived from tagged rows). Tests: `internal/state/workunits_test.go` (round-trip, lifecycle matrix incl. invalid transitions, missing dep, cycle, ordering).

## Phase 2: Provenance threading

- [[x]T003 [US2] Add `WorkUnitID string` (default '') to `state.ActionRecord`, `state.ToolAttemptPrepared`, `state.VerificationAttemptRecord`, `governor.AttemptRequest`, `governor.ProviderPrepared`, `governor.ProviderFinished`, and `agent.Task`; thread through `agent.Loop` persistence calls and the executor's `AttemptRequest`; store writes the column. Tests: existing suites stay green + `internal/state/workunits_test.go` provenance assertions (rows tagged, '' default unchanged).

## Phase 3: Work Unit scheduling and capability containment

- [[x]T004 [US2/US5] `internal/workunit` package: `Definition`/`WorkUnit` types, envelope validation (`tools ⊆ task tools`, `workspace_scope ⊆ task workspace`), ready selection (deps completed, deterministic order), cycle re-check before running. Tests: `internal/workunit/validate_test.go` (escalation rejected, missing dep, cycle), `internal/workunit/select_test.go` (deterministic ready order, one-at-a-time semantics via driver).

## Phase 4: Serial driver

- [[x]T005 [US2] `internal/workunit/driver.go` — `EnsureDefinitions` (idempotent), execute loop (ready → running → completed/failed/blocked/uncertain by loop outcome + per-unit verification decision), parent gate (`HasOpenWorkUnits` → typed refusal), evidence-refs snapshot at completion. Tests: `internal/workunit/driver_test.go` with REAL store + scripted executor: dependency-order execution, strict serial (exactly one running at a time, asserted from store states during execution), governor admission per attempt, verification gates completion, parent gate blocks finalize, narrative-without-evidence cannot complete, no-workunit compatibility.

## Phase 5: Recovery and context

- [[x]T006 [US3] Extend `state.RecoverySnapshot` with `WorkUnits` (+ loader) and `recovery.Resume` with `ResetInterruptedWorkUnits`; `internal/context` gains `Input.WorkUnits` + pinned/degradable `work units:` section; `recovery.BuildContext` passes them. Tests: `internal/context/workunits_test.go` (fixtures, no secrets, determinism), `internal/recovery/recovery_test.go` (snapshot includes units; resume resets interrupted running→ready).
- [[x]T007 [US3] Real-SQLite recovery integration: parent with 2 work units, first completed with evidence, second partial; interrupt; resume with a NEW scripted conversation; assert: first unit/effects not replayed (attempt/evidence counts), second reconciled and continues, context section present, provider/session independent, execution reaches verification. Uncertain-effect variant: unit with uncertain provider attempt stays blocking after resume (no auto-retry).

## Phase 6: CLI, inspection, diagnostics

- [[x]T008 [US1/US6] `--workunits FILE` on `run` and `resume` (operator JSON in `cmd/runstead`); driver wiring; `runstead inspect` Work Units section (no secrets); transition trace lines. Tests: `cmd/runstead/workunit_test.go` (flag ingestion idempotent, e2e serial run + parent gate, inspect render without secrets), trace assertions.

## Phase 7: Docs and gates

- [[x]T009 [US6] Docs: `docs/architecture.md` Work Units section (contract, lifecycle, authority, recovery, capability containment, Stage A/B boundary), README pointer, roadmap #106 note. No behavior claimed beyond covered tests.
- [[x]T010 [ALL] Full verification: `test -z "$(gofmt -l .)"`, `git diff --check`, build, vet, `go test ./...`, `go test -race ./...`, `bash experiments/protocol/test.sh`, quality gates (growth/errcheck/live-convention via tools/quality) and provider-abstraction module; PR body with architecture, migrations, lifecycle, authority notes, recovery/replay semantics, #51 integration, tests+results, limitations, Stage B not implemented, skills used, Spec Kit artifacts, unproven items. No merge.

## Dependencies & Execution Order

- Phase 1 blocks everything; Phase 2 blocks 4-6; Phase 4 blocks 5-6's integration tests; Phase 7 depends on all.
- Within each task: tests first (red), implementation (green), commit per logical group.
- No concurrency/worker-pool code anywhere; serial execution only.