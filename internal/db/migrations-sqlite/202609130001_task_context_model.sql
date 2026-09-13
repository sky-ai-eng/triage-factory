-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE conversation_memory_attempts (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    started_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at      DATETIME,
    outcome           TEXT,
    error_kind        TEXT,
    error_message     TEXT,
    system_llm_run_id TEXT REFERENCES system_llm_runs(id) ON DELETE SET NULL,
    window_rows_total INTEGER NOT NULL DEFAULT 0,
    window_rows_sent  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_conversation_memory_attempts_conversation
    ON conversation_memory_attempts(conversation_id, started_at DESC, id DESC);

ALTER TABLE conversations ADD COLUMN ended_at DATETIME;
ALTER TABLE conversations ADD COLUMN ended_reason TEXT;

CREATE INDEX idx_conversations_task_ended ON conversations (task_id) WHERE ended_at IS NOT NULL;

PRAGMA foreign_keys = off;

CREATE TABLE conversation_memory_new (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id  TEXT NOT NULL UNIQUE REFERENCES conversations(id) ON DELETE CASCADE,
    blueprint_run_id TEXT REFERENCES blueprint_runs(id) ON DELETE SET NULL,
    agent_content    TEXT,
    source           TEXT NOT NULL,
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

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

CREATE TEMP TABLE superseded_blueprint_runs AS
SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
        PARTITION BY task_id ORDER BY started_at DESC, id DESC
    ) AS rn
    FROM blueprint_runs
    WHERE status = 'running'
)
WHERE rn > 1;

UPDATE conversations
SET ended_at     = CURRENT_TIMESTAMP,
    ended_reason = 'delegated'
WHERE ended_at IS NULL
  AND parent_conversation_id IS NULL
  AND (status IS NULL OR status NOT IN ('completed', 'failed'))
  AND blueprint_run_id IN (SELECT id FROM superseded_blueprint_runs);

UPDATE blueprint_runs
SET status       = 'cancelled',
    abort_reason = 'system_cancelled',
    completed_at = CURRENT_TIMESTAMP
WHERE id IN (SELECT id FROM superseded_blueprint_runs);

DROP TABLE superseded_blueprint_runs;

CREATE UNIQUE INDEX blueprint_runs_one_active_run_per_task
    ON blueprint_runs (task_id) WHERE status = 'running';

CREATE TABLE workspace_snapshots_by_task (
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    task_id         TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    state           TEXT NOT NULL CHECK (state IN ('pending', 'written', 'failed')),
    writer_claim_id TEXT NOT NULL,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, task_id)
);

INSERT INTO workspace_snapshots_by_task (org_id, task_id, state, writer_claim_id, updated_at)
SELECT org_id, task_id, state, writer_claim_id, updated_at
FROM (
    SELECT ws.org_id          AS org_id,
           br.task_id         AS task_id,
           ws.state           AS state,
           ws.writer_claim_id AS writer_claim_id,
           ws.updated_at      AS updated_at,
           ROW_NUMBER() OVER (
               PARTITION BY ws.org_id, br.task_id
               ORDER BY br.started_at DESC, br.id DESC
           ) AS rn
    FROM workspace_snapshots ws
    JOIN blueprint_runs br ON br.id = ws.blueprint_run_id
)
WHERE rn = 1;

DROP TABLE workspace_snapshots;
ALTER TABLE workspace_snapshots_by_task RENAME TO workspace_snapshots;

CREATE INDEX idx_conversations_task_open ON conversations (task_id, started_at DESC, id DESC) WHERE ended_at IS NULL;

ALTER TABLE conversations ADD COLUMN system_block TEXT NOT NULL DEFAULT '';

UPDATE tasks SET status = 'in_progress' WHERE status = 'in_review';

UPDATE tasks
   SET status = 'in_progress', snooze_until = NULL
 WHERE status IN ('queued', 'snoozed')
   AND (claimed_by_user_id IS NOT NULL OR claimed_by_agent_id IS NOT NULL);

PRAGMA foreign_keys = off;

CREATE TABLE tasks_new (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    event_type           TEXT NOT NULL REFERENCES events_catalog(id) ON DELETE RESTRICT,
    dedup_key            TEXT NOT NULL DEFAULT '',
    primary_event_id     TEXT NOT NULL REFERENCES events(id),
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
    team_id              TEXT DEFAULT '00000000-0000-0000-0000-000000000010',
    creator_user_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000100',
    visibility           TEXT NOT NULL DEFAULT 'team'
                            CHECK (visibility IN ('private','team','org')),
    claimed_by_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    claimed_by_user_id   TEXT REFERENCES users(id)  ON DELETE SET NULL,
    rederive_owed        BOOLEAN NOT NULL DEFAULT 0,
    CONSTRAINT tasks_claim_xor CHECK (claimed_by_agent_id IS NULL OR claimed_by_user_id IS NULL),
    CONSTRAINT tasks_claimed_requires_team CHECK (
        (claimed_by_user_id IS NULL AND claimed_by_agent_id IS NULL) OR team_id IS NOT NULL
    ),
    CONSTRAINT tasks_queue_unclaimed CHECK (
        status NOT IN ('queued', 'snoozed')
        OR (claimed_by_user_id IS NULL AND claimed_by_agent_id IS NULL)
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
