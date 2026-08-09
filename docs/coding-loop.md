# The inspect-edit-test-fix coding loop

Issue #12 completes M4's primary capability: the runtime composes the
existing foundations — read-only inspection (#6), safe writes (#10), bounded
process recipes (#26), independent verification (#11), durable state (#8) and
recovery (#9) — into a real coding loop that runs through multiple
model/tool cycles, observes real failures, corrects them, and can only reach
`completed` through the control-plane verifier.

This document describes the loop as implemented, its failure classification,
the #12 loop guards, the deterministic sample repository, the required
regressions, and the honest status of the live OmniRoute scenario.

## The loop

```text
inspect repository
  -> identify a scoped change
  -> persist and execute a safe write (write_file/apply_patch)
  -> run an explicit test/build recipe through #26 (run_recipe)
  -> observe a REAL failure (non-zero exit, bounded output, evidence ID)
  -> inspect the relevant code and the process evidence
  -> apply a corrective write
  -> rerun the same recipe (allowed: the workspace changed)
  -> propose completion
  -> the runtime verifier independently observes the real filesystem,
     real git state, persisted evidence and the operator acceptance plan
  -> completed only when every check passes and no effect is uncertain
```

The model never executes a tool, never supplies a command, never approves an
effect and never marks a check passed. It proposes one envelope per turn; the
runtime decides existence, arguments, capability, approval, effect, evidence
and completion (see [architecture.md](architecture.md)).

A recipe observation whose real exit code is non-zero is **recoverable
evidence**, not a terminal failure: the observation carries the recipe id,
the real exit status, the terminating signal, timeout/cancellation flags,
bounded stdout/stderr with truncation flags and the evidence ID, and the loop
continues to the next governed model turn. Process output is untrusted
observation data; it never becomes a system instruction (the transcript role
separation from #7/#26 is preserved).

## Failure classification

The runtime distinguishes, with typed outcomes and stop reasons:

**Recoverable (the loop continues):**

- a test/build recipe returned non-zero: citable `recipe.Evidence` with the
  real exit status is returned to the model, and the run continues;
- a verification check failed (`DecisionFailed`): the structured verification
  result returns to the loop under the `verification` role and execution
  continues (already #11; exercised by the #12 scenario);
- a stale write (`stale_state`): the model re-inspects and re-proposes;
- a valid action that hit a corrigible environmental error (for example
  `path_not_found`, `read_failure`): a failed observation with its typed
  `FailureCode` returns to the model;
- the model needs to inspect another file: that is a normal next turn.

**Blocking / terminal / human-review (the loop stops or pauses):**

- uncertain effect (interrupted attempt): `verification_blocked` /
  `human_review_required`, never auto-reexecuted;
- pending approval: `approval_required` pause, never model-approved;
- capability denied / policy fail-closed: `corrections_exhausted` after the
  correction budget, with the typed policy reason;
- provider/account circuit terminal for the task budget:
  `provider_budget_exhausted`, `account_circuit_open`, `account_delay_timeout`;
- loop limits reached: `steps_exhausted`, `corrections_exhausted`,
  `repeated_action`, `time_budget_exhausted`, and the #12 guards below;
- an effect that cannot be reconciled: `human_review_required` at resume;
- control-plane verification blocked (`DecisionBlocked`): the task stays
  durably resumable.

There is no hidden retry loop: every continuation consumes the normal agent
loop budgets (model turns, provider attempts through the governor, time,
corrections, repeats, and the #12 failure guards).

## #12 loop guards

The repeat guard (#5/#9) rejects an identical proposal only while the
workspace signature is unchanged. It cannot classify two new failures: a
model that keeps proposing **different** failing actions, or keeps proposing
`complete` while the verifier keeps rejecting. The #12 guards bound exactly
that unproductive repetition with typed outcomes; each counted failure
already consumed a normal model/tool turn.

| Guard | Limit | Counted when | Typed outcome when exceeded |
| --- | --- | --- | --- |
| consecutive tool/process failures | `--max-consecutive-failures` (default 5, `RUNSTEAD_MAX_CONSECUTIVE_FAILURES`) | a failed tool observation, or a `run_recipe` observation whose real exit code is non-zero; any successful observation resets the streak | `consecutive_failures_exhausted` (exit 34) |
| repeated verification failures | `--max-verification-retries` (default 3, `RUNSTEAD_MAX_VERIFICATION_RETRIES`) | a verification attempt decided `failed`; a passed/blocked/uncertain decision resets the streak | `verification_failures_exhausted` (exit 35) |

Both counters are part of the loop budgets persisted in the task
configuration snapshot, so resume continues under the same allowances. Both
counters survive restart: the recovery pipeline recomputes the trailing
streaks from the persisted attempt and verification history and seeds the
resumed loop (`internal/recovery`, `agent.RecoverySeed`). A legitimate coding
loop never trips the guards: the required trajectory `fail -> write ->
fail -> write -> pass` has streaks of at most one, because the successful
writes reset the failure streak.

The guards are not an idempotency mechanism: fingerprints remain repeat/loop
evidence only, never a result-reuse key.

## Bounded context during the loop

The model-facing transcript carries, per turn, the original objective, the
system contract, every observation (bounded), every correction, and every
verification result (bounded, `verification` role). On resume, the recovery
context (`internal/recovery.BuildContext`, 32 KiB budget) represents the
original objective, verified progress with evidence IDs, unresolved failures,
uncertain attempts, the pending verification failure and the consumed loop
budgets. Acceptance checks reach the model only as the per-check results of a
failed verification attempt (the operator plan itself is never model input).
This is the minimal projection required for the M4 loop; the M8 context
compiler, vector memory and RAG are explicitly out of scope.

## Deterministic sample repository

`fixtures/coding-loop/` is a deliberately small but real repository: a Go
module whose test suite genuinely fails until the implementation is
corrected. It requires inspection of multiple files, two scoped writes, a
real failing `go test` run, diagnosis from the real bounded process evidence,
a corrective write, a passing rerun, real Git observation, a real acceptance
plan and a passed verification. The fixture is documented for #13 reuse
(interruption seams, provider failure, process failure, write uncertainty) in
its own README.

The deterministic trajectory is produced by the scripted provider
(`--scripted`); filesystem, git, process and verifier are real. The E2E tests
(`cmd/runstead/coding_loop_test.go`) copy the fixture into a fresh directory,
initialize a real git repository with a committed baseline, and run the real
CLI composition:

```text
read app/calc.go
run_recipe test            -> exit 1 (real go test failure)
read app/calc_test.go
write app/calc.go          -> first (insufficient) fix
run_recipe test            -> exit 1 (still red)
read app/calc.go
write app/calc.go          -> corrective fix
run_recipe test            -> exit 0
final complete             -> verifier passes (recipe_exit_zero + file_hash)
```

A premature `complete` proposal while the suite is red is refused
(`recipe_exit_nonzero`) and returns to execution. Interruption after real
progress (inspection + failing test + first write) is resumed without
duplicating effects, without reconstructing the completed recipe as if it
never happened, and completes with the corrective trajectory.

## Final evidence report

`runstead inspect <task-id>` renders, from the persisted verification report:

- which acceptance checks passed (check id, type, status, expected,
  observed, evidence IDs, reason);
- the real process attempts with their exit codes
  (`Process attempts: ... recipe=test exit=1/0 ...`);
- the write attempts with their effect hashes;
- a `Git observation:` section with the real `git status`, the pre-existing
  vs during-task changed files and the baseline-truncation limitation.

The verified completion summary is produced by the verifier from the
acceptance checks; the model's free text is surfaced only as an unverified
note. Before/after hashes, evidence IDs and git attribution all come from
authoritative runtime state, never from model claims.

## Live OmniRoute scenario — honest status

The live acceptance criterion of #12 (ChatGPT Web through OmniRoute under
the same governor and evidence rules) remains **blocked by external
prerequisites**: `#29` (producer-side attempt receipts) -> `#30` (protected
live activation) -> `#4` (OmniRoute live provider). Nothing in this issue
relaxes attempt-receipt accounting, bypasses the governor or invents
receipts. The deterministic offline scenario is the shipped proof of the
loop; the opt-in manual live harness remains `experiments/protocol`
(`run.sh --live`), which requires live credentials and fails closed without
them. `runstead run` with OmniRoute configuration fails closed with the
typed blocker diagnostic, covered by
`TestCodingLoopLivePathFailsClosed`. The maintainer decides whether the live
criterion stays in #12 or moves to the #14 gate; this issue implements and
proves the deterministic #12 core.

## Deferred scope

Not implemented here: generic shell, automatic git commit/push, deploy,
network tools, first-party ChatGPT Web connector, browser automation, the M8
context compiler, Work Units, multi-agent, capability-package frameworks,
automatic model routing, self-improvement, the full #13 chaos suite and the
#35 CI overhaul.
