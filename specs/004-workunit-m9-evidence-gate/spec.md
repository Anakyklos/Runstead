# Feature Specification: M9 Work Unit Concurrency Evidence Gate (Stage B1)

**Feature Branch**: `test/53-m9-workunit-concurrency-evidence`

**Created**: 2026-08-30

**Status**: Implemented (evidence collected)

**Input**: Issue #53 — M9 exit gate: "Multi-agent execution demonstrates a
measurable benefit on a corpus before becoming a default strategy." This
slice does NOT implement new concurrency. It builds the reproducible,
evidence-backed evaluation of the Stage B1 bounded shared/exclusive scheduler
(issue #109, merged via PR #110) that lets the maintainer close the M9 gate
with evidence, including if the answer is "the current benefit does not
justify expansion".

## Scope

Smallest corpus/harness able to compare the current behavior at
`workunit-concurrency` 1, 2 and 4 (where the scenario has enough parallelism),
covering:

- **Scenario A — fan-out**: independent, explicitly read-only Work Units; the
  only scenario where Stage B1 can shorten the critical path.
- **Scenario B — dependency chain**: read-only units with sequential
  dependencies; raising the bound must NOT invent parallelism where the DAG
  forbids it.
- **Scenario C — mixed shared/exclusive**: read-only units plus one exclusive
  unit; exclusivity must stay respected and the effectful barrier must show
  how much of the possible gain disappears.
- **Scenario D — governor-constrained**: concurrent units whose provider
  attempts all flow through the same governor; the harness separates
  scheduler concurrency, local work, provider-attempt time and
  governor-imposed serialization.

## Deliverables

1. Deterministic correctness corpus: `internal/workunit/m9_evidence_test.go`
   (barriers/channels/real SQLite/real governor; NO wall-clock assertions).
2. Ceiling E2E at concurrency=4 through real governed loops:
   `cmd/runstead/workunit_m9_evidence_e2e_test.go`.
3. Wall-clock reproduction harness (benchmarks, never CI assertions):
   `cmd/runstead/workunit_m9_bench_test.go` + runner
   `experiments/m9-workunit-concurrency/run.sh` + captured results.
4. Versioned report: `docs/m9-workunit-concurrency-evidence.md` ending in an
   explicit decision (KEEP_SERIAL_DEFAULT / EVIDENCE_SUPPORTS_BOUNDED_CONCURRENCY
   / NO_MATERIAL_BENEFIT / INSUFFICIENT_EVIDENCE / CORRECTNESS_BLOCKER).

## Hard boundaries (unchanged)

- `--workunit-concurrency 1` stays the default; the ceiling stays 4.
- No parallel writes, no concurrent `run_recipe`, no model-created units, no
  general multi-agent orchestration.
- No change to governor, policy, evidence, verifier, recovery or durability.
- Performance results are never converted into CI timing thresholds.

## Constitution Check

- Local durable state authoritative: the corpus reads unit/attempt rows from
  the same SQLite the scheduler writes; ready selection unchanged. PASS.
- Model never gains authority: fixtures are operator-defined units only. PASS.
- No hidden amplification: the governor-constrained corpus counts attempts
  exactly once against the REAL governor. PASS.
- Evidence/verifier authority: unit completion still requires the unit's own
  passed verification in every scenario. PASS.
- Fail-closed: exclusive/unknown/omitted envelopes stay exclusive; bounds are
  never exceeded. PASS.
- Bounded/deterministic: every CI assertion is channel/barrier/count-based;
  timeouts exist only as deadlock detectors. PASS.

## Decision template

The report conclusion must pick exactly one of:

- `KEEP_SERIAL_DEFAULT` — Stage B1 is useful in specific cases, but `1` stays
  the default;
- `EVIDENCE_SUPPORTS_BOUNDED_CONCURRENCY` — material reproducible benefit, but
  any default change still requires maintainer decision;
- `NO_MATERIAL_BENEFIT` — implementation correct, workloads do not justify
  expansion;
- `INSUFFICIENT_EVIDENCE` — no basis to claim benefit;
- `CORRECTNESS_BLOCKER` — a material defect blocks accepting Stage B1.