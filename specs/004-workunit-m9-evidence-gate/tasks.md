# Tasks: M9 Work Unit Concurrency Evidence Gate (Stage B1)

Traceability of the issue #53 acceptance criteria to the evidence produced by
this slice (PR: `test(workunit): add M9 concurrency evidence corpus (#53)`).

## Acceptance-criteria traceability

| #53 acceptance criterion | Evidence in this slice |
|---|---|
| Work Units exist as durable Runstead-owned entities independent of model sessions | Already proven by #106/#107 (Stage A); this slice runs every scenario through the durable rows (statuses, evidence refs, provenance) and re-asserts completion/gate on them |
| Serial Work Unit execution and recovery proven before concurrency | concurrency=1 is the baseline of scenarios A/C and equals the Stage A contract (`TestSchedulerConcurrencyOnePreservesSerial` from #109 still green); benchmark cells at concurrency=1 are the baseline column |
| Every executor still uses the normal governor/protocol/policy/tool/verifier path | Scenario D runs ALL provider attempts through ONE real `governor.Governor`; the E2E runs real loops + real governor-owned executor + real verifier, asserting per-row provenance and per-unit verification |
| Concurrent execution has explicit worker, token/request and elapsed-time bounds | Bound never exceeded at 2/4 (active-count guards + wave gates); attempts exactly N (one per unit); elapsed time measured only by the benchmark harness, repeated, never asserted |
| Write conflicts fail closed under runtime policy rather than informal model coordination | Scenario C: the exclusive unit never overlaps anything, still runs first, gate stays closed. The driver-level corpus declares a `write_file` envelope (lane classification proven); the wall-clock harness uses an omitted-envelope exclusive with read-only scripted turns (exclusive-lane serialization measured); unchanged from #109 |
| Work Unit results join through persisted evidence | E2E: evidence ids unique under concurrency, per-unit evidence refs come from the unit's OWN rows, parent final grounded in persisted evidence |
| Defect found by the corpus: shared unit dispatched while an exclusive unit was RUNNING (shared fill raced the exclusive's settle when slots remained) | Reproduced pre-fix in every run (`TestM9EvidenceExclusiveIsolationRegression`, store-authoritative trap, causally synchronized with no timing window: `shared unit dispatched while exclusive unit is running`); fixed minimally in `internal/workunit/scheduler.go` (`activeExclusive` running-lane state blocks ALL dispatch while an exclusive settles); existing exclusive traps hardened to store-status checks in `scheduler_test.go` and the corpus |
| Provider/session death does not erase Work Unit progress | Already proven by #109 crash/restart E2E (unchanged, still green); not re-proven here |
| Multi-agent execution demonstrates a measurable benefit on a corpus BEFORE becoming a default strategy | This PR: scenarios A–D at 1/2/4, deterministic structural proofs + wall-clock min/median/max harness + versioned report with explicit decision; default/ceiling untouched |

## Checklist

- [x] Corpus covers fan-out (A), dependency chain (B), mixed shared/exclusive (C), governor-constrained (D)
- [x] concurrency=1 baseline; 2 and 4 compared where semantically valid
- [x] Correctness/bounds proven deterministically (barriers/counters, no timing)
- [x] No scenario exceeds the concurrency bound
- [x] Exclusive units never overlap; dependencies respected
- [x] Defect found by the corpus (shared dispatched while exclusive running) reproduced deterministically, fixed minimally, regression added
- [x] All provider attempts through the same governor (real governor in D; real executor in E2E)
- [x] Physical request accounting exactly observable (attempts, maxFlight, debits)
- [x] Evidence/provenance per Work Unit correct
- [x] Verification and parent completion fail-closed (GateParent closed everywhere)
- [x] No goroutine leak/data race (`-race` green)
- [x] Performance results kept separate from correctness proofs
- [x] Report includes methodology, results, limitations and an explicit decision
- [x] No default/ceiling/parallel-write change
- [x] Non-Work-Unit code keeps working (full suite green)
- [x] Documentation updated only where the results change documented state