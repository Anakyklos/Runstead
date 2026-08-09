-- Runstead independent verification schema, migration 8 (issue #11).
--
-- Contract: docs/adr/0001-durable-execution.md and docs/verification.md.
--
--   - acceptance_plans: the operator-provided, versioned, typed acceptance
--     plan of one task. It is persisted at task start and read by the
--     verifier; the model can never invent or modify acceptance criteria.
--   - workspace_baselines: the bounded real git status/diff observed at task
--     start, so verification can distinguish pre-existing repository changes
--     from changes produced during the task "where practical".
--   - verification_attempts: one control-plane verification run (projection)
--     with its journal event. The completion gate (FinalizeTask) refuses
--     status 'completed' unless the latest verification attempt of the task
--     has decision 'passed'.
--   - verification_checks: per-check results of one attempt, individually
--     inspectable, with bounded expected/observed/reason descriptions.
--
-- Verification performs no SQLite transaction while observing the external
-- environment: the verifier runs outside any transaction and the attempt is
-- persisted after the observations complete. No ON DELETE CASCADE exists.

CREATE TABLE acceptance_plans (
    task_id     TEXT PRIMARY KEY REFERENCES tasks(task_id),
    version     INTEGER NOT NULL,
    spec_json   TEXT NOT NULL DEFAULT '{}',
    digest      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE TABLE workspace_baselines (
    task_id          TEXT PRIMARY KEY REFERENCES tasks(task_id),
    git_status_json  TEXT NOT NULL DEFAULT '',
    git_diff_json    TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);

CREATE TABLE verification_attempts (
    attempt_id  TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(task_id),
    sequence    INTEGER NOT NULL,
    decision    TEXT NOT NULL CHECK (decision IN ('passed', 'failed', 'blocked', 'uncertain')),
    report_json TEXT NOT NULL DEFAULT '{}',
    summary     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    UNIQUE (task_id, sequence)
);

CREATE TABLE verification_checks (
    task_id      TEXT NOT NULL REFERENCES tasks(task_id),
    attempt_id   TEXT NOT NULL REFERENCES verification_attempts(attempt_id),
    check_id     TEXT NOT NULL,
    type         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'blocked', 'uncertain')),
    expected     TEXT NOT NULL DEFAULT '',
    observed     TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '[]',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (task_id, attempt_id, check_id)
);
