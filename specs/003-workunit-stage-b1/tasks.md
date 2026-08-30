---
description: "Task list for bounded shared/exclusive Work Unit scheduling (#109)"
---

# Tasks: Bounded Shared/Exclusive Work Unit Scheduling (Stage B1)

**Input**: Design documents from `/specs/003-workunit-stage-b1/`

**Prerequisites**: plan.md (required), spec.md (required for user stories)

**Tests**: REQUIRED by the feature specification (issue #109 mandates deterministic concurrency proof classes 1-19). TDD: failing test first, then implementation, then `go test` green.

**Organization**: classification/state before scheduler before CLI/E2E before docs. Paths relative to repository root.

## Phase 1: Classification and durable config

- [[x]T101 [US2/US3] `internal/workunit/lane.go` — `Lane` (shared/exclusive), `Classify(tools []string)` over the closed observational set (`read_file`, `list_files`, `search_text`, `git_status`, `git_diff`); nil envelope = exclusive; explicit `[]` = shared; unknown/effectful tools = exclusive (fail-closed). Concurrency constants (`DefaultConcurrency=1`, `MinConcurrency=1`, `MaxConcurrency=4`). Tests: `internal/workunit/lane_test.go` classification matrix (nil/[]/5 observational/write_file/apply_patch/run_recipe/unknown/mixed).
- [[x]T102 [US1] `internal/state` — `WorkUnitConcurrencyKey = "workunit_concurrency"` and `WorkUnitConcurrencyFromConfigJSON(configJSON) (int, bool)` reading the existing `tasks.config_json` (absent = Stage A serial contract). Tests: `internal/state/workunits_test.go` additions (absent / present / malformed handling).

## Phase 2: Bounded scheduler

- [[x]T103 [US2/US3/US4] `internal/workunit/driver.go` — `Driver.Concurrency int` (0 -> 1) and rewrite `RunAll` as the bounded shared/exclusive scheduler: deterministic ready selection from persisted state, exclusive-first dispatch (no shared dispatch while a ready exclusive waits; active drain), shared fill up to the bound, per-unit worker goroutines with Stage A outcome mapping (verification-gated completion; failed/blocked/uncertain transitions; canceled/unknown leave `running`), batch-stop on first non-completed settle (drain current batch, then `ErrWorkUnitBlockedChain`), cancellation propagation + drain (no goroutine leaks), `ctx` checked before every dispatch round, `ErrWorkUnitConcurrency` for out-of-range values before any run. Existing driver tests must stay green unchanged (zero-value driver = serial).
- [[x]T104 [US2/US3/US4] `internal/workunit/scheduler_test.go` — deterministic channel/barrier tests (no sleeps): concurrency=1 serial; two read-only units simultaneously active at 2; max never exceeded; dependencies preserved; exclusive never overlaps exclusive/read-only; nil envelope exclusive; write_file/apply_patch/run_recipe/unknown exclusive; unknown fails closed; no starvation of a ready exclusive; sibling failed/blocked/uncertain stops new batches after batch settles; cancellation propagates/leaks nothing; out-of-range concurrency rejected.

## Phase 3: CLI surface and durable wiring

- [[x]T105 [US1] `cmd/runstead` — `--workunit-concurrency N` on `run` (flag default 1) and `resume` (manual parse, optional); range validation before any Work Unit execution; effective N persisted in the bootstrapped `ConfigJSON` (`withWorkUnitConcurrency`) when `--workunits` present; `runWorkUnitChain(..., concurrency, ...)`; resume drift check (explicit flag differs from persisted => fail closed; omitted flag adopts persisted); locked trace sink for concurrent unit chains. Tests: `cmd/runstead/workunit_concurrency_e2e_test.go` (flag=1 serial preserved; invalid values 0/-1/5 fail before execution; inspect renders `workunit_concurrency`; resume drift fails closed; resume without flag adopts persisted).

## Phase 4: Concurrent execution and recovery proof

- [[x]T106 [US2/US5/US6] `cmd/runstead/workunit_concurrency_e2e_test.go` — concurrent read-only e2e through the REAL loop with a request-id-keyed scripted provider (`taskID-workUnitID` prefix): both units completed; evidence IDs unique; provenance `work_unit_id` correct on actions/tool_attempts/provider_attempts/verification_attempts; every provider attempt accounted exactly once; physical attempts never overlap (governor `MaxInFlight` stays authoritative). Crash/restart e2e with two active read-only units (existing `TestRuntimeCrashHelper` + `tool_tx2_after` N=2): both units `running` after crash; resume reconciles both through a new conversation; completed units/effects never replayed (row counts); no blind provider replay; parent completes.

## Phase 5: Documentation

- [[x]T107 [US7] `docs/architecture.md` Work Units section (shared/exclusive policy, bound location, governor authority, cancellation/recovery, durable config/resume drift, limitations); `docs/roadmap.md` Stage B1 status; README CLI mention. Spec Kit `tasks.md` all ticked; `spec.md`/`plan.md` final.
- [[x]T108 [US1-US7] Full validation: `test -z "$(gofmt -l .)"`, `go test ./...`, `go vet ./...`, `go build ./cmd/runstead`, `go test -race ./...`, `bash experiments/protocol/test.sh`, quality gates (growth/errcheck/live-convention), provider-abstraction module gates, `git diff --check`. Single PR against `main` titled `feat(workunit): add bounded shared/exclusive scheduling (#109)`.