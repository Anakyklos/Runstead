# Implementation Plan: M9 Work Unit Concurrency Evidence Gate (Stage B1)

**Branch**: `test/53-m9-workunit-concurrency-evidence` | **Date**: 2026-08-30 | **Spec**: `specs/004-workunit-m9-evidence-gate/spec.md`

**Input**: Issue #53 M9 exit gate. Reuse the Stage B1 scheduler (#109/PR #110) and its existing test seams; add no production behavior change.

## Summary

A measurement slice: deterministic correctness corpus (scenarios A–D) at driver level with the REAL governor; a ceiling-concurrency=4 E2E through real governed loops; a wall-clock reproduction harness (Go benchmarks + runner script) comparing concurrency 1/2/4 with min/median/max over repetitions; a versioned report with an explicit decision.

## Architecture

- `internal/workunit/m9_evidence_test.go` (new): wave-gate barriers force canonical schedules; a tracker proves overlap, bounds, ordering, wave depth (structural critical path) and exact work-tick accounting; scenario D runs every provider attempt through one real `governor.Governor` (MaxInFlight=1) and asserts exactly-once admission/debiting.
- `cmd/runstead/workunit_m9_evidence_e2e_test.go` (new): 4 read-only units at concurrency=4 through the real loop + real governor-owned executor; exact attempts (10), maxFlight=1, unique evidence, per-row `work_unit_id` provenance, exactly-once debiting.
- `cmd/runstead/workunit_m9_bench_test.go` (new): benchmarks only (never run by `go test`); per-cell min/median/max of task duration, provider-time sum (serialized by the real governor), local+overhead remainder, attempts/maxFlight/maxActive; one `M9CELL` line per cell.
- `experiments/m9-workunit-concurrency/run.sh` (new): pinned `go test -bench` invocation; captures dated results under `experiments/m9-workunit-concurrency/results/`.
- `docs/m9-workunit-concurrency-evidence.md` (new): methodology, results, correctness evidence, limitations, explicit decision.
- `specs/004-workunit-m9-evidence-gate/` (new): this slice registration.

## Determinism rules

- No wall-clock assertion in any test; timeouts only as deadlock detectors.
- Barriers/channels prove overlap and exclusion; counters prove bounds/accounting/provenance.
- Benchmarks are measured wall-clock by design, repeated, reported as min/median/max, never asserted.

## Verification plan

| Requirement | Proof |
|---|---|
| Corpus covers fan-out, chain, mixed, governor-constrained | `internal/workunit/m9_evidence_test.go` TestM9EvidenceScenario{A,B,C,D} |
| concurrency=1 baseline; 2 and 4 compared where valid | same corpus + benchmark cells 1/2/4 |
| Bounds never exceeded | wave gates + active-count guards at every concurrency |
| Exclusive units never overlap | scenario C trap (shared over exclusive) + maxActive checks |
| Dependencies respected | scenario B traps + exact order |
| Provider attempts through the same governor | scenario D (real governor, maxFlight=1, attempts==N, debited==1) + E2E |
| Physical request accounting exactly observable | E2E attempts=10, evidence 5/5 unique, debited exactly once |
| Evidence/provenance per Work Unit | E2E per-row-class work_unit_id assertions |
| Verification/parent completion fail-closed | every corpus run ends with GateParent closed and per-unit completed; scenario C retains the exclusive-first order |
| No goroutine leak/data race | `go test -race` on the new tests and full suite |
| Performance not confused with correctness | separate benchmark file, env-dependent, never asserted |
| Report with methodology/results/limitations/decision | `docs/m9-workunit-concurrency-evidence.md` |
| No default/ceiling/parallel-write change | diff contains no production behavior change |

## Validation

`test -z "$(gofmt -l .)"`, `go vet ./...`, `go build ./cmd/runstead`, `go test ./...`, `go test -race ./...`, `bash experiments/protocol/test.sh`, quality gates (growth/errcheck/live-convention), provider-abstraction module gates, `git diff --check`; repeated runs of the new deterministic corpus (count) and race runs of the concurrency paths.