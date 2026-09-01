-- 0016_improvement_proposals.sql
--
-- Issue #55: evidence-backed improvement proposals. These tables hold
-- NON-AUTHORITATIVE control-plane information: proposals, operator review
-- decisions, versioned revisions of declarative targets and later objective
-- validation records. They are deliberately SEPARATE from the authoritative
-- task/effect/evidence state (tasks, actions, tool_attempts, tool_results,
-- provider_attempts, approvals, verification_*): nothing in this migration
-- can express policy, governor, verifier, approval or execution authority,
-- and no execution path reads these tables.
--
-- The only cross-table references are PROVENANCE references to durable
-- evidence (tool_results) and work units, so a proposal can never invent
-- evidence or completion.

CREATE TABLE improvement_proposals (
    proposal_id          TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL CHECK (kind IN ('composition')),
    scope_id             TEXT NOT NULL,
    title                TEXT NOT NULL,
    target_id            TEXT NOT NULL,
    target_base_version  TEXT NOT NULL DEFAULT '',
    source_task_ids      TEXT NOT NULL DEFAULT '[]',
    source_work_unit_ids TEXT NOT NULL DEFAULT '[]',
    proposed_change_json TEXT NOT NULL,
    rationale            TEXT NOT NULL DEFAULT '',
    expected_benefit     TEXT NOT NULL DEFAULT '',
    invariants_touched   TEXT NOT NULL DEFAULT '[]',
    validation_plan      TEXT NOT NULL DEFAULT '[]',
    lifecycle_status     TEXT NOT NULL CHECK (lifecycle_status IN ('pending', 'approved', 'rejected', 'applied', 'validated', 'rolled_back')),
    review_decision      TEXT NOT NULL DEFAULT '',
    review_reason        TEXT NOT NULL DEFAULT '',
    reviewed_by          TEXT NOT NULL DEFAULT '',
    decided_at           TEXT NOT NULL DEFAULT '',
    version_id           TEXT NOT NULL DEFAULT '',
    artifact_path        TEXT NOT NULL DEFAULT '',
    rolled_back_to       TEXT NOT NULL DEFAULT '',
    rolled_back_at       TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

-- Provenance join: every referenced evidence row must exist in
-- tool_results (real durable observation). A proposal can never cite
-- evidence that does not exist.
CREATE TABLE improvement_proposal_evidence (
    proposal_id TEXT NOT NULL REFERENCES improvement_proposals(proposal_id),
    task_id     TEXT NOT NULL REFERENCES tasks(task_id),
    evidence_id TEXT NOT NULL,
    PRIMARY KEY (proposal_id, task_id, evidence_id),
    FOREIGN KEY (task_id, evidence_id) REFERENCES tool_results(task_id, evidence_id)
);

-- Versioned revisions of an accepted proposal target. The canonical
-- artifact bytes are stored so rollback restores the previous revision
-- deterministically without interpreting any narrative.
CREATE TABLE improvement_versions (
    version_id      TEXT PRIMARY KEY,
    proposal_id     TEXT NOT NULL UNIQUE REFERENCES improvement_proposals(proposal_id),
    target_id       TEXT NOT NULL,
    revision        INTEGER NOT NULL CHECK (revision >= 1),
    base_version_id TEXT NOT NULL DEFAULT '',
    artifact_digest TEXT NOT NULL,
    artifact_json   TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    UNIQUE (target_id, revision)
);

-- Later objective validation records. outcome is an operator-attested
-- classification; every record must reference evidence rows that exist.
-- A model narrative alone can never create a validation record.
CREATE TABLE improvement_validations (
    validation_id TEXT PRIMARY KEY,
    proposal_id   TEXT NOT NULL REFERENCES improvement_proposals(proposal_id),
    version_id    TEXT NOT NULL REFERENCES improvement_versions(version_id),
    outcome       TEXT NOT NULL CHECK (outcome IN ('positive', 'negative', 'uncertain')),
    evidence      TEXT NOT NULL DEFAULT '[]',
    notes         TEXT NOT NULL DEFAULT '',
    observed_at   TEXT NOT NULL,
    created_at    TEXT NOT NULL
);