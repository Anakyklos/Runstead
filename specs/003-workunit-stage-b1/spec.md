# Feature Specification: Bounded Shared/Exclusive Work Unit Scheduling (Stage B1)

**Feature Branch**: `003-workunit-stage-b1`

**Created**: 2026-08-29

**Status**: Implemented

**Input**: Issue #109 — M9 Stage B1: opt-in, bounded concurrent execution for Work Units only when their declared capability envelope is provably read-only, preserving the Stage A trust path (#106 / PR #107) and introducing a fail-closed shared/exclusive scheduling policy. Child of #53; first bounded Stage B slice. No general multi-agent orchestration, no parallel writes.

## User Scenarios & Testing

### User Story 1 - Operator opts into bounded Work Unit concurrency (Priority: P1)

The operator supplies `--workunit-concurrency N` on `run`/`resume` (default `1`, minimum `1`, initial hard ceiling `4`). Invalid values fail before any Work Unit executes. `1` must preserve Stage A serial behavior exactly. The effective configuration is durably persisted with the task, rendered by `runstead inspect`, and a `resume` that would silently adopt a materially different concurrency contract fails closed.

**Why this priority**: The configuration surface is the gate that keeps Stage B opt-in; without it nothing concurrent can happen by default.

**Independent Test**: CLI acceptance: `--workunit-concurrency 0`, negatives and `>4` exit with usage before execution; `1` runs the stage-A serial e2e unchanged; inspect renders the effective value; resume with a different value under a running task fails closed.

### User Story 2 - Independent read-only Work Units may execute concurrently up to the bound (Priority: P1)

A Work Unit whose tool envelope is explicit and contains ONLY tools from the existing observational set (`read_file`, `list_files`, `search_text`, `git_status`, `git_diff`) — including an explicitly empty envelope — is eligible for the shared lane. Independent shared units run concurrently up to the configured maximum, while ready/dependency decisions still come exclusively from durable state in deterministic order.

**Why this priority**: The shared lane is the entire point of the slice: bounded safe concurrency for provably observational work.

**Independent Test**: Driver test with real SQLite and a channel/barrier-based `RunFunc`: two independent explicitly read-only units are simultaneously active under `concurrency=2`; the configured maximum is never exceeded (an active-count guard fails the test).

### User Story 3 - Effectful/unknown Work Units remain exclusive and never overlap anything (Priority: P1)

A Work Unit is exclusive when its tool envelope is omitted (`nil` = task default surface), or declares `write_file`, `apply_patch`, `run_recipe`, or any tool not provably read-only. An exclusive unit starts only when no other unit is active; while it runs nothing else starts. `workspace_scope` never authorizes parallel writers in this issue. A ready exclusive unit cannot be starved by later read-only units.

**Why this priority**: Exclusive units carry the whole trust model (effects, unknown capability surface). Overlap would break the Stage A guarantee.

**Independent Test**: Driver tests: an exclusive unit never overlaps another unit (shared or exclusive) under barriers; omitted envelope is exclusive; `write_file`/`apply_patch`/`run_recipe` and an unknown tool are exclusive (fail-closed for unknown); a mix where an exclusive unit becomes ready after shared units are dispatched still runs the exclusive unit before any NEW shared dispatch (deterministic, no starvation).

### User Story 4 - Failure/cancellation semantics settle the current bounded batch then stop (Priority: P1)

If a shared unit terminates failed/blocked/uncertain, the scheduler starts no new batches, does not artificially cancel siblings that already started, lets the current bounded batch reach durable states, then stops with the parent completion gate closed. Real operator/context cancellation propagates to every active unit, starts no new unit, leaves recoverable durable state and leaks no goroutines.

**Why this priority**: Aggressive internal cancellation would manufacture `unknown_submission`/uncertain effects; operator cancellation must still be honored cleanly.

**Independent Test**: Driver tests with barriers: a sibling failure stops future scheduling only after the already-dispatched batch settles; cancellation propagates to all active runs, no new dispatch happens, no goroutines leak (race detector), unit rows stay durably recoverable.

### User Story 5 - Crash/restart with multiple active units recovers without replay (Priority: P1)

With concurrency, several units may be `running` when the process dies. Recovery must: keep completed units and their evidence; reconstruct every affected unit from SQLite; reconcile provider/tool attempts through the existing recovery contracts; never assume an attempt did not reach the provider; never repeat completed effects; never blind-retry; only return a unit to runnable state when durable evidence makes it safe.

**Why this priority**: Multi-active crash safety is the reason concurrency is only Stage B material: the recovery gate must be proven before any benefit claim.

**Independent Test**: Crash/restart e2e with two active read-only units in a subprocess: after restart, completed units/effects are not replayed (row counts stable), interrupted units reconcile and re-run only from durable evidence, no blind provider replay.

### User Story 6 - Governor authority and provenance remain intact under concurrency (Priority: P1)

Every provider attempt of every concurrent unit still enters the same account-scoped governor; one governed physical attempt stays one accounted physical attempt. The scheduler never creates a second admission path, never rotates providers/models/keys/accounts/sessions to force parallelism, and never bypasses `MaxInFlight`. Evidence IDs stay unique under concurrency and every persisted action/tool/provider/verification row keeps the correct `work_unit_id`.

**Why this priority**: The trust spine must be byte-identical per attempt; concurrency is above the governor, never beside it.

**Independent Test**: E2E with two concurrent read-only units through the real loop + scripted provider: attempt rows are accounted once, `work_unit_id` provenance is correct on every row class, evidence ids are unique, and the governor serializes physical upstream attempts (MaxInFlight stays authoritative).

### User Story 7 - The scheduler is a narrow extension, not a new engine (Priority: P2)

No second agent loop, no worker framework, no actor/daemon/service, no third-party concurrency dependency. The existing agent loop executes every Work Unit; the driver's deterministic ready ordering remains the only selection source. Concurrency is not inferred from provider/model/observed success.

**Why this priority**: Architecture discipline keeps the modular monolith and the trust path reviewable.

**Independent Test**: Code review + build: `internal/workunit` contains the whole scheduler; no new dependencies in `go.mod`; existing package tests stay green.

## Out of Scope (hard boundaries)

- Parallel writes, even with disjoint `workspace_scope`.
- Concurrent `run_recipe`.
- Model-created/modified Work Units, child spawning, recursive agent trees.
- Worker daemons, background jobs, distributed workers.
- Provider/model routing, pooling, fallback, account rotation.
- Changing governor admission/rate/circuit semantics.
- M10 capability packages/profiles.
- Concurrency > 1 as default.
- Starting/resolving the next issue.

## Constitution Check

- Local durable state authoritative: configuration, unit rows and transitions persist; scheduler derives truth only from SQLite. PASS.
- Model never gains authority: units remain operator-defined; the scheduler only orders operator units. PASS.
- No hidden amplification: concurrency lives above the governor; physical attempts stay accounted one-to-one. PASS.
- Evidence/verifier authority: unit completion still requires its own persisted passed verification. PASS.
- Fail-closed: unknown tools are exclusive; config drift fails; invalid values fail before execution. PASS.
- Bounded/deterministic: ready selection unchanged; dispatch order deterministic; bound enforced. PASS.