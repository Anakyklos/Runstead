-- Runstead process-runner schema, migration 6 (issue #26).
--
--   - tool_attempts.process_intent_json: the bounded, sanitized process intent
--     persisted with the TX 1 intent of a run_recipe attempt: the resolved
--     recipe id, executable, argv, working directory, declared capabilities,
--     the control-plane policy decision and the configured timeout/output
--     limits. It is evidence of intent only and never proves the process ran;
--     a prepared process attempt left by a crash is recovery class 4 and is
--     reconciled as uncertain/human-review-required, never blindly re-run.

ALTER TABLE tool_attempts ADD COLUMN process_intent_json TEXT NOT NULL DEFAULT '';
