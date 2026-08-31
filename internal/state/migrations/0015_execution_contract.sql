-- Runstead persistence schema, migration 15 (#54, M10).
--
-- A frozen execution contract is immutable task configuration. The JSON is
-- canonical, non-secret material and the adjacent SHA-256 binds the exact
-- bytes that were persisted. Empty values preserve compatibility for tasks
-- created before M10.
ALTER TABLE tasks ADD COLUMN execution_contract_json TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN execution_contract_hash TEXT NOT NULL DEFAULT '';
