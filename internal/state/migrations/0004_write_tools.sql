-- Runstead write-tool schema, migration 4 (issue #10).
--
-- Safe writes. Contract: docs/adr/0001-durable-execution.md and
-- docs/writes.md.
--
--   - tool_attempts.effect_after_hash: the deterministic expected after-state
--     hash of one write effect, computed at TX 1 time from the real
--     (unredacted) arguments and persisted with the intent. Recovery
--     (issue #9) reconciles an interrupted write by comparing the current
--     file hash against the recorded before-precondition and this expected
--     after-hash. It is never derived from redacted persisted content, and
--     it is never treated as proof that the effect completed (only current
--     filesystem state can prove that);
--   - write_policy_decisions: one durable, typed policy decision per write
--     action (allowed, denied, approval_required, approved, rejected). The
--     decision is control-plane state, never model prose, and is persisted
--     with its typed reason for later inspection;
--   - approvals: external control-plane approval records (approved or
--     rejected) keyed by (task_id, fingerprint), where the fingerprint is the
--     repeat/loop identity of the write proposal. Keying by fingerprint makes
--     an approval durable across re-proposals of the same write (a resumed
--     or corrected run re-proposes the write as a NEW action id but with the
--     SAME fingerprint), so the operator approves the write proposal, not a
--     transient action id. Model-authored content can never create these
--     rows; only the operator control plane can.

ALTER TABLE tool_attempts ADD COLUMN effect_after_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE write_policy_decisions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(task_id),
    action_id  TEXT NOT NULL DEFAULT '',
    tool       TEXT NOT NULL,
    decision   TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE approvals (
    approval_id TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(task_id),
    action_id   TEXT NOT NULL,
    fingerprint TEXT NOT NULL DEFAULT '',
    decision    TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    reason      TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT 'operator',
    created_at  TEXT NOT NULL,
    UNIQUE (task_id, fingerprint)
);
