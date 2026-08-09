-- Runstead workspace baseline truncation, migration 9 (issue #11 review).
--
-- The bounded git baseline captured at task start can itself be truncated
-- (large workspaces). The truncation flags are persisted with the baseline so
-- verification can record the limitation explicitly: pre-existing changes
-- outside the truncated baseline window must never be silently attributed as
-- during_task.

ALTER TABLE workspace_baselines ADD COLUMN git_status_truncated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspace_baselines ADD COLUMN git_diff_truncated INTEGER NOT NULL DEFAULT 0;
