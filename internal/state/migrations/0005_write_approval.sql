-- Runstead write-approval and planned-effect schema, migration 5 (issue #10
-- review).
--
--   - tool_attempts.planned_diff_json: the bounded, sanitized planned diff of
--     a write effect persisted with the TX 1 intent. It is evidence of intent
--     only and never proves the effect happened; recovery promotes it to
--     reconciled completed evidence only when the current filesystem state
--     matches the expected after-state hash (tool_attempts.effect_after_hash).
--     It is bounded to the diff-evidence limit at plan time, so full file
--     contents are never persisted just for evidence.
--
-- Pending write approvals need no new columns: they are derived from
-- write_policy_decisions (decision='approval_required') joined with actions
-- and approvals (the fingerprint has no operator decision yet).

ALTER TABLE tool_attempts ADD COLUMN planned_diff_json TEXT NOT NULL DEFAULT '';
