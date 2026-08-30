# Persistence (issue #8)

This document describes the implemented durable-state architecture: the SQLite
driver decision and its evidence, the schema and migrations, the transactional
journal + operational projection model, the external-effect transaction
ordering, the persisted governor state, `runstead inspect`, database location
and lifecycle, security/redaction, and the explicit boundary between
persistence (#8) and recovery/resume (#9).

The canonical contract is
[`docs/adr/0001-durable-execution.md`](adr/0001-durable-execution.md); this
document describes the implemented reality against that contract.

## Architecture summary

Runstead persists task state as **transactional journal + operational
projections** in one SQLite database:

- operational tables (`tasks`, `actions`, `tool_attempts`, `tool_results`,
  `provider_attempts`, `provider_attempt_receipts`, `governor_state`,
  `governor_ledger`, `governor_task_states`, `governor_request_records`,
  `governor_attempt_ids`, `governor_rate_events`, `work_units`,
  `work_unit_dependencies`) hold the current state the
  runtime needs directly;
- an append-only `events` journal records meaningful transitions with
  deterministic task-scoped ordering `UNIQUE(task_id, sequence)`;
- every projection change and its corresponding event commit in the **same
  SQLite transaction**;
- state reconstruction is reading persisted records plus observable
  environment evidence; Runstead never re-executes historical model/tool
  effects to rebuild state.

There is no Event Sourcing framework, no generic repository/unit-of-work
layer, and no JSON/JSONL snapshot mechanics: the JCode line-salvage,
glued-JSON recovery, checkpoint-after-corrupt-line and forensic-copy rules
were explicitly rejected by the ADR and are not implemented.

## SQLite driver decision

**Selected: `modernc.org/sqlite` v1.34.5** (pure Go translation of SQLite,
BSD-3-Clause). It is the only dependency added to the previously
dependency-free module.

A focused implementation spike compared the realistic candidates under
Runstead's actual constraints (Go 1.22.2, Linux, deterministic tests, race
tests, crash windows, migrations, distribution). Evidence measured on the
development machine (12th gen i5, Ubuntu 24.04):

| Criterion | `mattn/go-sqlite3` v1.14.24 | `modernc.org/sqlite` v1.34.5 |
| --- | --- | --- |
| Go 1.22.2 build | ok | ok (requires go >= 1.21) |
| CGO requirement | required (gcc, CGO_ENABLED=1) | none, pure Go |
| Binary size (isolated build) | 6.2 MB | 8.8 MB |
| Cold build (fresh GOCACHE) | 37 s, 46 MB cache | 11 s, 88 MB cache |
| Bundled SQLite | 3.46.1 | 3.46.0 |
| WAL, busy_timeout, rollback, FK | verified | verified |
| `-race` concurrent writers | pass | pass |
| Locked-writer behavior | waits busy timeout, fails cleanly | identical (same engine) |
| Missing-directory error | clear ("no such file or directory") | confusing ("out of memory (14)"; the store creates the directory first, so this is not reachable through the runtime) |
| License | MIT | BSD-3-Clause |
| Maintenance | very active, huge community | active; release cadence tracks SQLite with a lag |

Rejected candidates:

- `mattn/go-sqlite3` (CGO): functionally identical engine semantics, but it
  makes the whole project require a C toolchain at build time, breaks
  `CGO_ENABLED=0` builds, complicates cross-compilation and contradicts the
  documented "single native executable" distribution goal
  (README, docs/architecture.md). The CGO requirement is the decisive
  negative for a project whose dependency policy is "practical minimum".
- `ncruces/go-sqlite3` (WASM-based pure Go): serializes all database access
  through a single WASM execution context, an unnecessary bottleneck and an
  unusual failure model for a runtime that reads from multiple goroutines
  while the governor writes.
- `glebarez/sqlite`: a thin wrapper over modernc adding an extra dependency
  layer for no benefit.

The version is pinned in `go.mod` (`modernc.org/sqlite v1.34.5`); newer
releases of the module require newer Go toolchains, so the pin is a deliberate
Go 1.22 compatibility constraint. The SQL is written against `database/sql`
and the schema stores timestamps as RFC 3339 UTC text, so a future driver
change would not require schema changes.

## SQLite operational policy

Applied on the store's single connection at open (see
`internal/state/store.go`):

| Setting | Value | Rationale |
| --- | --- | --- |
| `journal_mode` | `WAL` | readers never block the single writer; a process crash between commits cannot corrupt the main database file |
| `synchronous` | `NORMAL` | in WAL mode every committed transaction survives process death (the WAL is replayed on open) while the per-commit fsync is skipped; power-loss durability would require `FULL`, which a CLI whose realistic failure boundary is process interruption does not need |
| `busy_timeout` | 5000 ms | a second process (e.g. `runstead inspect` during a run) waits a bounded time for the writer instead of failing immediately |
| `foreign_keys` | `ON` | referential integrity is enforced; no `ON DELETE CASCADE` exists anywhere |
| connection count | `MaxOpenConns(1)` | Runstead is a single-writer CLI process; serializing in-process access removes driver-specific locking corner cases, while cross-process behavior is governed by WAL and busy_timeout |

Evidence: the driver spike measured WAL persistence across reopen, busy
timeout honoring (a contended writer waits the full timeout then fails with a
locked error), rollback semantics, FK enforcement and concurrent writers under
`-race`. The locking behavior is covered by repository tests
(`internal/state/locking_test.go`): WAL readers observe the pre-transaction
state during an open write transaction, and a contended writer fails cleanly
after the busy timeout instead of hanging.

## Database location and lifecycle

- Default location: `$RUNSTEAD_STATE_DIR`, else `$XDG_DATA_HOME/runstead`,
  else `~/.local/share/runstead`; the database file is `runstead.db` inside
  that directory.
- Override: `--state-dir PATH` on `runstead run` and `runstead inspect`, or
  the `RUNSTEAD_STATE_DIR` environment variable. Configuration precedence is
  flags > environment > defaults, matching the rest of the CLI.
- Permissions: the state directory is created with mode `0700` and the
  database file with mode `0600` (verified by test). The database may contain
  task objectives and sanitized repository content, so it is private to the
  invoking user.
- Failure behavior: if the directory cannot be created or the database cannot
  be opened or migrated, `run` and `inspect` fail with a clear diagnostic and
  a non-zero exit code; they never silently continue without persistence.
- Backup/export: stop any running `runstead run` process, then either copy
  the database file (the store checkpoints the WAL on close) or use the
  SQLite backup API/CLI (`sqlite3 runstead.db ".backup backup.db"`). WAL
  sidecar files (`-wal`, `-shm`) are transient; copying only `runstead.db`
  after a clean close is sufficient.
- Cleanup: there is no task-deletion command. Deliberately preserved: the
  append-only `events` journal is never cascade-deleted; any future cleanup
  path must not remove history (ADR invariant 4). Users may delete individual
  task rows with the SQLite CLI, but the journal and governor projections are
  account-scoped and should be retained while protection state matters.

## Migrations

Versioned SQL migrations are embedded in the executable
(`internal/state/migrations/*.sql`) and applied through `PRAGMA user_version`:

- fresh database creation is deterministic (verified: two fresh databases
  produce identical `sqlite_master` schemas);
- migrations are ordered and versioned explicitly (`0001_initial.sql`, ...);
  versions must be contiguous from 1, so a partial set (for example 1,3)
  cannot be applied and then make the database look newer than the migrations
  that actually produced it;
- reopening an up-to-date database is a no-op;
- each migration runs inside its own SQLite transaction and `user_version` is
  committed atomically with the statements, so a failed upgrade rolls back
  completely;
- `user_version` beyond the supported maximum fails clearly ("database schema
  version N exceeds the supported version M") instead of being silently
  repaired;
- malformed migration state (duplicate versions, broken SQL) fails with an
  explicit error.

`CREATE TABLE IF NOT EXISTS` is not used as a substitute for versioning; the
migration runner is the only schema-creation path (tests cover fresh creation,
re-run, upgrade from a previous version, rollback of a failed upgrade, newer
databases and duplicate versions).

## Identities

The ADR identity model is preserved in the schema:

- `task_id`: durable task root (`tasks.task_id`);
- `action_id`: one logical accepted envelope (`actions.action_id`, store
  allocated `action-NNNNNN`); every accepted envelope gets one row before the
  repeat guard decision, so rejected repeated proposals remain represented;
- `execution_id`: one concrete Runstead execution attempt, tool or provider
  (`tool_attempts.execution_id`, `provider_attempts.execution_id`, store
  allocated `exec-NNNNNN`); one row per concrete attempt, never a mutable
  retry counter;
- `client_request_id`: correlation identity of one admitted provider request
  (`provider_attempts.client_request_id`, `UNIQUE(task_id,
  client_request_id)`);
- `receipt_attempt_id`: upstream-owned evidence identity
  (`provider_attempt_receipts.receipt_attempt_id`); it identifies its own
  evidence row and is **never** a Runstead execution identity. A provider
  execution may map to zero or more receipt rows (the schema models
  one-to-many; M1 policy accepts one per completion);
- fingerprint: stored on `actions` only as repeat/loop evidence; fingerprint
  equality never merges actions, never reuses observations and is never an
  idempotency key;
- `idempotency_key`: none exists; nothing is invented client-side.

Evidence identifiers (`obs-NNNNNN`) are allocated per run by the tool
registry, so `tool_results` identity is task-scoped:
`PRIMARY KEY (task_id, evidence_id)`.

## External-effect transaction ordering

The ADR ordering is implemented literally. One provider attempt:

```text
TX 1  (governor.Execute, before client.Complete)
  insert provider_attempts row (status 'prepared')
  upsert governor_state + governor_ledger/task_states/request_records/attempt_ids
  append provider_attempt_prepared event
COMMIT
  client.Complete(...)            <- outside any SQLite transaction
TX 2  (governor.Execute, after permit finish)
  update provider_attempts with the classified outcome, debits, receipts
  upsert governor_state (post-finish)
  append provider_attempt_completed/failed/uncertain event
COMMIT
```

One tool attempt:

```text
TX 1  (agent loop, before registry.Execute)
  insert tool_attempts row (status 'prepared')
  append tool_attempt_prepared event
COMMIT
  registry.Execute(...)           <- outside any SQLite transaction
TX 2  (agent loop, after the observation)
  update tool_attempts (completed/failed), insert tool_results for success
  update the action status projection
  append tool_attempt_completed/failed event
COMMIT
```

No SQLite transaction ever spans an external effect. If TX 1 cannot be
committed, the provider call does not proceed and the run stops fail-closed
(`persistence_failure`). The started permit is aborted through
`Permit.CancelAfterStart`: no additional debit is recorded and the account
lane is fully released. This abort path is valid for receipt-aware permits
too, whose normal `Finish` path would refuse to run without receipts: an
upstream call that never happened must not leave the lane stuck. A crash after
TX 1 leaves the attempt `prepared`: durable evidence that the effect may have
started, never proof that it did not, and never authorization to re-execute
blindly. A crash after the effect but before TX 2 leaves the same `prepared`
state (the ADR crash table).

Crash-window coverage (subprocess tests with the deterministic `SetCrashPoint`
test seam): task created, task started, provider TX 1 committed, provider TX 2
not started, tool TX 1 committed, tool TX 2 not started, task finalize not
started, mid-effect provider kill, an interrupted write (the process dies with
a SQLite write transaction open but uncommitted), and the full-lifecycle
control. After each crash the database is reopened and the surviving state is
asserted; `runstead inspect` renders interrupted tasks. The interrupted-write
test proves the WAL replays on reopen and rolls the incomplete transaction
back, keeping the committed prefix consistent.

## Governor durability

The account governor persists its protection projection (#21) at the TX 1 and
TX 2 boundaries through `governor.Persistence`:

- rolling usage ledger (`governor_ledger`): every debit survives restart;
- cooldown state (`governor_state.cooldown_until`);
- circuit state, reason, open-until, refresh-required, rate-response events
  and last rate reset;
- the retained rate-response history (`governor_rate_events`): the #21
  threshold of three rate/capacity responses within the window is not reset
  by a restart, so a restarted process still opens the circuit on the third
  response;
- telemetry evidence that affects admission (available allowance, rate limit,
  capacity, upstream circuit, unsafe flag);
- retained request records (duplicate detection) and receipt attempt IDs
  (replay detection);
- per-task attempt/retry usage and the next-attempt counter.

`Options.Restore` feeds the snapshot back into `governor.New` at startup.
Restore preserves the governor invariants: usage is additive with the
restored ledger, expired circuit windows normalize to closed, and retention
pruning applies. In-flight and queue state are process-local and deliberately
not persisted. Tests prove protection survives restart both in-process
(cooldown, circuit, task budget, request dedup, rate-response threshold) and
across real subprocess restarts (a restarted process refuses admission with
the restored task budget, never reaches the provider, and retains the rolling
ledger). A dedicated test proves two rate/capacity responses before a restart
plus a third after the restart open the circuit to `human_review_required`.

The conservative accounting semantics of #29/#33 are unchanged: an uncertain
outcome keeps its charge visible, `telemetry.unsafe` is persisted, and a
restored `uncertain` attempt is never reinterpreted.

### Provider delivery state (#38)

`delivery_state` is transport evidence orthogonal to the provider-attempt
lifecycle. It does not mean task completion and it never replaces authoritative
attempt receipts. `sent_unconfirmed` is fail-closed: it is treated as an
uncertain effect and is not automatically replayed. `client_request_id` is the
Runstead correlation identity, not a promise that OmniRoute or the upstream
model honors an idempotency key. A `completed` delivery may still classify as a
provider failure when the HTTP result, response content or envelope is invalid.
An empty value means delivery was not observed before TX2 and is never converted
to a stronger state during recovery. No exactly-once execution guarantee is
introduced.

## `runstead inspect <task-id>`

After the original `runstead run` process exits, `runstead inspect <task-id>`
reopens the database and renders a stable, human-readable reconstruction:

- task identity, objective, status, typed outcome, stop reason, workspace,
  model, timestamps and summary;
- the sanitized configuration snapshot;
- the chronological event journal;
- logical actions (with fingerprint evidence);
- tool attempts (status, classification, evidence id, duration) and provider
  attempts (request id, delivery state, outcome, upstream-reached, debits,
  receipt errors);
- provider attempt receipts (upstream evidence identity, sequence, outcome,
  trigger);
- prepared and uncertain states flagged explicitly ("the effect may have
  started; reconcile before re-execution", "the upstream may have been
  reached; never auto-retry");
- governor consumption against ceilings, cooldown and circuit state: the
  windowed rolling counts exclude ledger entries outside the window, and the
  task-attempt figure is the inspected task's own attempt usage (from
  `governor_task_states.attempts`), never the number of retained tasks.

Output is deterministic (two renders of the same task are byte-identical),
ordered, and never dumps raw SQLite rows or opaque JSON blobs. Exit codes:
0 rendered, 1 task not found, 2 usage, 3 state database unavailable. The task
id is printed to stderr by `runstead run` (`task: <id>`) so the id is
available after a run.

## Security and redaction

The database never contains provider API keys, ChatGPT cookies/session
credentials, bearer/access tokens, authorization headers, full environment
dumps, raw private provider response bodies, or secrets embedded in error
strings:

- provider prompts/transcripts are never persisted (verified by test);
- raw provider response bodies are never persisted; only the sanitized
  `provider.ResponseMetadata` and classification codes cross the boundary;
- error strings are never persisted raw: only typed classifications
  (`OutcomeClass`, `tools.FailureCode`, receipt validation codes);
- `internal/state/redact.go` extends the credential-redaction semantics
  already exercised by the protocol experiment corpus (`experiments/protocol/
  run.sh`) into the persistence layer: Bearer tokens, `sk-...` keys,
  credential key/value pairs, and ChatGPT-Web-style session cookies are
  replaced with `<redacted>` before any text is stored (task objectives,
  action arguments, observation data/metadata, event payloads, stop
  reasons);
- the store is created with private permissions (`0700` directory, `0600`
  file).

Tests inject credential-shaped values into objectives, action arguments,
observation data, summaries and stop reasons, then read the raw database file
bytes and query individual columns to prove the secrets are absent and the
redaction marker is present.

## Persistence (#8) vs recovery/resume (#9)

Issue #8 implements the schema, journal, attempt records, governor durability
and `runstead inspect`. Issue #9 (resume/recovery) is explicitly **not**
implemented:

- there is no `runstead resume` workflow;
- there is no context reconstruction for a new provider conversation;
- there is no automatic reconciliation engine;
- prepared/uncertain attempts are persisted and flagged, but nothing
  automatically re-executes, reinterprets or reconciles them;
- first-party ChatGPT Web work remains a separate milestone; delivery-state
  transport tracking (#38), write tools (#10), the bounded process runner
  (#26) and the independent verifier (#11) are implemented (see
  [writes.md](writes.md), [process-runner.md](process-runner.md) and
  [verification.md](verification.md)), including the approval pause (task
  stays resumable with pending approvals derived from
  `write_policy_decisions` + `approvals`), the TX 1 evidence
  (`tool_attempts.planned_diff_json` migration 0005 for writes,
  `tool_attempts.process_intent_json` migration 0006 for process recipes,
  `actions.recipe_fingerprint` migration 0007 for the digest-bound recipe
  approval identity, and `config_json.recipe_catalog_digest` for the durable
  catalog digest that resume compares against the re-supplied catalog), and
  the verification schema (migrations 0008-0009: `acceptance_plans`,
  `workspace_baselines` with its git truncation flags,
  `verification_attempts`, `verification_checks`).

Work Units (issue #106) add two migration-0014 tables (`work_units` with the
typed lifecycle status check and `work_unit_dependencies` as a table-valued
DAG edge set) and `work_unit_id` provenance columns (default `''`) on
`actions`, `tool_attempts`, `provider_attempts` and `verification_attempts`,
so every row a unit produces stays attributable to the unit without
re-execution.

The schema keeps a compatible seam for those milestones (for example
`provider_attempt_receipts` is one-to-many and `recovery_class` is stored on
tool attempts), but no future functionality is presented as implemented.

## Resume and recovery (issue #9)

`runstead resume <task-id>` reconstructs an interrupted task from durable
state and continues it through the normal governed agent loop. Recovery is
defined as:

```text
persisted state
+ environment reconciliation
+ bounded context reconstruction
+ new governed execution attempts
```

It is **not** replaying the old workflow until the same history happens again:
historical provider calls are never re-issued to reproduce the old conversation
(provider response text is not persisted at all), completed tool effects are
never executed again merely because the task resumes, uncertain attempts stay
conservatively accounted, and the persisted governor protection state is
restored unchanged.

### Pipeline

`internal/recovery.Resume` implements the explicit recovery pipeline:

1. **load task** — `state.LoadRecoverySnapshot` reads the task root, logical
   actions (with fingerprints and recorded workspace signatures), tool
   attempts, provider attempts and citable evidence from SQLite;
2. **classify existing attempts** — completed/failed attempts are terminal
   progress; class-1 prepared attempts are interrupted replay-safe
   observations; class-2 write attempts (`write_file`/`apply_patch`) are
   reconciled from observable filesystem state; any other non-terminal tool
   attempt (recovery class 3-4) is unreconcilable; provider attempts that may
   have reached upstream are treated conservatively;
3. **reconcile interrupted attempts** — each transition commits atomically
   with its journal event (`tool_attempt_reconciled`,
   `provider_attempt_reconciled`) through `state.ReconcileToolAttempt` /
   `state.ReconcileWriteAttempt` / `state.ReconcileProviderAttempt`. A class-1
   prepared observation is reconciled as `replay_safe_observation`; a class-2
   write attempt is classified by `tools.ReconcileWrite` against the current
   filesystem state using the persisted precondition and expected after-state
   hash (`tool_attempts.effect_after_hash`, migration 0004): still matching
   the precondition means `write_effect_not_started` (no evidence), matching
   the expected after-state means `write_effect_completed` (observed evidence
   persisted as citable via `state.ReconcileWriteAttempt`, action completed;
   the evidence carries the bounded planned diff persisted at TX 1 —
   `tool_attempts.planned_diff_json`, migration 0005 — alongside the
   before/after hashes), and matching neither means
   `write_effect_unreconcilable` which stops
   automatic continuation with `human_review_required`. A provider request that may have
   reached upstream is reconciled as `upstream_may_have_been_reached` with
   `uncertain = 1` and `attempt_debited = 1`. A plain attempt was already
   debited in the governor ledger at TX 1 (`Start`); a receipt-aware attempt
   interrupted before TX 2 was **not** (StartReceiptAware defers all debits to
   the receipt finish path), so recovery applies the #29 conservative debit to
   the persisted governor protection projection in the same transaction —
   task attempt count +1, one rolling ledger event, telemetry unsafe —
   mirroring `finishReceiptFailureLocked`. The ledger event is dated with the
   **original permit start** (`provider_attempts.prepared_at`), never the
   resume time, so the 10m/1h/3h windows represent when the upstream attempt
   possibly happened; `lastStart` moves to that timestamp and
   `telemetry.available` is decremented when known, exactly like the governor's
   own `p.startedAt` fallback-to-now. A receipt-aware attempt persisted as
   `uncertain` was already debited at TX 2 and is never re-debited.
   Reconciling an already terminal attempt fails with `ErrNotReconcilable`, so
   a second resume never rewrites history;
4. **decide whether automatic continuation is safe** — an unreconcilable
   effect stops with the typed `human_review_required` outcome
   (`state.MarkHumanReviewRequired` persists the task status, stop reason and
   the structured attempt list); the restored governor may also report that
   continuation is blocked by account protection (circuit, cooldown, rolling
   or task budget), in which case `recovery_blocked` is journaled and the task
   stays pending;
5. **reconstruct bounded model context** — `internal/recovery.BuildContext`
   renders a deterministic, budgeted summary (see below);
6. **establish recovery boundary** — `recovery_started` (with the persisted
   `resume_count` projection), `recovery_context_reconstructed` and
   `recovery_continued` journal events mark where historical execution ends
   and new governed execution begins;
7. **continue through the normal agent loop** — the loop receives an
   `agent.RecoverySeed` (counters, evidence, repeat guard, context) and runs
   with the same governor admission, provider accounting, protocol parsing,
   registry validation, grounding and persistence as a fresh `run`. There is
   no separate "resume executor".

### Identities preserved

The resume path never collapses the ADR identities:

- `task_id` — one durable task;
- `action_id` — one logical proposal; every accepted envelope is a distinct
  action, so a re-proposed envelope after resume creates a new action row even
  when the fingerprint matches;
- `execution_id` — one concrete attempt; a re-execution is a new attempt, the
  reconciled historical attempt stays terminal;
- `client_request_id` — the interrupted request id is never re-issued; new
  turns continue the `<task-id>-NNNN` sequence from the persisted attempt
  count, so `UNIQUE(task_id, client_request_id)` holds;
- `receipt_attempt_id` — upstream evidence identities remain upstream-owned;
- fingerprint — recorded per action together with the workspace signature at
  acceptance time (`actions.workspace_signature`, migration 0003). Resume
  seeds the repeat guard from executed actions only, so an identical proposal
  is rejected only while the workspace signature is unchanged. After an
  external workspace change the same action may run again and produce fresh
  evidence: **fingerprint equality is loop/repetition evidence, never an
  idempotency or result-reuse key**.

### Recovery classes and reconciliation rules

The ADR recovery classes are represented by `tool_attempts.recovery_class`
(class 1 for the read-only tools, class 2 for `write_file`/`apply_patch`) and
the typed reconciliation reasons stored in `tool_attempts.recovery_reason` /
`provider_attempts.recovery_reason` (migration 0003) and rendered by
`runstead inspect`.

| Persisted state | Classification | Recovery decision |
| --- | --- | --- |
| tool attempt `completed` | verified progress | eligible for context reconstruction, evidence seeding and guard seeding; never re-executed |
| tool attempt `failed` | deterministic failure | retained in context as an unresolved failure; guard-seeded like a fresh run |
| tool attempt `prepared` (class 1) | interrupted replay-safe observation | reconciled `replay_safe_observation`; a re-proposal executes as a NEW attempt with fresh evidence; no guard seed, no evidence |
| tool attempt `prepared`/non-terminal (class 2 write) | interrupted write | reconciled from filesystem state: `write_effect_not_started`, `write_effect_completed` (citable evidence) or `write_effect_unreconcilable` → `human_review_required`; never blindly repeated |
| tool attempt non-terminal (class 3-4) | unreconcilable effect | `human_review_required`; no automatic continuation |
| provider attempt `prepared`/`running`/`uncertain` | may have reached upstream | reconciled `upstream_may_have_been_reached`; `uncertain = 1`, `attempt_debited = 1`; the request is never re-issued; a receipt-aware attempt interrupted before TX 2 also applies the conservative debit to the persisted governor projection (unsafe telemetry), so the restored governor blocks continuation fail-closed |
| provider attempt `completed`/`failed` | terminal classified outcome | no action; counted in the resumed turn budget |
| provider attempt `human_review_required` | already-required review | task stops with the unresolved human-review outcome |

A crash during recovery itself (for example after a reconciliation commit)
leaves the journaled prefix consistent: `recovery_started` and the completed
reconciliations survive, no `recovery_continued` is written, and a second
resume completes the task without re-reconciling.

### Bounded context reconstruction

`internal/recovery.Budget` bounds the model-facing recovery context
deterministically (default 32 KiB total, 8 observations with content, 4 KiB per
observation). The policy retains what continuation requires and drops
irrelevant historic noise by construction:

- the original objective, workspace and consumed-budget constraints always
  survive;
- every citable evidence ID is always listed (never silently discard evidence
  required to justify completion);
- unresolved failures and uncertain attempts are always listed (bounded);
- observation content is rendered newest-first up to the per-observation and
  per-count caps; a hard truncation carries an explicit marker.

The reconstructed context is built exclusively from already-sanitized
persisted state and re-passes `state.Redact`. It never contains hidden
provider reasoning or historical assistant turns. The resumed registry
continues the task-scoped evidence ID space from the persisted maximum
(`tools.Options.NextEvidenceSequence`), so new observations never collide with
persisted `(task_id, evidence_id)` rows.

The #12 failure-guard counters are part of the resumed loop budgets: the
recovery pipeline recomputes the trailing consecutive tool/process failure
streak and the trailing failed-verification streak from the persisted attempt
and verification history and seeds them into `agent.RecoverySeed`, so a
resumed run continues the guards instead of silently resetting them (see
[coding-loop.md](coding-loop.md)).

### Governor restoration

The resumed run rebuilds the account governor from the persisted protection
projection (`store.GovernorState` → `governor.Options.Restore`) before the
pipeline runs. Restart/resume never resets account protection: task
provider-attempt usage, rolling usage, cooldown, circuit state, retained
request/attempt records and rate-response history all survive. When the
restored governor reports that continuation is currently blocked
(`recovery.GovernorBlocks`: circuit not closed, active cooldown, exhausted
rolling or task budget), resume journals `recovery_blocked` and exits with the
typed `governor_blocked` code, leaving the task pending instead of finalizing
it on account-protection grounds. The resumed loop remains the authoritative
admission path for anything the pre-check does not cover.

### `runstead resume` CLI

```text
runstead resume <task-id> [flags]
  --scripted FILE            provider input for the new conversation (required)
  --state-dir PATH           state directory (RUNSTEAD_STATE_DIR)
  --log-level LEVEL          log level (RUNSTEAD_LOG_LEVEL, default info)
  --min-start-interval DURATION  governor pacing override
```

The task workspace is part of its durable identity: resume always operates on
the persisted workspace and there is deliberately no `--workspace` override,
because continuing the same task in a different directory would let a final
ground claims on evidence produced in the original workspace while executing
tools elsewhere. Loop budgets come from the persisted configuration snapshot
(`config_json`), so a resumed run keeps the same `max_steps`, `provider_budget`,
time budget and correction/repeat limits as the interrupted run; the provider
input is supplied again at resume time (the old remote conversation is
disposable metadata, never an authority over task state). Exit codes:
0 resumed and finished, 1 task not found, 2 usage, 3 state database
unavailable, 4 task not resumable (already terminal), 5 human review required
(unresolved or newly required), 6 corrupted/incompatible state, 7 continuation
blocked by restored account protection (including a conservative-debit unsafe
state); the agent outcome codes (20+ and 130) apply when the resumed loop stops
with a typed loop outcome.

### Scope of #9

Issue #9 implements resume for the current read-only M2 runtime only. It does
not implement safe write tools (#10), the process runner (#26), arbitrary
shell, first-party ChatGPT Web work, provider routing,
account rotation, automatic fallback, generic Event Sourcing,
Temporal-style deterministic replay, a broad checkpoint framework, or the
streaming reconciliation milestone (#42). The reconciliation seam is the
`recovery_class` + typed-reason model, so future write/process tools can add
their own reconciliation contracts without changing the pipeline shape.
