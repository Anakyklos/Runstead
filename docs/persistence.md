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
  `governor_attempt_ids`) hold the current state the runtime needs directly;
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
(`persistence_failure`). A crash after TX 1 leaves the attempt `prepared`:
durable evidence that the effect may have started, never proof that it did
not, and never authorization to re-execute blindly. A crash after the effect
but before TX 2 leaves the same `prepared` state (the ADR crash table).

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
(cooldown, circuit, task budget, request dedup) and across real subprocess
restarts (a restarted process refuses admission with the restored task
budget, never reaches the provider, and retains the rolling ledger).

The conservative accounting semantics of #29/#33 are unchanged: an uncertain
outcome keeps its charge visible, `telemetry.unsafe` is persisted, and a
restored `uncertain` attempt is never reinterpreted.

## `runstead inspect <task-id>`

After the original `runstead run` process exits, `runstead inspect <task-id>`
reopens the database and renders a stable, human-readable reconstruction:

- task identity, objective, status, typed outcome, stop reason, workspace,
  model, timestamps and summary;
- the sanitized configuration snapshot;
- the chronological event journal;
- logical actions (with fingerprint evidence);
- tool attempts (status, classification, evidence id, duration) and provider
  attempts (request id, outcome, upstream-reached, debits, receipt errors);
- provider attempt receipts (upstream evidence identity, sequence, outcome,
  trigger);
- prepared and uncertain states flagged explicitly ("the effect may have
  started; reconcile before re-execution", "the upstream may have been
  reached; never auto-retry");
- governor consumption against ceilings, cooldown and circuit state.

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
- delivery-state transport tracking (#38), write tools (#10), the process
  runner (#26), the verifier (#11) and first-party ChatGPT Web work remain
  separate milestones.

The schema keeps a compatible seam for those milestones (for example
`provider_attempt_receipts` is one-to-many and `recovery_class` is stored on
tool attempts), but no future functionality is presented as implemented.
