# Implementation Plan: Bounded Shared/Exclusive Work Unit Scheduling (Stage B1)

**Branch**: `feat/109-workunit-shared-exclusive` | **Date**: 2026-08-29 | **Spec**: `specs/003-workunit-stage-b1/spec.md`

**Input**: Issue #109 — first bounded Stage B slice on top of #106/PR #107 (in `main` at `f2432e1`). Opt-in, bounded concurrent execution ONLY for Work Units whose declared tool envelope is provably read-only, through a fail-closed shared/exclusive scheduler. No general multi-agent orchestration, no parallel writes, no second agent engine, no governor changes.

## Summary

A narrow extension of `internal/workunit.Driver`: a bounded scheduler with a **shared** (read-only, concurrent up to `--workunit-concurrency N`) and **exclusive** (never overlaps anything) lane. Classification is derived purely from the persisted tool envelope (`nil`/omitted = exclusive; explicit non-empty/[] envelope contained in the 5 observational tools = shared; anything else = exclusive, fail-closed). The effective concurrency is persisted in the task's existing `config_json` (no new migration), rendered by `runstead inspect`, and enforced fail-closed on `resume` drift. The existing agent loop, governor, policy, tools, evidence, recovery and verifier remain the ONLY execution path.

## Technical Context

**Language/Version**: Go 1.22.2 (module `github.com/RenyEnnos/Runstead`).

**Primary Dependencies**: stdlib only (`sync`, `context`, channels). No new dependency, no worker framework, no actor/daemon/service.

**Storage**: SQLite through `internal/state`. NO new migration: the effective concurrency is persisted inside the task's existing durable `tasks.config_json` (the same authoritative configuration snapshot `resume` already validates for provider/model/policy continuity). No scheduler metadata is persisted elsewhere; unit lanes are derived deterministically from the persisted `tools_json` envelope.

**Testing**: driver-level deterministic concurrency tests with real SQLite + channel/barrier `RunFunc` seams (no wall-clock assertions; timeouts only as deadlock detectors); loop-level E2E in `cmd/runstead` with a request-id-keyed scripted provider (the governor serializes physical attempts, so interleaving is handled by keying responses per unit id); crash/restart E2E reusing the existing `TestRuntimeCrashHelper` subprocess seam; `go test -race ./...`.

**Target Platform**: Linux CLI (`runstead run`/`resume --workunit-concurrency N --workunits FILE`), deterministic offline.

**Project Type**: persistence + bounded scheduler inside the modular monolith.

**Performance Goals**: O(V+E) classification; at most `concurrency` unit loops in flight; no background loops.

**Constraints**: invariants 1-19 of #109 (authority, governor accounting one physical attempt = one accounted attempt, no hidden retry/fallback/fan-out, evidence-before-completion, recovery without blind replay, verifier authority per unit, config default 1 / min 1 / ceiling 4, resume drift fails closed, no starvation of ready exclusives, no goroutine leaks, no parallel writes).

**Scale/Scope**: N units per task, at most `concurrency` active; existing single-task path untouched when no Work Units exist; `concurrency=1` byte-identical to Stage A.

## Constitution Check

- Local durable state authoritative: ready selection, transitions, config and lanes all come from SQLite; the scheduler holds no separate truth. PASS.
- Model only proposes: scheduler reorders only operator-defined units. PASS.
- Capability containment: envelope re-validated per dispatch before any effect; restricted registry views unchanged. PASS.
- Governor/attempt accounting: all attempts still flow through the single shared governor-owned executor; scheduler concurrency is above the governor, never beside it; `MaxInFlight` stays authoritative. PASS.
- No hidden amplification: no retries/fallback/rotation/fan-out; the scheduler adds no provider path. PASS.
- Verifier authority: completion still requires the unit's own persisted passed verification. PASS.
- Fail-closed: unknown/effectful tools are exclusive; invalid concurrency fails before execution; resume drift fails. PASS.
- No secrets: only the integer concurrency is added to config_json; nothing secret enters state/traces/docs. PASS.

## Architecture

### 1. Lane classification (`internal/workunit/lane.go`, new)

```go
// Lane is the scheduling class of one Work Unit derived from its persisted
// tool envelope (issue #109). The model never supplies it.
type Lane string

const (
    LaneShared    Lane = "shared"    // explicit envelope, all observational
    LaneExclusive Lane = "exclusive" // omitted envelope or any other tool
)

// ReadOnlyTools is the closed observational set proven safe to overlap:
// read_file, list_files, search_text, git_status, git_diff.
// Classify(tools) Lane:
//   - tools == nil (omitted = task default surface) -> exclusive
//   - tools != nil and every element in ReadOnlyTools -> shared
//     (including an EXPLICITLY EMPTY [] envelope: no tools = read-only)
//   - any other member (write_file, apply_patch, run_recipe, future/unknown)
//     -> exclusive (fail-closed: never concurrent because unrecognized)
```

Constants for the operator surface:

```go
const (
    DefaultConcurrency = 1 // Stage A behavior
    MinConcurrency     = 1
    MaxConcurrency     = 4 // initial hard ceiling
)
```

### 2. Bounded scheduler (`internal/workunit/driver.go`, extend `Driver`)

Add one field: `Concurrency int` (0 = default 1, preserving all existing driver tests and Stage A callers). Rewrite `RunAll` internals as a bounded scheduler while keeping the signature and the Stage A semantics for `concurrency=1`:

- **Main scheduler goroutine** owns dispatch. Per iteration:
  1. Return early on `ctx.Err()` (after draining active units) or a pending stop condition.
  2. `resolveBlockedWorkUnits` (approval resolution, unchanged).
  3. `ReadyWorkUnits` (unchanged: persisted, deterministic `created_at, work_unit_id` order; deps must be durably `completed`).
  4. If any ready unit is EXCLUSIVE: when `active == 0` dispatch the first deterministic exclusive; when `active > 0` dispatch NOTHING (wait for the active batch to drain). This is the anti-starvation rule: a ready exclusive blocks new shared dispatches deterministically.
  5. Otherwise fill shared slots in ready order up to `concurrency - active`.
- **Per-unit worker goroutine**: the dispatch path transitions `created -> ready -> running` (persisted BEFORE dispatch, as Stage A), increments `active`, then runs the unit:
  - `run(ctx, unit)` → on error: report, leave unit `running` (recovery reset), settle.
  - Outcome mapping identical to Stage A: `completed` requires the unit's own latest persisted verification decision `passed` (latest by `sequence`, tagged `work_unit_id`) else `blocked`; `failed/blocked/uncertain` transition with typed reason; `canceled` leaves `running`; unknown leaves `running` (interrupted). Every transition is journaled by the existing store path.
- **Settle events** flow over one buffered channel to the main goroutine. A first non-completed terminal outcome (`failed`, `blocked`, `uncertain`) sets the stop condition: NO new dispatch/batch, the already-dispatched batch settles to durable states, then `RunAll` returns `ErrWorkUnitBlockedChain` with that unit id (parent gate stays open). A `canceled` outcome or `ctx` cancellation stops dispatch, drains active units, and returns the wrapped `context.Canceled` (CLI exit 130); active units settle by their own outcomes (a genuinely completed sibling stays completed: durable truth, never replayed).
- **No goroutine leaks**: every dispatch is counted, every worker sends exactly one settle event, and every return path drains `active` to zero before returning (buffered settle channel sized `MaxConcurrency` as backstop).
- **Bound enforcement**: `Concurrency` normalized (0 -> 1); out-of-range (<1 or >4) fails BEFORE any unit runs (`ErrWorkUnitConcurrency`).
- **Determinism**: ready list order unchanged; exclusive pick = first in ready order; shared fill = ready order; settle handling in arrival order for stop-condition bookkeeping (per-unit outcomes are independent rows).

### 3. Durable scheduler config (`internal/state/workunits.go` + `internal/state/inspect.go` tiny additions)

- `state.WorkUnitConcurrencyKey = "workunit_concurrency"` — the key inside `tasks.config_json` (existing durable structure; no migration).
- `state.WorkUnitConcurrencyFromConfigJSON(configJSON string) (value int, ok bool)` — mirror of `ProviderIdentityFromConfigSnapshot`: reads the key; `ok=false` when absent (Stage A task = serial contract). The CLI validates range; the driver re-validates before execution (corrupt persisted values fail closed).
- Inspect: `renderConfig` already renders every `config_json` key sorted, so the effective concurrency appears in the Configuration section with zero state changes.

### 4. CLI (`cmd/runstead`)

- `run`: new flag `--workunit-concurrency N` (int, default 1). Validation `1 <= N <= 4` happens right after parsing, BEFORE any store work ("invalid values fail before executing Work Units"). When `--workunits` is present, the effective N is merged into the bootstrapped `ConfigJSON` (`withWorkUnitConcurrency(snapshot, n)`) and threaded into `runWorkUnitChain`.
- `resume`: same flag (optional). When `--workunits` is present: persisted P = `state.WorkUnitConcurrencyFromConfigJSON(preload.Task.ConfigJSON)` (absent -> 1); an EXPLICITLY supplied N different from P fails closed (exit usage) before recovery/chain; an omitted flag adopts P (the task's own contract, not a silent change). Validation of the flag range regardless.
- `runWorkUnitChain(..., concurrency int, unitRun)` builds `Driver{Concurrency: concurrency, ...}`.
- **Trace sink**: unit loops of one chain share a `cliTraceSink`; under concurrency two loops emit concurrently. Wrap the unit-chain sink in a mutex (`lockedTrace`) used by `run` and `resume` chain wiring so `go test -race` and real builds stay correct with `bytes.Buffer`/pipe writers.
- Evidence IDs stay unique: the registry's `nextID` is already a shared atomic across unit views (`tools.Registry.nextID`); `NextEvidenceSequence` seeding on resume unchanged.

### 5. Recovery (no code change required — verified)

`recovery.Resume` already handles N running units: `ResetInterruptedWorkUnits(running -> ready, "interrupted; attempts reconciled")` resets ALL interrupted units, `ReconcileUncertainWorkUnits` handles uncertain, per-unit counters continue from each unit's own persisted history (`WorkUnitLogicalTurnCount`), restricted registry views reuse the shared atomic evidence counter, and the #51 context compiler renders the full unit set. The scheduler reads the same authoritative `ReadyWorkUnits` after reset, so crash/restart with multiple `running` units reconciles each independently, never replays completed units/effects, and never blind-retries (existing attempt delivery-state contracts).

### 6. Tests

**`internal/workunit/lane_test.go`** — classification matrix (nil / [] / each observational tool / write_file / apply_patch / run_recipe / unknown / mixed).

**`internal/workunit/scheduler_test.go`** (real SQLite store, channel/barrier RunFunc, no sleeps):
1. `concurrency=1` keeps Stage A serial order and one-at-a-time.
2. Two independent read-only units are simultaneously active at `concurrency=2` (barrier: both entered before either completes; timeout = deadlock detector only).
3. Max never exceeded: active-count guard with 4 shared units at `concurrency=2`.
4. Dependencies preserved: dependent never enters before its dependency durably completes (barrier).
5. Exclusive never overlaps another exclusive.
6. Exclusive never overlaps a read-only unit (trap: a shared unit entering before the exclusive settles = violation).
7. Omitted (nil) envelope is exclusive.
8. `write_file`, `apply_patch`, `run_recipe`, and an unknown-but-allowed tool are exclusive.
9. Unknown tool fails closed (never shared), and out-of-parent-envelope unknown tool is already escalation-rejected (existing test).
10. No starvation: exclusive becoming ready while a shared batch drains blocks NEW shared dispatch until the exclusive settles (trap unit entering early = violation).
11. Sibling `failed`/`blocked`/`uncertain` stops new batches after the current batch settles (blocked runner + traps).
12. Cancellation: propagates to all active runs, no new dispatch, units stay `running` (recoverable), `RunAll` returns only after draining (no goroutine leaks).
13. Out-of-range concurrency fails before any run.

**`cmd/runstead/workunit_concurrency_e2e_test.go`**:
14. `--workunit-concurrency 1` re-runs the Stage A serial e2e unchanged.
15. Concurrent read-only e2e: two independent read-only units + parent through the REAL loop, param keyed by `taskID-workUnitID` prefix so interleaving is deterministic; asserts: both units completed; evidence IDs unique; provenance `work_unit_id` on actions/tool_attempts/provider_attempts/verification_attempts; every provider attempt accounted once; physical attempts never overlap (keyed fake tracks in-flight `Complete`).
16. Effortful unit in the same batch stays exclusive (mixed e2e optional; driver test covers).
17. Invalid values (0, -1, 5) exit usage before any unit runs.
18. `runstead inspect` renders `workunit_concurrency`.
19. Resume drift: run under N=2 with an open unit; resume with `--workunit-concurrency 1` fails closed; resume without the flag adopts persisted 2 and completes.
20. Crash/restart with two active read-only units (`TestRuntimeCrashHelper` + `tool_tx2_after` N=2): both units `running` after crash; resume with a new conversation reconciles both, no replay (row counts), correct provenance; parent completes.

**Existing suites** stay green unchanged (driver zero-value `Concurrency` = serial).

## Determinism rules

- Ready selection: `created_at` ascending, `work_unit_id` tie-break (unchanged store contract).
- Lane: derived from persisted `tools_json` only.
- Dispatch: exclusive = first ready exclusive; shared fill in ready order; slot count `<= concurrency`.
- Stop conditions: first settled non-completed outcome (in settle-arrival order) names the blocked-chain error unit; cancellation error wraps `context.Canceled`.
- Evidence: shared atomic sequence; per-unit persistence tags unchanged.

## Risks / Decisions

- **ConfigJSON vs new table**: the issue prefers existing durable structures when equally correct; concurrency is a per-task execution contract already covered by the config snapshot's resume-drift semantics. No migration. The state reader returns (value, present, error) with ONLY true absence mapping to the legacy default: present-but-invalid types, non-integral values and out-of-range integers are refused in the resume pre-flight (exitCorrupt) before the recovery pipeline; the driver re-validates before execution as defense in depth.
- **Scheduler goroutines**: one goroutine per dispatched unit, bounded by `concurrency`; settle channel buffered; every return path drains. No worker pool abstraction.
- **Non-deterministic provider interleaving**: accepted and handled — the governor serializes physical attempts; E2E uses unit-keyed scripted responses; assertions never depend on which unit reached the governor first.
- **Canceled-but-completed sibling**: a sibling that legitimately finished before cancellation still transitions (durable truth; "completed units are never replayed" outranks cancel-speed cosmetic concerns). Interrupted units stay `running` for recovery, exactly like Stage A.

## Verification plan

1. `test -z "$(gofmt -l .)"`; `go test ./...`; `go vet ./...`; `go build ./cmd/runstead`; `go test -race ./...`; `bash experiments/protocol/test.sh`; quality gates (growth/errcheck/live-convention); provider-abstraction module test+vet+build; `git diff --check`.
2. Driver scheduler tests + lane tests (deterministic, race-clean).
3. E2E: serial flag=1, concurrent read-only, invalid values, inspect, resume drift, crash/restart with two running units.
4. PR body: policy, bound location, exclusivity rationale, governor-authority proof, cancellation/recovery, durable config, tests, no migration, limitations, validation results.

## Limitations (deliberate)

- Shared lane is ONLY the 5 observational tools; everything else exclusive. `workspace_scope` never authorizes parallel writes.
- No `run_recipe` concurrency; no model-created units; no dynamic spawning; no provider routing/rotation; default stays 1.
- No general multi-agent claim: this proves the smallest safe Stage B scheduler only.