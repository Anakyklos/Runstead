# Tasks: Evidence-Backed Improvement Proposals

**Feature:** `007-improvement-proposals`

**Issue:** #55

- [x] T001 Migration `0016_improvement_proposals.sql` (proposals, evidence join with FK to `tool_results`, versions, validations) + store methods with atomic propose/review/apply/validate/rollback.
- [x] T002 `internal/improvement` typed contract: kinds/targets validation (trusted-kernel targets rejected), fail-closed lifecycle transitions, redaction, provenance validation against real evidence.
- [x] T003 State persistence/recovery tests: reopen store mid-lifecycle, no duplicate application, integrity failures typed.
- [x] T004 CLI `improvement propose|list|show|review|apply|validate|rollback` + E2E full lifecycle (propose -> review -> apply -> version -> use in new task -> validate -> rollback).
- [x] T005 Negative authority E2E: model cannot approve/apply (unknown_tool), workspace prompt-injection fixture stays pending/non-authoritative, secrets redacted in persistence/inspect.
- [x] T006 Contract/scope proofs: pending changes nothing; task under frozen contract unaffected by apply; new task uses revision only via explicit path; project-scope isolation; no capability expansion; no narrative completion/verification.
- [x] T007 Docs: roadmap M10 status refresh, `docs/improvements.md`, architecture section, SpecKit slice.
- [x] T008 Full validation: gofmt, `go test ./...`, vet, build, race, protocol, quality gates; deterministic repetitions for lifecycle/recovery tests.
- [x] T009 PR against `main` with `Closes #55`.

## Acceptance evidence log

| #55 proof | Test |
| --- | --- |
| pending proposal changes nothing | E2E/state: run with pending proposal; frozen contract identical |
| model cannot approve its own proposal | scripted action with improvement tool -> unknown_tool before effect |
| workspace injection cannot become global policy | malicious fixture -> pending only; no active config |
| proposal references real evidence/provenance | propose with durable task+evidence ids; stored refs |
| missing/incompatible evidence fails closed | typed error, no row |
| approval/rejection is explicit control plane | review transitions fail-closed |
| rejected proposal stays inspectable | list/show renders rejected |
| accepted change has explicit version identity | version_id/revision/digest rows |
| started task stays under original contract | apply after run start; contract hash unchanged |
| new task uses revision only explicitly | `--profile` of materialized artifact |
| rollback deterministically restores previous | bytes equality test |
| project-scoped isolation | unrelated scope not listed |
| proposal cannot expand capability | apply changes nothing in registry/policy |
| trusted-kernel target rejected | target validation table |
| no narrative completion/verification | validation requires evidence refs |
| secrets/private payloads absent | redaction fixtures + inspect |
| validation record needs observable evidence | validate without refs fails |
| crash/restart preserves lifecycle | reopen store; single version; transitions stable |
| race suite green | `go test -race ./...` |
## Validation log

| Run | Command | Result |
| --- | --- | --- |
| R1 | `go test ./internal/state/ -run 'TestImprovement' -count=50` | ok (9.6s) |
| R2 | `go test ./cmd/runstead/ -run 'TestImprovement' -count=20` | ok (208.2s) |
| R3 | `go test -race ./internal/state/ -run 'TestImprovement' -count=10` | ok (26.6s) |
| R4 | `go test -race ./cmd/runstead/ -run 'TestImprovement' -count=5` | ok (78.5s) |
| R5 | `go test ./...` | ok (23 packages) |
| R6 | quality gates (self-tests/vet, growth, errcheck, live-convention) | all PASS |

## Maintainer review fix (PR #117 review 5079927113, issue #55)

**Blockers addressed:**

1. **Revision-bound validation.** `ValidateImprovement` now proves each cited
   task's durable frozen execution contract carries the SAME M10 profile
   identity as the applied revision (extracted from the artifact at apply,
   `UNIQUE (target_id, profile_id, profile_version)` makes the mapping
   unambiguous). Cross-revision, phantom and contract-less evidence fail
   closed with `ErrEvidenceRevisionMismatch`; the E2E certifies revision 1
   with revision-1 evidence and revision 2 only with a task that actually ran
   under revision 2.
2. **Digest as integrity gate.** Every version load (`LoadImprovementVersion`,
   validation, rollback) recomputes SHA-256 of the stored artifact bytes and
   fails closed on mismatch. Corruption tests cover load, rollback, validate
   (state) and `show --artifact` (E2E, exit 6).
3. **Provenance coherence.** Source tasks must exist, evidence refs must
   belong to the declared (or evidence-derived) source task set, and work
   units must belong to a declared source task. The foreign Work Unit test
   now uses a REAL unit of another task (the double-`wu-` prefix bug is
   fixed).
