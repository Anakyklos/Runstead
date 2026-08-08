-- Runstead recovery schema, migration 3 (issue #9).
--
-- Recovery/resume support. Contract: docs/adr/0001-durable-execution.md.
--   - tasks.resume_count: projection updated atomically with the
--     recovery_started journal event so every recovery transition keeps the
--     projection+journal invariant;
--   - actions.workspace_signature: the repeat/loop evidence marker recorded
--     when the action was accepted. Resume seeds the repeat guard from it so
--     an identical proposal is rejected only while the workspace signature is
--     unchanged (fingerprint equality remains loop evidence, never an
--     idempotency key, and never a result-reuse key);
--   - tool_attempts.recovery_reason and provider_attempts.recovery_reason:
--     the typed recovery decision recorded when an interrupted attempt is
--     reconciled, rendered by `runstead inspect`.
--
-- Existing rows keep their old behavior: empty signature/reason and zero
-- resume count are the pre-recovery values.

ALTER TABLE tasks ADD COLUMN resume_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE actions ADD COLUMN workspace_signature TEXT NOT NULL DEFAULT '';

ALTER TABLE tool_attempts ADD COLUMN recovery_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE provider_attempts ADD COLUMN recovery_reason TEXT NOT NULL DEFAULT '';
