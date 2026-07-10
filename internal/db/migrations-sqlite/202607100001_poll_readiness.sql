-- +goose Up
-- poll_readiness (TFAC-583): DB-backed replacement for two in-memory,
-- process-local flags — the "config took effect" one-shot announce toast
-- (internal/app's old `announcer` type) and the /api/jira/stock readiness
-- gate (server.go's old jiraPollMu/jiraRestartedAt/jiraLastPollAt fields).
-- SQLite is N=1 (one process, one org via LocalDefaultOrgID), so this table
-- doesn't change local behavior — it's a storage move, not a behavior
-- change (CLAUDE.md's "Local mode / dialect impact: None" note for this
-- ticket) — but sharing one store interface + one set of SQL semantics
-- across both dialects is simpler than special-casing local mode to skip
-- persistence entirely.
--
-- One row per (org_id, source); source is 'github' or 'jira'. See
-- internal/db/migrations-postgres/202605130001_pg_baseline.sql's twin
-- table for the full field semantics (restarted_at / last_poll_at /
-- announce_pending) and internal/db/sqlite/poll_readiness.go for the
-- readiness predicate.
CREATE TABLE poll_readiness (
    org_id            TEXT NOT NULL,
    source            TEXT NOT NULL,
    restarted_at      DATETIME,
    last_poll_at      DATETIME,
    announce_pending  BOOLEAN NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, source)
);

-- +goose Down
SELECT 'down not supported';
