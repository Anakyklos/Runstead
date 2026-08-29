-- Runstead persistence schema, migration 14 (#106, M9 Stage A).
--
-- Durable Runstead-owned Work Units: bounded subtasks of one task, executed
-- serially through the existing governor/protocol/policy/tool/evidence/
-- verifier spine, surviving interruption, resume and provider/session
-- replacement. The durable object is the WORK, never the model/session
-- executing it.
--
--   - work_units: the versioned persisted Work Unit contract. It stores only
--     sanitized, operator-provided material: bounded objective, lifecycle
--     status with typed terminal/blocking reasons, allowed tool envelope and
--     workspace ownership scope (capability containment), the versioned
--     acceptance plan spec/digest, budget inputs, and derived evidence
--     references. No prompts, response bodies, credentials, headers or
--     private provider data ever enter this table.
--   - work_unit_dependencies: (unit -> dependency) pairs; a unit becomes
--     ready only when every dependency is 'completed'. Missing dependencies
--     and cycles fail closed before execution.
--   - work_unit_id provenance columns on the existing attempt/verification
--     tables (default '' = task-level rows, unchanged behavior): actions,
--     tool attempts, provider attempts and verification attempts tagged with
--     the owning Work Unit so evidence/provenance joins through durable
--     references instead of duplicated state.
--
-- Stage B (concurrent/multi-agent execution) is explicitly out of scope for
-- this migration: nothing here enables parallel workers.

CREATE TABLE work_units (
    work_unit_id        TEXT PRIMARY KEY,
    task_id             TEXT NOT NULL REFERENCES tasks(task_id),
    parent_work_unit_id TEXT NOT NULL DEFAULT '',
    objective           TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('created', 'ready', 'running', 'completed', 'failed', 'blocked', 'uncertain')),
    tools_json          TEXT NOT NULL DEFAULT '[]',
    workspace_scope     TEXT NOT NULL DEFAULT '',
    acceptance_plan     TEXT NOT NULL DEFAULT '{}',
    acceptance_digest   TEXT NOT NULL DEFAULT '',
    context_budget      INTEGER NOT NULL DEFAULT 0,
    provider_budget     INTEGER NOT NULL DEFAULT 0,
    step_budget         INTEGER NOT NULL DEFAULT 0,
    evidence_refs       TEXT NOT NULL DEFAULT '[]',
    failure_reason      TEXT NOT NULL DEFAULT '',
    blocking_reason     TEXT NOT NULL DEFAULT '',
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (task_id, work_unit_id)
);

CREATE TABLE work_unit_dependencies (
    work_unit_id             TEXT NOT NULL REFERENCES work_units(work_unit_id),
    depends_on_work_unit_id  TEXT NOT NULL REFERENCES work_units(work_unit_id),
    PRIMARY KEY (work_unit_id, depends_on_work_unit_id)
);

ALTER TABLE actions               ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tool_attempts         ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_attempts     ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';
ALTER TABLE verification_attempts ADD COLUMN work_unit_id TEXT NOT NULL DEFAULT '';