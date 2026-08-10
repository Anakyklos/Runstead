# Independent verification and completion checks

Issue #11 makes task completion a **runtime decision**, never a model claim.
A valid `<runstead_final status="complete">` envelope is only a **proposal**.
Before a task may be persisted as `completed`, the control-plane verifier
(`internal/verifier`) independently observes authoritative state — persisted
evidence, the real filesystem, real Git state and the operator acceptance
plan — and produces a structured verification report. Completion is permitted
only when every mandatory check passes and no uncertain effect or pending
approval remains.

```text
model proposes completion
→ protocol parses the final response
→ Runstead verifier independently observes authoritative state
→ acceptance checks evaluated
→ completion gate
     PASS   → completed (with the persisted verification report)
     FAIL   → structured verification observation → execution continues
     BLOCKED/UNCERTAIN → completion refused; task stays durably resumable
```

The verifier is control plane, not a tool the model controls. Model prose,
summaries, reasoning, exit code 0 and tool output are never inputs to the
decision, and model prose is never verified output either: a completed task's
summary is produced by the verifier from the acceptance checks, and the
model's final text is at most an unverified note (see "Model text is never
verified content" below). There is no path in the runtime by which a task is
marked completed only because the model said so: the state layer
independently refuses to finalize `completed` without a persisted `passed`
verification attempt.

## What constitutes evidence

Evidence is **persisted observation data** produced by the runtime:

- `tool_results` rows (one per successful observation, keyed
  `(task_id, evidence_id)`), including `write_file`/`apply_patch`
  `WriteEvidence` with before/after hashes, and `run_recipe`
  `recipe.Evidence` with the real exit status, signal, duration, bounded
  output and truncation flags;
- the real filesystem (via the same canonical workspace resolver as the
  tools) and real bounded Git observations (`git status`/`git diff` through
  the same git seam as the tools);
- the operator acceptance plan.

The model cannot create evidence by asserting something. A final response's
cited evidence is **typed**: each citation is `{"evidence_id": "...",
"tool": "..."}` and the model must declare the tool that produced the
evidence it cites. A fabricated, foreign or type-incompatible ID is rejected:
a `read_file` observation can never be cited as `run_recipe` evidence.

Completion is **fail closed without acceptance criteria**. A `status="complete"`
proposal is refused `blocked` when no operator acceptance plan with at least
one check exists for the task: without a task-specific acceptance criterion, a
completion proposal can never be proven against the task objective. The task
stays durably resumable, and the operator attaches a plan at resume with
`--acceptance` (an operator-owned act, persisted with the task; the model can
never attach or modify acceptance criteria).

## Acceptance checks

The acceptance plan is an **operator-provided, versioned, typed JSON file**
(`--acceptance FILE` or `RUNSTEAD_ACCEPTANCE_PLAN`), read once at task start
and persisted with the task. The model can never invent or modify its own
acceptance criteria after execution. There is no DSL, expression engine or
scripting: the verifier implements exactly this small typed set:

| Type | Fields | Meaning |
| --- | --- | --- |
| `file_exists` | `path` | the file must exist in the workspace |
| `file_absent` | `path` | the file must be absent |
| `file_hash` | `path`, `sha256` | the complete file must match the sha256 |
| `recipe_exit_zero` | `recipe`, `require_untruncated?` | one executed `run_recipe` evidence for the id with started=true, exit code 0, no timeout/cancellation/signal |

Example:

```json
{
  "version": 1,
  "checks": [
    {"id": "artifact", "type": "file_exists", "path": "dist/app"},
    {"id": "tests-pass", "type": "recipe_exit_zero", "recipe": "test", "require_untruncated": true}
  ]
}
```

Checks have stable ids and are individually inspectable (`runstead inspect`).
The parser is strict: unknown fields, duplicate keys, unsupported versions,
trailing JSON and duplicate check ids are rejected fail-closed.

Every verification attempt also runs **structural checks** that exist for every
task, with or without an operator plan:

- `evidence_grounded`: every cited evidence ID exists in the task's persisted
  evidence;
- `evidence_claims_typed`: every cited evidence's declared tool matches the
  persisted tool of the evidence row;
- `no_uncertain_attempts`: no interrupted/uncertain/human-review attempt;
- `no_pending_approvals`: no operator approval is still pending;
- `writes_reconciled`: the latest persisted write of every target path
  matches the current filesystem (created files exist, hashes match). A
  corrective write in the #12 coding loop legitimately supersedes an earlier
  write to the same path: the earlier write is recorded in the report as
  superseded (its intermediate state no longer exists), and only the latest
  write must match the current file — so the final state is provably the
  state the task's own last write produced;
- `git_observed`: real Git status/diff captured, with change attribution and
  an explicit limitation when the task-start baseline was truncated;
- `acceptance_criteria_required`: an operator acceptance plan with at least one
  check exists; without one, completion is refused `blocked` (fail closed).

The verifier report is bounded: `Limits.MaxChecks` caps the checks evaluated
in one attempt (an oversized plan is refused `blocked` with
`check_budget_exceeded`, never partially proven), and `Limits.MaxObservedChars`
truncates every expected/observed/reason description with an explicit marker.

## Completion gate

`runstead` persists `completed` only when the latest verification attempt of
the task decided `passed`. The gate is enforced in **two layers**:

1. the agent loop runs the verifier for every `status="complete"` proposal and
   only then stops with `OutcomeCompleted`;
2. `FinalizeTask` in the state layer refuses `completed` unless a persisted
   verification attempt with decision `passed` exists — so no alternate code
   path can finalize a task as completed without a valid verification.

## Failed verification returns to execution

A failed verification is **not** a protocol correction: the model produced a
valid final; the environment did not satisfy completion. No correction budget
is consumed. The structured verification result (check id, expected, observed,
evidence ids, typed reason) is appended to the transcript under the
`verification` role as a bounded observation and execution continues. The
existing loop budgets (max steps, provider budget, time budget) bound the
continuation; there is no hidden retry loop inside the verifier.

`blocked` and `uncertain` are distinct from `failed`:

- `uncertain`: an authoritative effect is uncertain (interrupted attempt,
  human-review-required state) — completion is refused until reconciled;
- `blocked`: a control-plane dependency cannot be satisfied by the model (a
  pending operator approval at completion time, a check that cannot be
  evaluated, or a missing operator acceptance plan).

Both stop the run with `OutcomeVerificationBlocked`; the task is **not**
finalized and stays durably resumable so the operator can reconcile or decide
before a normal `runstead resume`.

## Truncation semantics

Truncation is recorded explicitly in the report (`truncated_evidence`) and
never silently ignored, but it does not automatically fail every check:

- a `recipe_exit_zero` check without `require_untruncated` is satisfied by a
  real exit status 0 even when output was truncated — the conclusion (the
  process reported success) does not depend on the missing output;
- `require_untruncated: true` fails the check when stdout or stderr was
  truncated, because the operator's conclusion depends on the full output;
- a claim that depends on content that could be in the truncated part (for
  example "37 specific tests passed") can never be supported by truncated
  evidence.

## Model text is never verified content

Model prose is never a verified statement. A completed task's summary is
produced by the **verifier** from the acceptance checks and authoritative
evidence (for example `completion verified: acceptance check passed
(artifact)`), never from the model's free-text `summary`. The model's own
final text is surfaced only as an explicitly labeled **unverified note**
(`note (unverified): ...` in the CLI output) and is never persisted as the
task summary. A final like `summary: "tests passed"` therefore cannot become
a verified completion claim: without a `recipe_exit_zero` acceptance check
(and real executed `recipe.Evidence` backing it), "tests passed" is at most
an unverified note next to a completion verified by whatever acceptance
checks actually exist.

## Normal completion output versus inspect

After a task is durably finalized as `completed`, both `runstead run` and
`runstead resume` print a bounded `Verified runtime result:` projection. This
projection is loaded from durable task state and the latest persisted verifier
report, so its outcome, verifier decision, acceptance checks, evidence IDs,
Git attribution/diff and recipe process results do not come from the model's
final response. The projection is emitted only when the persisted task outcome
is `completed` and the latest verifier decision is `passed`.

For failed, blocked, uncertain, approval-paused or incomplete tasks, the CLI
continues to print only the existing typed outcome and diagnostic evidence; it
does not present a completed report. `runstead inspect <task-id>` remains the
historical and detailed view when the operator needs the full event journal,
all attempts, policy decisions, recovery state or every verification attempt.

## Recipe and process evidence

A `recipe_exit_zero` acceptance check is satisfied only when there is real
`recipe.Evidence` from an executed `run_recipe` through the #26 bounded
process runner and capability policy. The verifier checks the recipe id, the
concrete execution, the real exit status, terminating signal, timeout and
cancellation flags, truncation and duration. `exit code == 0` proves only that
that process reported success; it never automatically proves task completion.
The verifier performs **no process execution**: it only reads the persisted
evidence produced by the #26 boundary (structural test: the verifier package
imports no process-execution packages).

## Git attribution

Changed files are derived from **real Git observation**, never from the model
response. At task start the runtime captures a bounded Git baseline
(`workspace_baselines`, including the truncation flags of the bounded
baseline); at verification time it captures the current state and attributes
changes:

- `pre_existing`: files already changed when the task started (the baseline),
  never silently attributed to the task;
- `during_task`: files changed during the task (present now but not in the
  baseline, or with a different status).

Git attribution is a "where practical" observation: a non-Git workspace does
not block completion, but the limitation is recorded in the report and shown
in inspect. When the task-start baseline was itself truncated, the limitation
is recorded explicitly (`baseline_truncated`) because pre-existing changes
outside the truncated baseline window may be attributed as during-task; the
flags are persisted with the baseline (migration 0009) so the limitation
survives restart.

## Persistence

Verification is part of the authoritative task history (migrations 0008-0009):

- `acceptance_plans`: the persisted operator plan (spec + digest). A task run
  without a plan has no row; the operator may attach one at resume (the
  operator-owned continuation of the fail-closed completion gate);
- `workspace_baselines`: the bounded Git baseline captured at task start with
  its truncation flags (migration 0009);
- `verification_attempts`: one control-plane verification run (decision,
  bounded report, summary) with its journal event (`verification_recorded`);
- `verification_checks`: per-check results, individually inspectable.

Every projection change and its journal event commit in one SQLite
transaction **after** the external observations complete; no transaction is
open during filesystem/Git observation. The acceptance plan digest is
persisted in the task configuration; resume rejects a divergent `--acceptance`
override fail-closed and loads the persisted plan from state by default.

`runstead inspect <task-id>` renders the Verification section: each attempt's
decision and summary, and each check's id, type, status, expected, observed,
evidence ids and typed reason.

## Honest limitations

- Git attribution distinguishes pre-existing from during-task changes "where
  practical"; git cannot attribute a concurrent external edit.
- The verifier observes the filesystem and git through the same seams as the
  tools; it does not add kernel-level guarantees.
- Verification checks what the typed checks express. An operator plan that
  omits a condition cannot be invented by the verifier; without any plan,
  completion is refused (fail closed) until the operator supplies acceptance
  criteria.
- A task whose final is `incomplete` ends honestly without a completion
  decision; verification gates only `completed`.
