-- +goose Up
-- run_memory_entities (TFAC-622): a run's memory today attaches to exactly
-- one entity via run_memory.entity_id (the task's primary entity). Runs
-- that legitimately touch several entities (a Jira ticket + the PR it
-- opens) leave no memory on the others. This join lets one run_memory row
-- reach every entity the run actually touched, tagged with the strength of
-- that association.
--
-- Keyed on run_id, not a run_memory row id — deliberate: touches are
-- recorded mid-run, before the run_memory row exists (it's written at
-- termination). run_memory's UNIQUE(run_id) makes the join through run_id
-- sound once that row lands.
--
-- role is free text (house style: external_actions.action) — see
-- domain.MemoryRole* — rather than a CHECK, so a new role never needs a
-- migration. No producers write touched/produced rows yet (sibling
-- tickets); only the backfill below and RecordEntityTouchSystem populate
-- this table as of this migration.
CREATE TABLE run_memory_entities (
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    entity_id  TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, entity_id)
);
CREATE INDEX idx_run_memory_entities_entity ON run_memory_entities (org_id, entity_id);

-- Backfill: one 'primary' join row per pre-existing run_memory row, so the
-- entity-scoped read switch (entity_links UNION arms, both dead — grep
-- finds zero writers — replaced by membership through this join) is
-- result-identical for every row that exists as of this migration.
INSERT OR IGNORE INTO run_memory_entities (org_id, run_id, entity_id, role, created_at)
SELECT org_id, run_id, entity_id, 'primary', created_at FROM run_memory;

-- +goose Down
SELECT 'down not supported';
