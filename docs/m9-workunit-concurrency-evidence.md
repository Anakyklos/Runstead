# M9 Evidence Gate: Work Unit Concurrency Benefit Evaluation (Stage B1)

**Date**: 2026-08-30
**Issue**: [#53](https://github.com/Anakyklos/Runstead/issues/53) — M9 exit gate
**Evaluated implementation**: Stage B1 bounded shared/exclusive scheduler
(issue #109, PR #110) on top of Stage A durable Work Units (issue #106,
PR #107), plus the suite stabilization of #111/PR #112.
**Slice spec**: `specs/004-workunit-m9-evidence-gate/`
**Reproduction**: `experiments/m9-workunit-concurrency/run.sh`
**Raw captured results**: `experiments/m9-workunit-concurrency/results/m9-2026-08-30T234741Z.txt`

This report answers the M9 gate question:

> Does opt-in read-only Work Unit concurrency produce measurable task-level
> benefit without weakening governor, durability, evidence, recovery or
> verifier?

The answer below is `KEEP_SERIAL_DEFAULT`, with one material defect found and
fixed by the corpus (exclusive-unit isolation; see section 6): the
implementation is correct after that fix, and the opt-in bound is useful in
specific (local-work-heavy) cases, but the measured task-level benefit on the
current read-only workload shapes does not justify changing the
`--workunit-concurrency 1` default or raising the ceiling. The final decision
to close M9 belongs to the maintainer.

---

## 1. Corpus

Two layers, deliberately separated so performance is never confused with
correctness:

1. **Deterministic correctness corpus** (`internal/workunit/m9_evidence_test.go`,
   `cmd/runstead/workunit_m9_evidence_e2e_test.go`) — driver level with real
   SQLite, channel/barrier `RunFunc` seams and the real `internal/governor`,
   plus one ceiling E2E through real governed loops. No wall-clock assertion
   anywhere; timeouts exist only as deadlock detectors.
2. **Wall-clock reproduction harness** (`cmd/runstead/workunit_m9_bench_test.go`)
   — benchmarks (never run by `go test` without `-bench`) at the REAL
   composition root: real driver, real SQLite, real governor-owned executor,
   real agent loops, real tools, scripted provider with configurable
   per-attempt latency. Run by `experiments/m9-workunit-concurrency/run.sh`.

### Scenario A — read-only fan-out

Four independent units, explicit `read_file` envelope, two tool turns + final
each. The only scenario where Stage B1 can shorten the critical path.
Measured at provider latencies 5 ms (local-work-heavy) and 50 ms
(provider-bound).

### Scenario B — dependent chain

Four read-only units in a strict chain (`wu-1 -> wu-2 -> wu-3 -> wu-4`).
Raising the bound must not invent parallelism where the DAG forbids it.

### Scenario C — mixed shared/exclusive

Three shared read-only units plus one exclusive unit. The exclusive unit's
definition differs between the two layers (fidelity note):

- **Driver-level deterministic corpus**: the exclusive unit declares an
  explicit `write_file` envelope (the lane classification - a write-capable
  envelope is exclusive - is what is proven; the fake `RunFunc` executes no
  write).
- **Wall-clock harness**: the exclusive unit has an OMITTED (nil) envelope
  (omitted = task default surface = exclusive lane) and every scripted turn,
  including the exclusive unit's, executes `read_file`. The wall-clock
  numbers therefore measure the scheduler's **exclusive-lane barrier for an
  omitted-envelope unit executing read-only turns**, NOT a write/effectful
  workload. That is the claim used everywhere below for the benchmark layer.

In both layers the exclusive unit is ready from the start, so the
exclusive-first dispatch rule runs it before the shared wave; the scenario
shows how much of the possible gain the exclusive-lane barrier erases.

### Scenario D — governor-constrained

Four concurrent read-only units (concurrency=4) whose provider attempts all
flow through ONE real governor (`MaxInFlight=1`, single-attempt route). The
harness separates scheduler concurrency from the governor-imposed
serialization of physical attempts.

## 2. Methodology

- **Deterministic layer**: wave-gate barriers force the canonical schedule
  and prove the number of units simultaneously active; counters prove bounds,
  ordering, exact work-tick accounting and per-attempt accounting through the
  real governor. Metrics: `maxActive`, wave depth (structural critical path),
  attempts, `maxFlight`, work ticks, unit statuses, `GateParent`.
- **Wall-clock layer**: each (scenario, concurrency, latency) cell runs a full
  task (chain + parent loop) N times (`-benchtime=20x`; each repetition uses a
  fresh workspace, SQLite store and task). Per repetition we measure:
  total task duration; summed provider-attempt time (the real governor
  serializes attempts, so the sum is the serialized provider component);
  local+overhead = total minus provider sum; attempts; `maxFlight`; observed
  `maxActive`. Results are reported as min/median/max over the repetitions;
  nothing is asserted.
- **Environment**: this laptop (Samsung 550XED, 12th Gen Intel i5-1235U,
  31 GiB RAM, Linux), Go 1.22.12, no Docker, default `GOMAXPROCS`. The raw
  log records the full benchmark output for the same environment. Numbers are
  environment-dependent and must not be compared across machines. A
  supplementary 40-repetition spot run of scenario A (not committed) agreed
  with the committed 20-repetition capture on every qualitative conclusion
  (section 4.3).
- **Why provider latency is scripted**: the benchmark must be reproducible
  without a live model endpoint. The 5 ms and 50 ms cells bracket the
  local-heavy and provider-bound mixes; a real network provider (hundreds of
  ms per attempt) lands even further toward the provider-bound column.

## 3. Scenario configuration

| Scenario | Units | Envelope | Dependencies | Turns/unit | Provider latency |
|---|---|---|---|---|---|
| A fan-out | 4 read-only | explicit `read_file` | none | 2 actions + final | 5 ms, 50 ms |
| B chain | 4 read-only | explicit `read_file` | strict chain | 2 actions + final | 15 ms |
| C mixed | 3 read-only + 1 exclusive | harness: OMITTED envelope (read-only scripted turns); driver corpus: `write_file` envelope | none | 2 actions + final | 15 ms |
| D governed | 4 read-only | explicit `read_file` | none | 2 actions + final | 50 ms |

Concurrency compared at `1`, `2` and `4` in every scenario (the ceiling).
The wall-clock harness's scenario C measures the exclusive-LANE barrier for
an omitted-envelope unit executing read-only turns (see section 1); it does
not measure any write/effectful execution.

## 4. Results

### 4.1 Deterministic structural results (correctness layer)

| Scenario | Concurrency | Max simultaneously active | Wave depth (structural critical path) | Bound exceeded | Provider attempts | Physical attempts in flight (max) |
|---|---|---|---|---|---|---|
| A fan-out | 1 | 1 | 4 (= N) | no | — | — |
| A fan-out | 2 | 2 | 2 (= ceil(4/2)) | no | — | — |
| A fan-out | 4 | 4 | 1 (= ceil(4/4)) | no | — | — |
| B chain | 1/2/4 | 1 at every bound | 4 at every bound | no | — | — |
| C mixed | 1 | 1 | 4 (1 exclusive + 3 shared) | no | — | — |
| C mixed | 2 | 2 | 3 (1 exclusive + 2+1 shared) | no | — | — |
| C mixed | 4 | 3 | 2 (1 exclusive + wave of 3) | no | — | — |
| D governed | 4 | 4 | local work: 1 wave; provider lane serialized | no | exactly 4 (1 per unit) | 1 |

Every scenario ends with all units `completed` and `GateParent` closed, and
with exact work-tick accounting (no lost or duplicated work). Exclusive units
never overlap anything (store-authoritative trap in every exclusive test;
`maxActive == min(concurrency, shared)` in the mixed scenario); dependents
never start before their dependency is durably `completed`; the exclusive
unit is never starved.

### 4.2 Wall-clock results (median over 20 repetitions per cell, microseconds)

**Scenario A — fan-out, provider = 5 ms (local-work-heavy):**

| Concurrency | Total (min/med/max) | Provider time (med) | Local+overhead (med) | maxActive | maxFlight |
|---|---|---|---|---|---|
| 1 | 122379 / **150692** / 167712 | 75865 | 75156 | 1 | 1 |
| 2 | 112623 / **126096** / 139728 | 76892 | 48934 | 2 | 1 |
| 4 | 126923 / **140254** / 153574 | 77219 | 63062 | 4 | 1 |

Relative median gain vs concurrency=1: **−16.3 % at 2**, **−6.9 % at 4**. The
concurrency=2 vs concurrency=4 delta is within run-to-run noise (min/max
ranges overlap; a 40-repetition spot run measured 162.8 / 143.0 / 144.7 ms
for 1/2/4, i.e. −12 % at both bounds). The deterministic corpus proves the
scheduler reaches full four-way overlap at concurrency=4; the wall-clock
difference between 2 and 4 is small because the governor-serialized provider
lane is already the dominant cost.

**Scenario A — fan-out, provider = 50 ms (provider-bound):**

| Concurrency | Total (min/med/max) | Provider time (med) | Local+overhead (med) | maxActive | maxFlight |
|---|---|---|---|---|---|
| 1 | 772442 / **790625** / 813517 | 706924 | 84255 | 1 | 1 |
| 2 | 739380 / **750068** / 777909 | 708002 | 42242 | 2 | 1 |
| 4 | 741573 / **747428** / 758013 | 708148 | 38924 | 4 | 1 |

Relative median gain vs concurrency=1: **−5.1 % at 2**, **−5.5 % at 4**.

**Scenario B — chain (15 ms):**

| Concurrency | Total (min/med/max) | Provider time (med) | Local+overhead (med) | maxActive | maxFlight |
|---|---|---|---|---|---|
| 1 | 270241 / **277625** / 290273 | 216126 | 61611 | 1 | 1 |
| 2 | 266082 / **275412** / 295559 | 216182 | 59330 | 1 | 1 |
| 4 | 269792 / **278642** / 294593 | 216114 | 62742 | 1 | 1 |

Relative median change vs concurrency=1: **−0.8 % / +0.4 %** (noise; no
invented parallelism, as required: maxActive stays 1 at every bound).

**Scenario C — mixed shared/exclusive (15 ms):**

| Concurrency | Total (min/med/max) | Provider time (med) | Local+overhead (med) | maxActive | maxFlight |
|---|---|---|---|---|---|
| 1 | 263323 / **276919** / 305218 | 216169 | 61222 | 1 | 1 |
| 2 | 258841 / **269800** / 288852 | 217585 | 51686 | 2 | 1 |
| 4 | 249405 / **260292** / 275244 | 217418 | 43049 | 3 | 1 |

Relative median gain vs concurrency=1: **−2.6 % at 2**, **−6.0 % at 4**
(smaller than the pure fan-out: the exclusive-lane barrier costs one serial
wave - the exclusive unit runs alone even though its scripted turns are
read-only - and the provider lane stays the dominant serialized cost;
`maxActive=3` at concurrency=4 proves the exclusive never overlaps the shared
wave).

**Scenario D — governor-constrained (50 ms):**

| Concurrency | Total (min/med/max) | Provider time (med) | Local+overhead (med) | maxActive | maxFlight |
|---|---|---|---|---|---|
| 1 | 760351 / **767604** / 790602 | 706129 | 61724 | 1 | 1 |
| 2 | 745476 / **755818** / 768424 | 708351 | 47478 | 2 | 1 |
| 4 | 745551 / **756553** / 773325 | 708028 | 47648 | 4 | 1 |

Relative median gain vs concurrency=1: **−1.5 % at 2**, **−1.4 % at 4**.

In every cell `attempts = 14` (4 units × 3 turns + parent × 2) and
`maxFlight = 1`: the scheduler never bypassed the governor, never duplicated
or dropped a physical attempt, and the observed `maxActive` matches the
configured bound exactly where the workload permits overlap (and stays 1 for
the chain, where it must).

### 4.3 Cross-capture stability

Three independent captures were taken (a 5-repetition sanity run and a
20-repetition pre-fix capture that was discarded because it ran against the
buggy scheduler). The committed capture above and a supplementary
40-repetition spot run of scenario A agree on every qualitative point:
C>1 saves wall time on fan-outs (roughly 3–17 % depending on the
local/provider mix), C=2 vs C=4 deltas are within noise, the chain gains
nothing, and physical attempts never overlap (`maxFlight=1`).

## 5. Correctness evidence

All deterministic proofs run in CI with no timing dependency
(`internal/workunit/m9_evidence_test.go`,
`internal/workunit/m9_exclusive_regression_test.go`,
`cmd/runstead/workunit_m9_evidence_e2e_test.go`):

- overlap exists when allowed (wave gates force full-wave occupancy);
- overlap is absent when prohibited (chain, exclusive-lane barrier, governor lane);
- the configured bound is never exceeded (`maxActive` guards + wave gates);
- dependencies are durably respected before dispatch (trap runs);
- exclusive units never overlap anything and are not starved (store-based
  traps, exclusive-first order, `maxActive == min(concurrency, shared)` in
  the mixed scenario);
- every provider attempt passes through the same real governor: exactly one
  attempt per unit, unique client request ids, `maxFlight == 1`,
  `AttemptDebited == 1` per attempt (real governor accounting);
- at the E2E ceiling (concurrency=4, four real loops): 10 provider attempts
  accounted exactly once, evidence ids 5/5 unique, per-row `work_unit_id`
  provenance exact across actions/tool_attempts/provider_attempts/
  verification_attempts, every unit completed with its own passed
  verification, parent completed only after the chain gate closed;
- verification/parent completion stayed fail-closed in every scenario.

## 6. Defect found and fixed by the corpus

The corpus found a material correctness defect in the Stage B1 scheduler:

**Symptom.** In the mixed shared/exclusive benchmark cell at concurrency=4 the
harness observed `maxActive=4` with 3 shared + 1 exclusive (omitted-envelope)
unit: a shared unit was running at the same time as the exclusive unit.

**Root cause.** The scheduler's exclusive isolation rule only blocked NEW
dispatch while an exclusive unit was *ready*. Once an exclusive unit was
dispatched (at `active == 0`), it left the ready list (`ReadyWorkUnits` only
returns `created`/`ready` rows), so the shared fill in the next scheduler
iteration could dispatch read-only units into the remaining slots while the
exclusive was still *running*. The result: at concurrency>1 an exclusive unit
could overlap shared units whenever slots remained after its dispatch — a
violation of the issue #109 contract "while one is active nothing else
starts".

**Deterministic reproduction.** `TestM9EvidenceExclusiveIsolationRegression`
blocks the exclusive unit and trap-checks every shared dispatch against the
exclusive's DURABLE row. The regression is causally synchronized: no
`time.After` and no absence window participates in the verdict. In the fixed
scheduler the pass is entailed by store ordering — a shared unit can only be
dispatched after the exclusive's settle event, which is sent only after the
exclusive's row transitioned to `completed` — so every shared entry reads
`completed`. A reintroduced bug that dispatches while the exclusive's row says
`running` makes the shared entry read `running` and fires. Verified in both
directions during development: the committed test failed in every run
(subtests at concurrency 2 and 4, ~50 ms each) against the pre-fix scheduler
with `shared unit dispatched while exclusive unit is running`, and passes 10/10
against the fix. The existing channel-based exclusive traps in
`scheduler_test.go` missed the defect because they relied on the test goroutine
releasing the exclusive before the scheduler's next iteration.

**Minimal fix** (`internal/workunit/scheduler.go`): the scheduler now tracks
`activeExclusive` — whether a dispatched-but-not-settled unit is exclusive —
set at dispatch and cleared when that unit's settle event is processed. While
it is set, the shared fill refuses to dispatch anything (`settle()` waits for
the exclusive to settle). No other behavior changed: exclusive units still
start only at `active == 0`, the ready-exclusive anti-starvation rule is
unchanged, and `concurrency=1` remains the Stage A serial contract.

**Regression hardening.** The new deterministic regression test is committed;
the existing channel-based traps in `scheduler_test.go`
(`exclusiveTrapDriver`, exclusive/exclusive trap) and the corpus scenario C
were converted to store-authoritative status checks so a reintroduction of
the defect fails deterministically instead of by timing luck.

**Post-fix verification.** With the fix, the mixed benchmark cell reports
`maxActive=3` at concurrency=4 (exclusive alone + shared wave of 3), the
corpus scenario C asserts `maxActive == min(concurrency, shared)`, and the
full scheduler/corpus/E2E suites pass repeatedly and under `-race`. Captures
taken against the pre-fix scheduler were discarded; the committed results
file corresponds to the fixed code.

## 7. Limitations

- Wall-clock numbers are environment-dependent (laptop CPU, no Docker, no
  load isolation; min/median/max over 20 reps; spread up to ~30 % between min
  and max in the fastest cells; concurrency=2 vs concurrency=4 deltas at 5 ms
  provider latency are within noise). They must not be compared across
  machines or CI runs; they are for THIS environment only.
- The provider is scripted with a fixed latency; real network providers add
  per-attempt latency that would push the mix further toward the
  provider-bound column (smaller relative benefit).
- The read-only tool set (`read_file`, `list_files`, `search_text`,
  `git_status`, `git_diff`) performs inherently fast local work; the 5 ms
  cell is already the optimistic local-heavy bound for the current tool set.
  Context-heavy or verification-heavy units are not representable with the
  current read-only envelope.
- `run_recipe`, writes and unknown-capability units never join the shared
  lane (correct by design), so no measurement exists for concurrent
  effectful work: the exclusive serialization cost is fully paid.
- The parent loop tail is serial in every cell and is included in `total`;
  relative gains are therefore conservative.
- E2E at the ceiling was re-proven for the fan-out/governed shape only; the
  chain and mixed shapes are proven deterministically at driver level (the
  scheduler behavior is identical; only the DAG shape differs).
- No live-endpoint measurement was attempted (opt-in live smoke tests remain
  out of scope for the gate environment).
- Scenario D uses a real governor instance at the driver seam rather than a
  full live loop; the real-loop equivalent is covered by the E2E
  (concurrency=4, `maxFlight=1`, exactly-once debiting).

## 8. Conclusion

Decision: **KEEP_SERIAL_DEFAULT** — after the exclusive-isolation fix of
section 6, Stage B1 is correct and useful in specific cases, but `1` remains
the default.

Rationale, in evidence order:

1. **Correctness, after the fix, is not in question.** The deterministic
   corpus proves overlap only where the DAG and the envelope allow it, bounds
   are never exceeded, exclusive units never overlap anything (the one defect
   found was reproduced, fixed minimally and locked with regressions), all
   provider attempts flow through the same governor with exactly-once
   accounting, and verification/parent completion stay fail-closed.
2. **The benefit is real but small and workload-dependent.** The
   governor-serialized provider lane dominates realistic read-only workloads,
   so scheduler concurrency can only hide the local-work remainder: −3…5 % on
   provider-bound fan-outs, −7…17 % on an optimistic local-heavy fan-out
   (concurrency=2 already captures most of it; 4 adds little), −3…6 % with an
   exclusive-lane barrier, ≈0 % (correctly) on a dependency chain.
3. **The default stays 1.** None of the measured gains justify changing the
   default for all tasks; the opt-in `--workunit-concurrency 2/4` remains
   available for operators with genuinely parallel, read-only, local-heavy
   fan-outs (e.g., many independent repository inspections).
4. **The ceiling stays 4**; measured C=4 vs C=2 differences are marginal and
   within noise for these workload shapes.

The M9 gate question is therefore answered with evidence: bounded read-only
Work Unit concurrency is implemented safely (with the isolation fix shipped
in this PR), its benefit is measurable only when local work is a material
fraction of unit time, and no change to default or ceiling is supported by
this corpus. Closing M9 and any expansion (deeper Stage B, parallel writes,
model-created units) remain maintainer decisions under issue #53; no further
work was started by this slice.