-- +goose NO TRANSACTION
-- +goose Up
-- 'in_review' leaves the task status vocabulary. It meant two different
-- things and neither survived: a derived approval column the spawner flipped
-- while a run held an unresolved artifact, and a manual stage marker on a
-- user-claimed task that nothing read. Needs-you is carried by the card frame
-- and the attention order instead, and a delegation places its task
-- 'in_progress' once when its blueprint run is minted.
--
-- The rewrite is safe to run unguarded because nothing writes the value any
-- more: the two rows it can find are a stage a person set by hand and a
-- column a prior build's recompute left, and 'in_progress' is where both
-- belong — the mirror already collapsed them to the same Jira status.
UPDATE tasks SET status = 'in_progress' WHERE status = 'in_review';

-- SQLite can only narrow a CHECK by rebuilding the table. NO TRANSACTION
-- because the rebuild needs PRAGMA foreign_keys toggles, which are no-ops
-- inside a transaction; the pool is capped at one connection, so the pragmas
-- hold for every statement here. Ordering: foreign_keys OFF before the drop
-- (the seven children referencing tasks(id) must not fire their ON DELETE
-- actions on what is really a catalog swap), back ON before the rename.
--
-- No view reads tasks — llm_spend reads conversations and system_llm_runs —
-- so nothing is dropped and recreated around the swap.
PRAGMA foreign_keys = off;

CREATE TABLE tasks_new (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key            TEXT NOT NULL DEFAULT '',
    primary_event_id     TEXT NOT NULL REFERENCES events(id),
    -- The board state-machine, and the DB backstop against a typo'd status
    -- from any write path. The "queue" is a derived filter over these values
    -- rather than a sixth one, and "claimed" is the claim columns, not a
    -- status — so the enum is complete and not expected to grow.
    status               TEXT NOT NULL DEFAULT 'queued'
                            CHECK (status IN ('queued','in_progress','done','dismissed','snoozed')),
    priority_score       REAL,
    ai_summary           TEXT,
    autonomy_suitability REAL,
    priority_reasoning   TEXT,
    scoring_status       TEXT NOT NULL DEFAULT 'pending',
    severity             TEXT,
    relevance_reason     TEXT,
    source_status        TEXT,
    snooze_until         DATETIME,
    close_reason         TEXT,
    close_event_type     TEXT REFERENCES events_catalog(id),
    closed_at            DATETIME,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id               TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- team_id is the owning/attributed team and is NULLABLE: NULL = the
    -- owner is unresolved (author-centric routing couldn't pick a single
    -- team). An unowned task is still visible via task_teams and is the
    -- auto-fire gate for free (empty team disables auto-delegation); it
    -- resolves on the first human claim. The DEFAULT carries the local
    -- sentinel for callers that omit the column.
    team_id              TEXT DEFAULT '00000000-0000-0000-0000-000000000010',
    creator_user_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility           TEXT NOT NULL DEFAULT 'team'
                            CHECK (visibility IN ('private','team','org')),
    claimed_by_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    claimed_by_user_id   TEXT REFERENCES users(id)  ON DELETE SET NULL,
    -- The re-derive debt: written in the same statement that writes the
    -- scores, cleared only by a re-derive pass that evaluated the task.
    rederive_owed        BOOLEAN NOT NULL DEFAULT 0,
    CONSTRAINT tasks_claim_xor CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL),
    -- A claimed task must have a resolved owner: claiming (human or bot)
    -- consolidates the card to a single team. The unclaimed residual may
    -- carry team_id NULL (unresolved owner, visible via task_teams).
    CONSTRAINT tasks_claimed_requires_team CHECK (
        (claimed_by_user_id IS NULL AND claimed_by_agent_id IS NULL) OR team_id IS NOT NULL
    )
);

INSERT INTO tasks_new (
    id, entity_id, event_type, dedup_key, primary_event_id, status,
    priority_score, ai_summary, autonomy_suitability, priority_reasoning,
    scoring_status, severity, relevance_reason, source_status, snooze_until,
    close_reason, close_event_type, closed_at, created_at, org_id, team_id,
    creator_user_id, visibility, claimed_by_agent_id, claimed_by_user_id,
    rederive_owed
)
SELECT
    id, entity_id, event_type, dedup_key, primary_event_id, status,
    priority_score, ai_summary, autonomy_suitability, priority_reasoning,
    scoring_status, severity, relevance_reason, source_status, snooze_until,
    close_reason, close_event_type, closed_at, created_at, org_id, team_id,
    creator_user_id, visibility, claimed_by_agent_id, claimed_by_user_id,
    rederive_owed
FROM tasks;

DROP TABLE tasks;

PRAGMA foreign_keys = on;

ALTER TABLE tasks_new RENAME TO tasks;

-- The eight indexes the dropped table carried, recreated verbatim.
CREATE UNIQUE INDEX idx_tasks_active_entity_event_dedup
    ON tasks(entity_id, event_type, dedup_key)
    WHERE status NOT IN ('done', 'dismissed');
CREATE INDEX        idx_tasks_status          ON tasks(status);
CREATE INDEX        idx_tasks_entity          ON tasks(entity_id);
CREATE INDEX        idx_tasks_status_priority ON tasks(status, priority_score DESC);
CREATE UNIQUE INDEX tasks_id_org_unique       ON tasks (id, org_id);
CREATE INDEX        tasks_claimed_agent_idx   ON tasks(claimed_by_agent_id) WHERE claimed_by_agent_id IS NOT NULL;
CREATE INDEX        tasks_claimed_user_idx    ON tasks(claimed_by_user_id)  WHERE claimed_by_user_id  IS NOT NULL;
CREATE INDEX        idx_tasks_rederive_owed   ON tasks(created_at) WHERE rederive_owed = 1;

-- +goose Down
SELECT 'down not supported';
