# Implementation Plan: Evidence-Backed Improvement Proposals

**Branch:** `feat/55-improvement-proposals`

**Issue:** #55

**Spec:** `specs/007-improvement-proposals/spec.md`

## Scope

- `internal/improvement` (new, metadata-only): typed `Proposal`, lifecycle
  transitions (fail-closed), kind/target validation, provenance validation,
  redaction. No execution authority.
- `internal/state`: migration `0016_improvement_proposals.sql` + store
  methods (atomic propose/review/apply/validate/rollback, list/show with
  sanitized rendering, evidence FK integrity, version identity).
- `cmd/runstead`: `improvement propose|list|show|review|apply|validate|
  rollback` subcommands.
- Docs: roadmap M10 status refresh, `docs/improvements.md`, architecture
  section.
- SpecKit slice `007`.

## Sequence

1. Migration + state store tests (T001, T003).
2. `internal/improvement` contract/lifecycle tests (T002).
3. CLI wiring + E2E tests (T004).
4. Negative authority / prompt-injection / secrets tests (T005).
5. Version/rollback/recovery proofs (T006).
6. Docs + full validation + repetitions (T007-T009).
7. PR against `main` with `Closes #55` (T010).

## Validation matrix

| Requirement | Evidence |
| --- | --- |
| Proposal persisted separately from authoritative task state | own tables, migration 0016, state tests |
| Scope + source/evidence provenance | validated refs, scope filter test |
| Model cannot approve/apply | no protocol tool; negative CLI/loop test |
| Accepted changes versioned and reversible | version identity + deterministic rollback bytes |
| Workspace content cannot become global policy | malicious fixture E2E |
| M9/M10 reused, no second execution system | composition Profile reuse, contract unchanged |
| Objective later measurement | validation records require existing evidence refs |
| Bad proposal reject/rollback without harming task history | lifecycle/recovery tests |
| Trusted kernel authority unchanged | target rejection tests |

## Out of scope proof

Final diff has no plugin/script/WASM/marketplace/daemon/routing/retry/
scheduler/concurrency changes; no mutation endpoint reachable by the model.