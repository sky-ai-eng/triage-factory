-- +goose NO TRANSACTION
-- +goose Up
-- conversation_memory gains `source` and loses `entity_id` and `human_content`.
--
-- source says who wrote agent_content: 'agent' (the conversation's own memory
-- file), 'generated' (TF composed it from the transcript) or 'none' (nothing
-- was remembered, and agent_content is NULL). App-validated at the store door,
-- which also holds the invariant that agent_content IS NULL exactly when
-- source = 'none'.
--
-- entity_id was a denormalization of the primary join row every memory already
-- has in conversation_memory_entities, and which every read goes through.
-- human_content held machine-composed artifact verdicts nothing reads.
--
-- A rebuild rather than three ALTERs: entity_id carries a FK, and SQLite's
-- ALTER TABLE ... DROP COLUMN refuses a column a constraint references. NO
-- TRANSACTION because the rebuild needs PRAGMA foreign_keys toggles, which are
-- no-ops inside a transaction; the pool is capped at one connection, so the
-- pragmas hold for every statement here. Same two ordering constraints the
-- conversations refactor documented: foreign_keys OFF before the drop (so the
-- swap is a catalog operation and not an implicit DELETE firing the children's
-- ON DELETE actions), back ON before the rename.

PRAGMA foreign_keys = off;

CREATE TABLE conversation_memory_new (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id  TEXT NOT NULL UNIQUE REFERENCES conversations(id) ON DELETE CASCADE,
    -- Denormalized from the conversation: the blueprint run this memory belongs
    -- to, or NULL for a standalone conversation. Groups every step of one
    -- blueprint run's memory together so a step reads its siblings as its
    -- handoff. Single-column FK (vs the composite the Postgres baseline uses
    -- for tenant isolation): local mode is N=1, so there is nothing to scope.
    blueprint_run_id TEXT REFERENCES blueprint_runs(id) ON DELETE SET NULL,
    agent_content    TEXT,
    source           TEXT NOT NULL,
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Every row carries across. A row that recorded no usable memory file reads as
-- 'none' — the only honest reading of it — and anything with content was the
-- agent's own narrative.
--
-- "No usable memory file" is EMPTY, not just NULL: the writer canonicalized
-- empty and whitespace-only input to NULL, but this table is older than that
-- writer's current shape and a released build is not something this migration
-- gets to assume about. A surviving '' or all-whitespace row copied as 'agent'
-- would break the invariant the new store door holds (agent_content IS NULL
-- exactly when source = 'none') in the direction that actually costs something:
-- it is non-NULL, so every entity read admits it, and the materializer writes
-- the next agent an empty file — the precise thing hiding 'none' rows exists to
-- prevent. So the copy applies the emptiness test itself and lands NULL.
--
-- The trim set is the four ASCII blanks, matching the memory_missing derivation
-- these rows are already read through (internal/db/sqlite/factory.go,
-- conversation.go) — one answer to "is this row empty", not a second spelling of
-- it. Non-empty content is copied verbatim, never trimmed: the door stores what
-- the agent wrote.
INSERT INTO conversation_memory_new (
    id, org_id, conversation_id, blueprint_run_id, agent_content, source, created_at)
SELECT
    id, org_id, conversation_id, blueprint_run_id,
    CASE WHEN NULLIF(TRIM(agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL
         THEN NULL ELSE agent_content END,
    CASE WHEN NULLIF(TRIM(agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL
         THEN 'none' ELSE 'agent' END,
    created_at
FROM conversation_memory;

DROP TABLE conversation_memory;

PRAGMA foreign_keys = on;

ALTER TABLE conversation_memory_new RENAME TO conversation_memory;

-- No conversation_id index: the UNIQUE constraint above is one. The two
-- entity-keyed indexes the old table carried went with the column — the entity
-- reads join through conversation_memory_entities, which has its own.

-- +goose Down
SELECT 'down not supported';
