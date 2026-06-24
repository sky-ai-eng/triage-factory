-- +goose Up
-- artifacts (TFAC-455): the single durable, run-attributed, polymorphic
-- record of everything a run produces in an external system (a pushed
-- branch, a draft/open PR, a draft/submitted review, a Jira/Linear issue,
-- a comment). One row per external object; provider + kind discriminate
-- the shape. Every capture writer in TFAC-454's Sub-epic A UPSERTs on
-- (org_id, dedup_key) so the same logical object — seen via exec and again
-- via reconciliation — is one row. Replaces the never-written run_artifacts
-- placeholder (dropped below).
--
-- team_id is denormalized from the owning run so reads scope by team
-- exactly like runs. run_id is nullable with ON DELETE SET NULL so a row
-- survives a run purge for the audit ledger. state is per-kind lifecycle
-- (domain consts, no CHECK — extensible). SQLite is N=1 (local mode) so
-- there is no RLS; the org_id default mirrors the Postgres baseline.
CREATE TABLE artifacts (
    id            TEXT PRIMARY KEY,
    run_id        TEXT REFERENCES runs(id) ON DELETE SET NULL,
    org_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    team_id       TEXT NOT NULL,
    provider      TEXT NOT NULL,
    kind          TEXT NOT NULL,
    target        TEXT NOT NULL,
    external_id   TEXT,
    url           TEXT,
    state         TEXT NOT NULL,
    dedup_key     TEXT NOT NULL,
    details_json  TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_artifacts_dedup        ON artifacts (org_id, dedup_key);
CREATE INDEX        idx_artifacts_run          ON artifacts (run_id);
CREATE INDEX        idx_artifacts_team_created ON artifacts (team_id, created_at DESC);

-- Retire the dead run_artifacts placeholder (never written, zero inserts).
-- The baseline keeps its def — this migration drops it forward.
DROP TABLE IF EXISTS run_artifacts;

-- +goose Down
SELECT 'down not supported';
