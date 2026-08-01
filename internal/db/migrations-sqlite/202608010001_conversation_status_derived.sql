-- +goose NO TRANSACTION
-- +goose Up
-- conversations.status shrinks to OUTCOME-OR-NOTHING: 'open' (a deliberate
-- park), a terminal ('completed' / 'failed' / 'cancelled' /
-- 'task_unsolvable'), or NULL. NULL is the mid-flight state — an active claim
-- is driving the conversation right now, or its last claim released without
-- writing an outcome and nobody has picked it up yet. "Queued" and "running"
-- become derived: read off the claim table plus the needs-driving predicate,
-- never stored.
--
-- Two schema consequences, both here:
--
--   1. The conversations_delegation_has_status CHECK (type <> 'delegation' OR
--      status IS NOT NULL) has to go — a delegation conversation is NULL for
--      its whole pre-outcome life now. SQLite can only drop a CHECK by
--      rebuilding the table, which is what the bulk of this file is.
--   2. The claim's partial index moves from WHERE status = 'queued' to
--      WHERE status IS NULL: same job (span only work that might need
--      driving), new spelling of the same set. The predicate's other arm — a
--      parked 'open' conversation woken by new input — is served by
--      idx_messages_undelivered, which already exists.
--
-- NO TRANSACTION because the rebuild needs PRAGMA foreign_keys toggles, which
-- are no-ops inside a transaction; the pool is capped at one connection, so
-- the pragmas hold for every statement here. The ordering constraints are the
-- same two the conversations refactor documented: foreign_keys OFF before the
-- drop (so the children's ON DELETE actions don't fire on what is really a
-- catalog swap), and back ON before the rename (so the table's own
-- self-referential parent_conversation_id FK is rewritten to the final name).

-- llm_spend reads conversations; SQLite refuses ALTERs while a view over the
-- table dangles, so it goes first and is recreated verbatim at the end.
DROP VIEW IF EXISTS llm_spend;

PRAGMA foreign_keys = off;

CREATE TABLE conversations_new (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    -- type names the owning surface: 'delegation' | 'curator' |
    -- 'interactive' (reserved) | namespaced 'subagent:<kind>' (reserved —
    -- never the bare word). App-validated, no value CHECK.
    type            TEXT NOT NULL DEFAULT 'delegation',
    creator_user_id TEXT DEFAULT '00000000-0000-0000-0000-000000000100' REFERENCES users(id),
    team_id         TEXT DEFAULT '00000000-0000-0000-0000-000000000010',
    visibility      TEXT NOT NULL DEFAULT 'team'
                       CHECK (visibility IN ('private','team','org')),
    task_id         TEXT REFERENCES tasks(id),
    prompt_id       TEXT REFERENCES prompts(id),
    trigger_id      TEXT REFERENCES event_handlers(id),
    trigger_type    TEXT NOT NULL DEFAULT 'manual',
    origin          TEXT NOT NULL DEFAULT 'blueprint',
    -- 'sdk' | 'native': the executing engine. A one-way ratchet per
    -- conversation — once any engagement runs native, the SDK can never
    -- continue this transcript.
    runtime         TEXT NOT NULL DEFAULT 'sdk',
    -- Outcome-or-nothing: 'open' | a terminal | NULL (mid-flight). Mint
    -- writes nothing here; parks write 'open'; terminals write their
    -- terminal; nothing else touches it. Engagement sub-state
    -- (fetching/cloning/agent_starting/awaiting_credentials) lives on the
    -- live claim's phase, coalesced over this on display reads.
    status          TEXT,
    model           TEXT,
    -- The SDK-runtime resume handle (former runs.session_id +
    -- projects.curator_session_id). NULL under runtime='native'.
    sdk_session_id  TEXT,
    worktree_path   TEXT,
    result_summary  TEXT,
    outcome         TEXT,
    outcome_reason  TEXT,
    failure_kind    TEXT,
    stop_reason     TEXT,
    started_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME,
    parked_at       DATETIME,
    -- Retires a conversation from its surface's "current" view without
    -- deleting history (the curator's reset / new-chat mechanism). An
    -- archived conversation is never claimed again.
    archived_at     DATETIME,
    actor_agent_id  TEXT REFERENCES agents(id) ON DELETE SET NULL,
    -- Curator anchor: the owning project (knowledge base, homing,
    -- cascade-delete). NULL for every other type.
    project_id      TEXT REFERENCES projects(id) ON DELETE CASCADE,
    -- Subagent link; scoping anchors are denormalized from the parent at
    -- mint so visibility never recurses.
    parent_conversation_id TEXT REFERENCES conversations_new(id) ON DELETE CASCADE,
    blueprint_run_id     TEXT REFERENCES blueprint_runs(id),
    blueprint_step_index INTEGER,
    triggering_event_id  TEXT REFERENCES events(id),
    -- Stamped once at enqueue and never bumped again — there is no requeue
    -- write left to bump it on (releasing a claim is the requeue).
    -- Display-only: the scheduler orders by started_at.
    queued_at       DATETIME,
    preferred_executor_id TEXT,
    CONSTRAINT conversations_creator_matches_trigger_type CHECK (
        (trigger_type = 'manual' AND creator_user_id IS NOT NULL)
        OR
        (trigger_type = 'event'  AND creator_user_id IS NULL)
    ),
    CONSTRAINT conversations_team_visibility_requires_team CHECK (
        visibility <> 'team' OR team_id IS NOT NULL
    ),
    CONSTRAINT conversations_origin_requires_parents CHECK (
        (origin = 'blueprint'
            AND blueprint_run_id IS NOT NULL
            AND task_id IS NOT NULL
            AND prompt_id IS NOT NULL)
        OR origin <> 'blueprint'
    )
);

-- Carry every row across, collapsing the retired vocabulary onto the new
-- one: a stored 'queued' or 'running' was exactly "mid-flight, no outcome",
-- which is now NULL. The claim rows are untouched, so a conversation that
-- was 'running' under a live claim still reads 'running' on the wire (the
-- display ladder's active-claim rung) and one that was 'queued' still reads
-- 'queued' (its needs-driving rung).
INSERT INTO conversations_new (
    id, org_id, type, creator_user_id, team_id, visibility, task_id,
    prompt_id, trigger_id, trigger_type, origin, runtime, status, model,
    sdk_session_id, worktree_path, result_summary, outcome, outcome_reason,
    failure_kind, stop_reason, started_at, completed_at, parked_at,
    archived_at, actor_agent_id, project_id, parent_conversation_id,
    blueprint_run_id, blueprint_step_index, triggering_event_id, queued_at,
    preferred_executor_id)
SELECT
    id, org_id, type, creator_user_id, team_id, visibility, task_id,
    prompt_id, trigger_id, trigger_type, origin, runtime,
    CASE WHEN status IN ('queued','running') THEN NULL ELSE status END,
    model,
    sdk_session_id, worktree_path, result_summary, outcome, outcome_reason,
    failure_kind, stop_reason, started_at, completed_at, parked_at,
    archived_at, actor_agent_id, project_id, parent_conversation_id,
    blueprint_run_id, blueprint_step_index, triggering_event_id, queued_at,
    preferred_executor_id
FROM conversations;

DROP TABLE conversations;

PRAGMA foreign_keys = on;

ALTER TABLE conversations_new RENAME TO conversations;

CREATE INDEX        idx_conversations_task           ON conversations(task_id);
CREATE INDEX        idx_conversations_prompt_started ON conversations(prompt_id, started_at DESC);
CREATE INDEX        idx_conversations_trigger        ON conversations(trigger_id);
CREATE INDEX        idx_conversations_status         ON conversations(status);
CREATE UNIQUE INDEX conversations_id_org_unique      ON conversations (id, org_id);
CREATE INDEX        conversations_actor_agent_idx    ON conversations(actor_agent_id) WHERE actor_agent_id IS NOT NULL;
CREATE INDEX        idx_conversations_blueprint      ON conversations(blueprint_run_id, blueprint_step_index);
CREATE UNIQUE INDEX conversations_event_trigger_fence ON conversations (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL;
-- The claim scan's index: the NULL arm of the needs-driving predicate, in
-- the claim's own (started_at, id) order. Replaces the WHERE status='queued'
-- partial — same set, new spelling.
CREATE INDEX        idx_conversations_needs_driving  ON conversations(started_at, id) WHERE status IS NULL;
-- The curator's conversation lookup: one live conversation per
-- (project, creator); archived rows excluded by the app-side predicate.
CREATE INDEX        idx_conversations_project        ON conversations(project_id, creator_user_id) WHERE project_id IS NOT NULL;
CREATE INDEX        idx_conversations_parent         ON conversations(parent_conversation_id) WHERE parent_conversation_id IS NOT NULL;

-- llm_spend, recreated byte-for-byte from the conversations refactor: this
-- migration changes no column the view reads.
CREATE VIEW llm_spend AS
  SELECT CASE c.type WHEN 'delegation' THEN 'run' ELSE 'curator' END AS source,
         m.id AS source_id,
         m.org_id,
         c.team_id,
         CASE WHEN c.type = 'curator' THEN 'curator'
              WHEN c.trigger_type = 'manual' THEN 'manual'
              ELSE 'autonomous' END AS category,
         NULL AS subtype,
         c.creator_user_id,
         c.actor_agent_id,
         c.trigger_id,
         m.model,
         COALESCE(m.cost_usd, 0) AS total_cost_usd,
         COALESCE(m.input_tokens, 0) AS input_tokens,
         COALESCE(m.output_tokens, 0) AS output_tokens,
         COALESCE(m.cache_read_tokens, 0) AS cache_read_tokens,
         COALESCE(m.cache_creation_tokens, 0) AS cache_creation_tokens,
         m.created_at AS occurred_at
  FROM messages m
  JOIN conversations c ON c.id = m.conversation_id
  WHERE c.type IN ('delegation', 'curator')
    AND (m.role = 'assistant' OR m.cost_usd IS NOT NULL)
  UNION ALL
  SELECT 'system' AS source,
         id AS source_id,
         org_id,
         NULL AS team_id,
         'system_overhead' AS category,
         job AS subtype,
         NULL AS creator_user_id,
         NULL AS actor_agent_id,
         NULL AS trigger_id,
         model,
         total_cost_usd,
         input_tokens,
         output_tokens,
         cache_read_tokens,
         cache_creation_tokens,
         started_at AS occurred_at
  FROM system_llm_runs;

-- +goose Down
SELECT 'down not supported';
