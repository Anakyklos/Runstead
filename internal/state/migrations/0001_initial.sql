-- Runstead persistence schema, migration 1 (issue #8).
--
-- Contract: docs/adr/0001-durable-execution.md
--   - operational projections (tasks, actions, attempts, evidence, governor
--     state) plus an append-only events journal;
--   - projection change and journal event commit in one SQLite transaction;
--   - no SQLite transaction ever spans an external effect;
--   - execution_id (Runstead) and receipt_attempt_id (upstream) are separate
--     ID spaces;
--   - events are never cascade-deleted; cleanup paths never remove history.
--
-- Timestamps are stored as RFC 3339 UTC text so the schema stays driver
-- agnostic and deterministic. Foreign keys are enforced; no ON DELETE CASCADE
-- exists anywhere in this schema.

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- tasks: one durable user task. status is a convenience projection; the full
-- history lives in events. objective is the user-provided task prompt.
CREATE TABLE tasks (
    task_id     TEXT PRIMARY KEY,
    objective   TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('planned', 'running', 'completed', 'failed', 'canceled', 'human_review_required')),
    outcome     TEXT NOT NULL DEFAULT '',
    stop_reason TEXT NOT NULL DEFAULT '',
    workspace   TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    config_json TEXT NOT NULL DEFAULT '{}',
    summary     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT ''
);

-- actions: one logical model proposal per accepted envelope, including
-- proposals the repeat guard later rejects. fingerprint is repeat/loop
-- evidence only, never an idempotency key.
CREATE TABLE actions (
    action_id       TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks(task_id),
    action_sequence INTEGER NOT NULL,
    tool            TEXT NOT NULL,
    arguments_json  TEXT NOT NULL DEFAULT '{}',
    fingerprint     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('planned', 'prepared', 'rejected', 'completed', 'failed', 'human_review_required')),
    created_at      TEXT NOT NULL,
    UNIQUE (task_id, action_sequence)
);

-- tool_attempts: one concrete Runstead execution per actual tool attempt.
-- status follows the ADR tool-attempt lifecycle; the runtime today persists
-- 'prepared' (TX 1) and 'completed'/'failed' (TX 2). A row left 'prepared' is
-- ambiguous after a crash and must be reconciled, never re-executed blindly.
CREATE TABLE tool_attempts (
    execution_id    TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks(task_id),
    action_id       TEXT NOT NULL REFERENCES actions(action_id),
    tool            TEXT NOT NULL,
    arguments_json  TEXT NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL CHECK (status IN ('planned', 'prepared', 'running', 'observed', 'verified', 'completed', 'failed', 'uncertain', 'verification_failed', 'reconciled', 'canceled', 'human_review_required')),
    classification  TEXT NOT NULL DEFAULT '',
    recovery_class  INTEGER NOT NULL DEFAULT 1 CHECK (recovery_class BETWEEN 1 AND 4),
    evidence_id     TEXT NOT NULL DEFAULT '',
    duration_ns     INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    prepared_at     TEXT NOT NULL DEFAULT '',
    completed_at    TEXT NOT NULL DEFAULT ''
);

-- tool_results: persisted observation evidence. Evidence identifiers
-- (obs-NNNNNN) are allocated per run by the tool registry, so identity is
-- task-scoped: (task_id, evidence_id). evidence_id is never model-chosen.
-- Failed observations carry no row here (no citable evidence). untrusted
-- preserves the tool metadata marker.
CREATE TABLE tool_results (
    evidence_id   TEXT NOT NULL,
    task_id       TEXT NOT NULL REFERENCES tasks(task_id),
    execution_id  TEXT NOT NULL REFERENCES tool_attempts(execution_id),
    success       INTEGER NOT NULL CHECK (success IN (0, 1)),
    untrusted     INTEGER NOT NULL DEFAULT 1 CHECK (untrusted IN (0, 1)),
    data_json     TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    PRIMARY KEY (task_id, evidence_id)
);

-- provider_attempts: one governed provider execution per admitted request.
-- The execution_id is Runstead-owned; client_request_id correlates one
-- admitted request. An 'uncertain' outcome must never be silently
-- reinterpreted on restart; a row left 'prepared' may have reached upstream.
CREATE TABLE provider_attempts (
    execution_id       TEXT PRIMARY KEY,
    task_id            TEXT NOT NULL REFERENCES tasks(task_id),
    client_request_id  TEXT NOT NULL,
    provider           TEXT NOT NULL DEFAULT '',
    model_pool         TEXT NOT NULL DEFAULT '',
    model              TEXT NOT NULL DEFAULT '',
    attempt_sequence   INTEGER NOT NULL DEFAULT 0,
    receipt_aware      INTEGER NOT NULL DEFAULT 0 CHECK (receipt_aware IN (0, 1)),
    status             TEXT NOT NULL CHECK (status IN ('planned', 'prepared', 'running', 'completed', 'failed', 'uncertain', 'reconciled', 'canceled', 'human_review_required')),
    outcome            TEXT NOT NULL DEFAULT '',
    upstream_reached   INTEGER NOT NULL DEFAULT 0 CHECK (upstream_reached IN (0, 1)),
    uncertain          INTEGER NOT NULL DEFAULT 0 CHECK (uncertain IN (0, 1)),
    attempt_debited    INTEGER NOT NULL DEFAULT 0,
    selected_backoff_ns INTEGER NOT NULL DEFAULT 0,
    error_class        TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    prepared_at        TEXT NOT NULL DEFAULT '',
    completed_at       TEXT NOT NULL DEFAULT '',
    UNIQUE (task_id, client_request_id)
);

-- provider_attempt_receipts: sanitized authoritative receipt evidence (#29).
-- receipt_attempt_id is the upstream-owned identity of this evidence row and
-- is never the identity of a Runstead execution. One provider execution may
-- map to zero or more receipts.
CREATE TABLE provider_attempt_receipts (
    receipt_attempt_id TEXT PRIMARY KEY,
    task_id            TEXT NOT NULL REFERENCES tasks(task_id),
    execution_id       TEXT NOT NULL REFERENCES provider_attempts(execution_id),
    schema_version     INTEGER NOT NULL,
    client_request_id  TEXT NOT NULL,
    sequence           INTEGER NOT NULL,
    provider           TEXT NOT NULL DEFAULT '',
    model              TEXT NOT NULL DEFAULT '',
    account_lane_hash  TEXT NOT NULL DEFAULT '',
    started_at         TEXT NOT NULL,
    completed_at       TEXT NOT NULL,
    outcome            TEXT NOT NULL,
    trigger            TEXT NOT NULL,
    upstream_reached   INTEGER NOT NULL CHECK (upstream_reached IN (0, 1)),
    UNIQUE (execution_id, receipt_attempt_id)
);

-- events: append-only journal. task-scoped deterministic ordering. Payload
-- hashes are integrity aids, not identity. No cleanup path may cascade into
-- this table.
CREATE TABLE events (
    task_id     TEXT NOT NULL REFERENCES tasks(task_id),
    sequence    INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (task_id, sequence)
);

-- governor_state: single operational projection row for the account
-- protection state (#21). Restarting the process must not reset protection.
CREATE TABLE governor_state (
    id                          INTEGER PRIMARY KEY CHECK (id = 1),
    account_policy_id           TEXT NOT NULL,
    provider_id                 TEXT NOT NULL,
    model_pool                  TEXT NOT NULL,
    model                       TEXT NOT NULL DEFAULT '',
    allowance_profile           TEXT NOT NULL,
    next_attempt                INTEGER NOT NULL,
    last_start                  TEXT NOT NULL DEFAULT '',
    cooldown_until              TEXT NOT NULL DEFAULT '',
    circuit_state               TEXT NOT NULL,
    circuit_reason              TEXT NOT NULL DEFAULT '',
    circuit_open_until          TEXT NOT NULL DEFAULT '',
    circuit_refresh_required    INTEGER NOT NULL DEFAULT 0 CHECK (circuit_refresh_required IN (0, 1)),
    circuit_last_rate_reset     TEXT NOT NULL DEFAULT '',
    telemetry_available         INTEGER,
    telemetry_reset_at          TEXT NOT NULL DEFAULT '',
    telemetry_cooldown_until    TEXT NOT NULL DEFAULT '',
    telemetry_rate_limited      INTEGER NOT NULL DEFAULT 0 CHECK (telemetry_rate_limited IN (0, 1)),
    telemetry_capacity_exhausted INTEGER NOT NULL DEFAULT 0 CHECK (telemetry_capacity_exhausted IN (0, 1)),
    telemetry_upstream_circuit  TEXT NOT NULL DEFAULT 'unknown',
    telemetry_unsafe            INTEGER NOT NULL DEFAULT 0 CHECK (telemetry_unsafe IN (0, 1)),
    rolling_3h_ceiling          INTEGER NOT NULL DEFAULT 0,
    rolling_1h_ceiling          INTEGER NOT NULL DEFAULT 0,
    rolling_10m_ceiling         INTEGER NOT NULL DEFAULT 0,
    task_budget_ceiling         INTEGER NOT NULL DEFAULT 0,
    retry_budget_ceiling        INTEGER NOT NULL DEFAULT 0,
    updated_at                  TEXT NOT NULL
);

-- governor_ledger: rolling usage ledger events required by #21. Entries are
-- appended in commit order; the governor counts them per window on restore.
CREATE TABLE governor_ledger (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT ''
);

-- governor_task_states: per-task attempt/retry usage retained by the
-- governor. Not task history; it is the governor's protection projection.
CREATE TABLE governor_task_states (
    task_id      TEXT PRIMARY KEY,
    attempts     INTEGER NOT NULL,
    retries      INTEGER NOT NULL,
    last_touched TEXT NOT NULL
);

-- governor_request_records: retained client request ids for duplicate
-- detection across restart.
CREATE TABLE governor_request_records (
    request_id   TEXT PRIMARY KEY,
    state        TEXT NOT NULL,
    completed_at TEXT NOT NULL DEFAULT ''
);

-- governor_attempt_ids: receipt attempt ids already reconciled, for replay
-- detection across restart.
CREATE TABLE governor_attempt_ids (
    attempt_id TEXT PRIMARY KEY,
    seen_at    TEXT NOT NULL
);
