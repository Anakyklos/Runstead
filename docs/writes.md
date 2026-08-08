# Safe writes

Issue #10 introduces the first controlled write capabilities into Runstead:
`write_file` and `apply_patch`. This document describes their safety model,
durable lifecycle, approval modes, evidence and crash behavior. It is
intentionally honest about limitations: nothing here claims atomicity between
SQLite and the filesystem, exactly-once execution, or protection beyond the
documented invariants.

## Scope and non-goals

- Only local filesystem mutation inside the selected workspace is supported.
- There is **no generic shell**, **no arbitrary subprocess execution**, no
  build/test command execution, no network tools, no automatic `git commit`,
  `git push`, package publication or deployment. Those belong to later
  milestones (#26 for process execution, #11 for verification).
- Runstead never pretends filesystem writes are atomic with SQLite
  transactions. The durable-execution contract
  (`docs/adr/0001-durable-execution.md`) fixes the two-transaction ordering:
  intent first, effect outside SQLite, observed result second.

## Tools

### `write_file`

Writes a UTF-8 text file inside the workspace.

```json
{"path":"src/main.go","content":"package main\n...","expected_before_hash":"<sha256 or \"absent\">"}
```

- `path` must be a relative path inside the workspace; the parent directory
  must already exist.
- `content` is the complete new file content (bounded by
  `MaxWriteBytes`, default 256 KiB).
- `expected_before_hash` is the stale-state precondition: the sha256 of the
  current file content as reported by `read_file`, or the literal `absent`
  when the file must not exist yet.

`read_file` now reports `sha256` for the complete file content (never
truncated, even when the returned content is bounded), so a model can always
produce a valid precondition for the state it actually observed.

### `apply_patch`

Applies a strict, deterministic subset of unified diff to one existing file.

```json
{"path":"src/main.go","patch":"--- src/main.go\n+++ src/main.go\n@@ -1,3 +1,3 @@\n ...","expected_before_hash":"<sha256>"}
```

Accepted format:

- `--- <path>` and `+++ <path>` headers whose paths match the tool target
  (optionally quoted, or with `a/`/`b/` prefixes);
- `@@ -S,C +S,C @@` hunk headers whose counts match the body (context lines
  count on both sides);
- context (` `), removal (`-`) and addition (`+`) lines.

The patch is parsed and applied **entirely in memory before any filesystem
mutation**. A patch either applies cleanly (all hunks match, all-or-nothing)
or is rejected as a typed failure without touching the file. Unsupported
constructs (`index`, `mode`, `rename`, `\ No newline` markers, fuzzy
matching) are rejected. No shell `patch` command is involved. The patch
argument is bounded (`MaxPatchBytes`, default 128 KiB) and the target file
(`MaxPatchTargetBytes`, default 4 MiB).

## Workspace security boundary

Write paths reuse the canonical resolver and path security model from the
read-only tools (#6). Writes fail closed for:

- absolute paths (`absolute_path`);
- `..` traversal components (`path_traversal`);
- symlink escapes: a write never follows or replaces a symlink, even one that
  points inside the workspace (`symlink_escape`);
- resolved paths outside the configured workspace;
- missing parent directories (`path_not_found`);
- non-regular targets such as directories (`wrong_type`);
- malformed or ambiguous paths.

Read and write paths share the same canonical security model: there is no
second path-validation implementation.

## Stale-state protection

A write never overwrites state the model did not observe. `expected_before_hash`
must equal the sha256 of the current file content (or `absent` for a new
file); any mismatch refuses the write with the typed outcome `stale` and the
file is left untouched. This makes a stale repeated write fail rather than
silently overwrite newer state.

### Effect-boundary revalidation (TOCTOU)

The initial before-state validation happens before the effect; a concurrent
external process could modify the target, create an originally-absent target,
or swap a path component to a symlink in the gap before the rename. Runstead
therefore revalidates immediately at the effect boundary and again right
before the rename:

- canonical containment is re-checked (`EvalSymlinks` of the parent and the
  `filepath.Rel` boundary check), and the resolved canonical target must equal
  the one validated at the start of the effect: a path or parent component
  swapped to a symlink aborts with `symlink_escape`;
- the before-state is re-checked against the CURRENT file content: an external
  modification, or a concurrently-created target where `absent` was the
  precondition, aborts with `stale_state` and the external state is preserved;
- the final-component write never follows a symlink (Lstat rejects it), so a
  symlink swapped in as the target is refused rather than traversed.

This is a **revalidation, not a compare-and-swap**. The Go standard library
offers no atomic rename-if-unchanged primitive (no `openat`/`dirfd`-based
rename), so a modification landing after the last revalidation and before the
rename is not detectable by the writer. Runstead does not claim CAS or
per-file atomicity beyond the rename itself. The deterministic test seam
(`tools.SetWriteRaceHook`) proves the revalidation aborts external
modification, concurrent creation and symlink swaps.

The following cases stay distinct:

| Case | Outcome |
| --- | --- |
| Exact repeated write (same fingerprint, unchanged workspace) | rejected by the repeat guard; no new attempt |
| Stale repeated write (precondition no longer matches) | `stale`, no effect |
| No-op write (content already matches) | `noop`, file untouched, hashes equal |
| Successful new write | `success` with before/after evidence |
| Uncertain previous write | reconciled from filesystem state or `human_review_required` |

An action fingerprint is loop/repetition evidence only. It is never an
idempotency key and never proof that an effect happened.

## Durable lifecycle

Every write follows the ADR transaction ordering:

```text
TX 1: persist action + attempt intent, mark prepared, persist the
      deterministic expected after-state hash (effect_after_hash), append
      event; COMMIT
      → perform the filesystem effect OUTSIDE SQLite (temp-file + rename)
      → observe the resulting state
TX 2: persist result/evidence, update projections, append event; COMMIT
```

- If TX 1 cannot commit, the write never starts.
- If the process dies after TX 1, the persisted `prepared` attempt carries the
  expected after-state hash; recovery reconciles it from the filesystem.
- No SQLite transaction is ever held open across the filesystem effect.
- The filesystem effect is a temp-file-plus-rename in the target directory, so
  a crash mid-write leaves either the old or the new file, never a torn
  partial write. The temp file is fsynced before the rename.
- There is no exactly-once semantics. An interrupted write is never blindly
  repeated.

## Approval and policy

Write actions are gated by a control-plane policy seam (`internal/policy`)
before any execution decision. The static policy maps each write tool to a
mode:

- `allow`: the write executes without a separate approval step;
- `deny`: the write never executes;
- `approval_required` (default): the write executes only after the operator
  records an approval.

Policy decisions are persisted (`write_policy_decisions` rows plus journal
events) with typed reasons before any execution decision. The following never
count as approval:

- model prose, reasoning or a field invented by the model;
- repository content;
- tool output.

Approvals are operator control-plane records created by
`runstead decide <task-id> <action-id> approved|rejected` (or the equivalent
state API). They are keyed by `(task_id, fingerprint)`, the repeat/loop
identity of the write proposal, so an approved write stays approved across
re-proposals of the same write (each re-proposal is a new action id with the
same fingerprint). Re-deciding the same fingerprint replaces the previous
decision and keeps one durable row. `runstead inspect` renders policy
decisions and approvals.

`runstead decide` only accepts an action of the given task that is actually
pending approval: a write action with a persisted `approval_required` decision
and no operator decision yet. Approvals for read actions, unknown actions, or
actions outside the current approval flow are rejected.

### Approval is a control-plane pause, not a protocol correction

When a write proposal requires approval, the run pauses with the typed outcome
`approval_required`:

- the write does **not** execute;
- no correction budget is consumed;
- no further provider attempt is made to wait for the operator;
- the task is **not** finalized as completed or failed: it stays durably
  resumable (status `running`) with a `task_approval_required` journal event;
- the CLI output reports the task id and the pending action id needed for
  `runstead decide <task-id> <action-id> approved|rejected`;
- `runstead inspect` shows a "Pending approvals:" section listing every write
  action still awaiting an operator decision.

After the operator decides:

- `approved`: a normal `runstead resume` re-proposes the write (same
  fingerprint, new action id), the persisted approval unlocks it, and it
  executes;
- `rejected`: a normal `runstead resume` preserves the rejection; the
  re-proposed write is denied and never executes.

A task with a pending write approval can **never** be finalized as completed:
the loop pauses instead, and the state layer refuses the completed transition
(`ErrPendingApprovals`) as defense in depth.

### The effective write policy is durable

The effective write policy (the canonical `tool=mode` specification) is part
of the authoritative task configuration persisted with the task
(`config_json.write_policy`, sanitized and visible via `runstead inspect`).
Resume always continues under the persisted policy:

- with no `--write-policy` override, the persisted policy is used;
- a `--write-policy` override that diverges from the persisted policy is
  rejected fail-closed before any recovery or execution side effect (there is
  no external authority that could justify silently widening the policy a
  task started under);
- a legacy task without a persisted policy falls back to the fail-closed
  default (`approval_required`), never to a permissive gap.

The policy seam is deliberately narrow so later milestones (#26 and beyond)
can extend it without coupling: a `Policy` implementation returns a typed
`Outcome` for a typed `Request`, and the caller persists the decision.

## Evidence

Every changed file produces structured write evidence
(`tools.WriteEvidence`) that is persisted in `tool_results` and returned to
the model:

- normalized relative path;
- `before_hash` and `after_hash` (sha256, or `absent` for a new file);
- `byte_count`;
- `change_kind` (`created`, `modified`, `unchanged`);
- `outcome` (`success`, `noop`, `denied`, `invalid_arguments`,
  `path_violation`, `stale`, `invalid_patch`, `failed`, `uncertain`,
  `reconciliation_required`, `human_review_required`);
- bounded structured diff evidence (`diff`, `diff_bytes`, `diff_truncated`);
- `action_id`, `execution_id` and `evidence_id`.

File contents are not persisted for convenience: only hashes, sizes, change
classification and bounded diffs. All persisted state passes through the
credential redaction boundary (`state.Redact`).

## Crash and reconciliation

Write attempts are ADR recovery class 2: local effects with deterministic
reconciliation. `internal/recovery` reconciles an interrupted write from
observable filesystem state, never by re-executing it:

| Current file state vs intent | Reconciliation |
| --- | --- |
| Matches the recorded precondition (before-hash/absent) | `effect_not_started`; safe to reconsider, no evidence |
| Matches the recorded expected after-state hash | `effect_completed`; observed evidence persisted as citable, action completed |
| Matches neither | `human_review_required` with the typed reason `write_effect_unreconcilable`; automatic continuation stops |

The expected after-state hash is computed at TX 1 time from the real
(unredacted) arguments and persisted with the intent
(`tool_attempts.effect_after_hash`). Recovery never derives it from redacted
persisted content and never treats an action fingerprint as proof that an
effect happened.

Alongside the after-hash, TX 1 persists a **bounded, sanitized planned diff**
(`tool_attempts.planned_diff_json`): the full-replacement diff for
`write_file`, the supplied patch for `apply_patch`, capped at the
diff-evidence limit. It is evidence of intent only and never proves the
effect happened. Only when the current filesystem state matches the recorded
expected after-state hash is it promoted to reconciled completed evidence, so
a crash-reconciled write carries the same before/after hashes, byte count,
change classification and bounded diff fields as a live write.

Reconciled write evidence is citable: a resumed run can ground a final on it,
and the registry continues the task-scoped evidence id space past it. A
verified-completed write seeds the repeat guard; a not-started write does not,
so the model may legitimately re-propose it after resume.

## Distinguishing Runstead changes from unrelated changes

Runstead never automatically commits, resets, checks out, stashes or
otherwise rewrites the user's working tree. The before-hash recorded from the
model's own observation, the after-hash recorded from the actual effect, and
the persisted bounded diff together distinguish state observed before the
write from the change produced by the specific write attempt. Git evidence
comes from the real workspace (`git_status`/`git_diff` observations), never
from model claims.

## Configuration

| Flag / env | Meaning |
| --- | --- |
| `--write-policy SPEC` / `RUNSTEAD_WRITE_POLICY` | comma-separated `tool=mode`, e.g. `write_file=allow,apply_patch=deny`; default `approval_required` for every write tool |

## Current limitations

- `apply_patch` supports a strict unified-diff subset only; it never falls
  back to fuzzy or interactive application.
- Writing through a symlink (even an internal one) is refused; there is no
  symlink-following write mode.
- Parent directories are never created implicitly.
- The effect-boundary revalidation is not a compare-and-swap: a mutation
  landing between the final revalidation and the rename is not detectable by
  the writer (stdlib offers no `openat`/`dirfd`-based rename). Runstead does
  not claim per-file CAS or atomicity beyond the rename itself.
- Approvals are per write proposal fingerprint, not per path pattern or per
  future milestone capability.
- The policy seam is static configuration plus operator approvals; there is no
  generic policy engine and no expression language.
- No generic shell or arbitrary subprocess execution exists. Subprocess
  execution belongs to #26.
