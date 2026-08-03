# PR #24 Maintainer Review Implementation Plan

> **For agentic workers:** Execute this plan task-by-task with a failing test before each behavior change.

**Goal:** Make the Issue #21 account governor package boundary, provider seam, event delivery, retention, file ownership and lifecycle tests honest without adding framework layers.

**Architecture:** Rename the account-scoped governor from its previous package directory to `internal/governor`. Keep one `Governor` state machine and one account lane, but move concrete methods into focused files. Queue sanitized events under the mutex and deliver them after unlocking through a small re-entrant-safe drain; retain only pending/active request IDs and bounded recent completions.

**Tech Stack:** Go standard library, existing fake clock/telemetry/event seams, table-driven tests, `go test -race`.

## Global Constraints

- No new dependencies, framework, event bus, provider registry, daemon or background worker.
- `max_in_flight` remains exactly one and provider `Complete` remains one invocation/one upstream attempt.
- No provider-owned retry, quota, fallback, pooling, rotation or scheduling policy.
- Process-local ledger/cooldown/retention remain explicitly M1 behavior; durable persistence stays in #8.
- Event sinks receive sanitized events only and are never invoked while `Governor.mu` is held.

### Task 1: Rename the governor package

**Files:**
- Rename directory: previous governor package directory -> `internal/governor`
- Modify: all Go imports, package declarations, docs and test paths that reference the previous package directory

- [x] Rename the directory and package declarations without changing behavior.
- [x] Run a repository search and remove every stale governor-package reference.
- [x] Run `go test ./...`; the existing suite passes under `internal/governor`.

### Task 2: Remove the raw provider bypass from `internal/agent`

**Files:**
- Modify/remove: `internal/agent/client.go`
- Modify/remove: `internal/agent/agent_test.go`
- Test: `internal/agent` focused compile boundary

- [x] Add a failing compile/boundary test showing the agent package does not expose a raw `provider.Client` execution path.
- [x] Remove the temporary `agent.Client` that stores and calls `provider.Client`; do not add the future #7 loop.
- [x] Keep only agent types still used by current code and document that #7 must enter through `internal/governor`.
- [x] Run `go test ./internal/agent ./internal/governor`; PASS with no direct `Complete` caller outside governor/provider tests.

### Task 3: Deliver events outside the governor mutex and deduplicate transitions

**Files:**
- Modify: `internal/governor/governor.go` and the split event-owning file
- Test: `internal/governor/governor_test.go`

- [x] Add re-entrant and blocked sink tests; `Emit` can call `Governor.Snapshot` without deadlock.
- [x] Replace direct sink calls with sanitized events appended under lock and a drain that unlocks before invoking `EventSink.Emit`; re-entrant drains defer until the current batch completes.
- [x] Make `transitionCircuitLocked` the only producer of `EventCircuit`; remove the second transition emission from outcome recording.
- [x] Add assertions for one event on security transition, rate threshold, acknowledgement and credential refresh transitions.
- [x] Run the focused event tests; PASS.

### Task 4: Bound request-ID and task-state retention

**Files:**
- Modify: governor state/config/snapshot files
- Test: `internal/governor/governor_test.go`
- Modify: `docs/account-protection.md`

- [x] Add tests for recent duplicate rejection, three-hour expiry, pending/active preservation and bounded completed retention.
- [x] Store request state as pending, active or completed with completion timestamps; prune completed IDs after the rolling three-hour horizon and at a hard cap of 4096 completed IDs. Never prune pending or active IDs.
- [x] Track task `lastTouched`, preserve queued/active tasks, prune completed task state after three hours and cap retained task states at 1024.
- [x] Expose retained counts in the sanitized snapshot so tests and future persistence can observe the bound.
- [x] Document the process-local retention horizon and caps.
- [x] Run the focused retention tests; PASS.

### Task 5: Split the implementation by concrete behavior

**Files:**
- Rename/split: `internal/governor/governor.go`
- Create: `internal/governor/admission.go`, `permit.go`, `telemetry.go`, `circuit.go`, `execute.go`, `snapshot.go`

- [x] Move existing methods without changing signatures or introducing new interfaces: admission/queue, permits, telemetry, circuit/refresh, execution and snapshots.
- [x] Keep shared state/types in one small core file; preserve package-private helpers and event ordering.
- [x] Run `gofmt -w internal/governor/*.go`; formatting is clean.
- [x] Run `go test ./internal/governor`; PASS with behavior unchanged.

### Task 6: Correct route wording and lifecycle tests

**Files:**
- Modify: `docs/account-protection.md`, `docs/architecture.md`, `docs/development.md`
- Test: `internal/governor/governor_test.go`

- [x] Replace wording that treats `RouteSafety` metadata as proof; state that the governor enforces the declaration and #4 must provide route evidence.
- [x] Change the goroutine-leak test to `Finish` a started permit, then assert no goroutine/lane leak.
- [x] Add a focused test proving `CancelBeforeStart` after `Start` returns `ErrPermitStarted`, leaves accounting unchanged and does not release the lane.
- [x] Run the focused lifecycle tests; PASS.

### Task 7: Full verification and handoff

- [x] Run `gofmt -w internal/governor/*.go internal/provider/*.go internal/config/*.go internal/trace/*.go internal/agent/*.go`.
- [x] Run `go test ./...`, `go test -race ./...`, `go vet ./...`, `go build ./cmd/runstead`, `go test -count=20 ./internal/governor/...`, `bash experiments/protocol/test.sh` and `git diff --check`; all passed.
- [x] Inspect the final diff and confirm `.omx/` and `CLAUDE.md` remain untracked and untouched.
- [ ] Commit/push only after explicit review authorization for those remote actions; keep PR #24 draft.
