-- Runstead persistence schema, migration 2 (issue #8 review).
--
-- governor_rate_events: retained circuit rate-response history (#21). Three
-- rate/capacity responses within RateResponseWindow open the circuit to
-- human_review_required; this history must survive process restart so the
-- threshold is not reset. Entries are appended in commit order; the governor
-- prunes entries older than the window when it snapshots, so persisted rows
-- are already window-bounded.
CREATE TABLE governor_rate_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL
);
