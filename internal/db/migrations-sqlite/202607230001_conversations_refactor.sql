-- +goose NO TRANSACTION
-- +goose Up
-- The data-model refactor: runs + curator_requests dissolve into
-- conversations (durable context) + claims (per-engagement execution state),
-- and run_messages + curator_messages unify into messages (one transcript
-- canon, keyed by conversation). The three pending side-tables
-- (run_pending_input, staged_agent_injections, curator_pending_context)
-- dissolve into delivered=0 message rows; run_credentials becomes
-- claim_credentials (Postgres-only — local mode reads the live secret store,
-- so the SQLite twin simply drops the old table).
--
-- This migration targets the RELEASED SQLite lineage (last tag v1.12.2). The
-- two unreleased migrations 202607220001/202607220002 were deleted from the
-- tree and their columns are absorbed into the messages DDL below; a dev DB
-- that applied them must be re-initialized with ./scripts/clean-slate.sh
-- (their goose_db_version rows are inert). NO TRANSACTION because the
-- rebuild needs PRAGMA foreign_keys toggles, which are no-ops inside a
-- transaction; the pool is capped at one connection, so the pragmas hold for
-- every statement here. A crash mid-migration leaves a partial state goose
-- will retry into — local-mode recovery is clean-slate, accepted for a
-- single-user dev-shaped database.
--
-- Ordering constraints, load-bearing:
--   1. RENAME runs -> conversations happens while foreign_keys is still ON
--      (the DSN default): SQLite only rewrites other tables' REFERENCES
--      clauses on rename when the pragma is enabled, and that rewrite is
--      what lets every child table (artifacts, external_actions, run_memory,
--      run_memory_entities, run_worktrees) survive with a column rename
--      instead of a rebuild.
--   2. foreign_keys goes OFF before the conversations rebuild: with it ON,
--      DROP TABLE performs an implicit DELETE FROM, which would fire the
--      children's ON DELETE CASCADE / SET NULL actions and destroy their
--      rows. OFF makes the drop+rename a pure catalog swap.

-- ---------------------------------------------------------------------------
-- 0. llm_spend references runs + curator_requests; SQLite rewrites view
-- bodies on table rename and then refuses ALTERs while a view dangles, so it
-- goes first and is recreated (re-keyed) at the end.
DROP VIEW IF EXISTS llm_spend;

-- ---------------------------------------------------------------------------
-- 1. Rename the spine + re-key the surviving children (foreign_keys still ON).
ALTER TABLE runs RENAME TO conversations;
ALTER TABLE artifacts            RENAME COLUMN run_id TO conversation_id;
ALTER TABLE external_actions     RENAME COLUMN run_id TO conversation_id;
ALTER TABLE run_memory           RENAME COLUMN run_id TO conversation_id;
ALTER TABLE run_memory_entities  RENAME COLUMN run_id TO conversation_id;
ALTER TABLE run_worktrees        RENAME COLUMN run_id TO conversation_id;

PRAGMA foreign_keys = off;

-- Stage the runs-era claim columns before the rebuild discards them; the
-- claims mint below (step 4) reads this snapshot. duration_ms / num_turns
-- ride along — the flattened engagement telemetry lands on the one migrated
-- claim. Dropped at the end.
CREATE TABLE conversations_claim_src AS
SELECT id, executor_id, boot_epoch, cred_pubkey, claimed_at,
       duration_ms, num_turns
FROM conversations
WHERE executor_id IS NOT NULL;

-- Stage the runs-era settled dollars too: the historical cost stamp
-- (step 5b) re-keys each conversation's total onto its last message row,
-- claim or no claim. Dropped at the end.
CREATE TABLE conversations_cost_src AS
SELECT id, total_cost_usd
FROM conversations
WHERE total_cost_usd IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. The target conversations shape. Differences from the runs era: type /
-- project_id / parent_conversation_id / archived_at /
-- runtime are new; session_id is renamed sdk_session_id (it now also absorbs
-- projects.curator_session_id); team_id and status drop NOT NULL (a curator
-- conversation may carry a NULL team snapshot and has no work lifecycle);
-- the claim machinery (claimed_at / attempts / executor_id / boot_epoch /
-- cred_pubkey) moves to claims. The accounting cache columns
-- (total_cost_usd / duration_ms / num_turns / the four token rollups) are
-- gone entirely: messages is the money/token ledger (cost_usd + per-row
-- tokens, summed at read time), claims carry the per-engagement
-- duration/turns telemetry.
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
    -- Work lifecycle for delegation/interactive/subagent conversations
    -- (vocabulary unchanged from the runs era). NULL for curator
    -- conversations, whose turn state is derived from messages + claims.
    status          TEXT DEFAULT 'cloning',
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
    ),
    CONSTRAINT conversations_delegation_has_status CHECK (
        type <> 'delegation' OR status IS NOT NULL
    )
);

INSERT INTO conversations_new (
    id, org_id, type, creator_user_id, team_id, visibility, task_id,
    prompt_id, trigger_id, trigger_type, origin, runtime, status, model,
    sdk_session_id, worktree_path, result_summary, outcome, outcome_reason,
    failure_kind, stop_reason, started_at, completed_at, parked_at,
    actor_agent_id,
    blueprint_run_id, blueprint_step_index, triggering_event_id, queued_at,
    preferred_executor_id)
SELECT
    id, org_id, 'delegation', creator_user_id, team_id, visibility, task_id,
    prompt_id, trigger_id, trigger_type, origin, 'sdk', status, model,
    session_id, worktree_path, result_summary, outcome, outcome_reason,
    failure_kind, stop_reason, started_at, completed_at, parked_at,
    actor_agent_id,
    blueprint_run_id, blueprint_step_index, triggering_event_id, queued_at,
    preferred_executor_id
FROM conversations;

DROP TABLE conversations;

-- Back ON before the rename: SQLite only rewrites conversations_new's own
-- self-referential parent_conversation_id FK clause to the new name while
-- the pragma is enabled. Every remaining drop below is child-before-parent,
-- so enforcement stays safe from here on.
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
CREATE INDEX        idx_conversations_queued         ON conversations(started_at, id) WHERE status = 'queued';
-- The curator's conversation lookup: one live conversation per
-- (project, creator); archived rows excluded by the app-side predicate.
CREATE INDEX        idx_conversations_project        ON conversations(project_id, creator_user_id) WHERE project_id IS NOT NULL;
CREATE INDEX        idx_conversations_parent         ON conversations(parent_conversation_id) WHERE parent_conversation_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. Curator conversations: one per (project, creator) that ever chatted,
-- visibility='private' (creator-scoped — the runs-era RLS vocabulary already
-- expresses curator scoping), no work-lifecycle status. The team snapshot is
-- the latest request's; the SDK resume handle transfers to the conversation
-- of whichever creator sent the project's latest request (session continuity
-- for the dominant user — exact preservation at local N=1).
INSERT INTO conversations (
    id, org_id, type, creator_user_id, team_id, visibility, trigger_type,
    origin, runtime, status, project_id, started_at, sdk_session_id)
SELECT
    lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
          substr(hex(randomblob(2)), 2) || '-' ||
          substr('89ab', (abs(random()) % 4) + 1, 1) ||
          substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))),
    cr.org_id,
    'curator',
    cr.creator_user_id,
    (SELECT c2.team_id FROM curator_requests c2
      WHERE c2.project_id = cr.project_id AND c2.creator_user_id = cr.creator_user_id
      ORDER BY c2.created_at DESC, c2.rowid DESC LIMIT 1),
    'private',
    'manual',
    'curator',
    'sdk',
    NULL,
    cr.project_id,
    MIN(cr.created_at),
    CASE WHEN cr.creator_user_id = (
            SELECT c3.creator_user_id FROM curator_requests c3
             WHERE c3.project_id = cr.project_id
             ORDER BY c3.created_at DESC, c3.rowid DESC LIMIT 1)
         THEN (SELECT p.curator_session_id FROM projects p WHERE p.id = cr.project_id)
    END
FROM curator_requests cr
GROUP BY cr.project_id, cr.creator_user_id, cr.org_id;

-- ---------------------------------------------------------------------------
-- 4. claims: per-engagement execution state, both surfaces. Migrated ids are
-- REUSED from the source rows (claim id = run id for the runs era's single
-- flattened claim, claim id = request id for curator turns) so message
-- attribution below is a plain join and re-runs of external tooling keep
-- stable identifiers.
CREATE TABLE claims (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    -- The queued turn this engagement was minted to drive — curator pickups
    -- stamp it so failed (zero-message) claims stay attributable to their
    -- exact turn; NULL for delegation claims and pre-refactor history.
    message_id      INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    executor_id     TEXT NOT NULL,
    boot_epoch      INTEGER NOT NULL DEFAULT 0,
    -- The per-engagement credential sidecar's X25519 pubkey (multi-mode
    -- only; stays NULL locally).
    cred_pubkey     TEXT,
    claimed_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- NULL = this claim is live (its executor owns the conversation's
    -- process). Stamped exactly once, on release.
    released_at     DATETIME,
    -- App-validated: 'completed' | 'failed' | 'cancelled' | 'requeued' |
    -- 'parked' | 'reaped'. NULL while live.
    outcome         TEXT,
    error           TEXT,
    -- Engagement telemetry the runtime reports per invocation and nothing
    -- can derive from messages. Dollars and tokens deliberately do NOT
    -- live here — messages is the ledger.
    duration_ms     INTEGER,
    num_turns       INTEGER,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX claims_id_org_unique   ON claims (id, org_id);
CREATE UNIQUE INDEX idx_claims_one_active  ON claims(conversation_id) WHERE released_at IS NULL;
CREATE INDEX        idx_claims_conversation ON claims(conversation_id, claimed_at);

-- ---------------------------------------------------------------------------
-- 4b. messages: the unified transcript canon (the columnar-canon columns from
-- the deleted 202607220001/202607220002 land here directly). The DDL sits
-- BEFORE the claim mints below: claims.message_id references this table, and
-- with foreign_keys back on SQLite refuses DML against a child whose FK
-- parent table is missing — even rows that would stamp NULL.
CREATE TABLE messages (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id                TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    conversation_id       TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    -- Stamps the whole turn: the requesting user owns their user row AND
    -- the assistant/tool rows produced in response. NULL = system.
    user_id               TEXT REFERENCES users(id) ON DELETE SET NULL,
    -- Attributes a row to the executor engagement that produced it
    -- (per-engagement token accounting, turn reconstruction). Never read
    -- by assembly.
    claim_id              TEXT REFERENCES claims(id) ON DELETE SET NULL,
    role                  TEXT NOT NULL,
    content               TEXT,
    subtype               TEXT DEFAULT 'text',
    tool_calls            TEXT,
    tool_call_id          TEXT,
    is_error              BOOLEAN DEFAULT 0,
    metadata              TEXT,
    model                 TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    -- Dollars settled at this row: NULL = not a settlement row, 0 =
    -- genuinely free. The runtime stamps an invocation's reported total
    -- as ONE lump on the invocation's last recorded row at terminal time
    -- (no proration — proration without a pricing table would be
    -- confidently wrong), so SUM over any window is the spend in that
    -- window.
    cost_usd              REAL,
    created_at            DATETIME DEFAULT CURRENT_TIMESTAMP,
    -- Replay-only reasoning details ({index, type, text?, signature?,
    -- data?}); the API validates signatures, not ordering.
    reasoning             TEXT,
    -- Non-text content (images/files) the flat content column can't carry.
    content_blocks        TEXT,
    -- 0 = durable pending input not yet folded into any assembly; flipped
    -- exactly-once via UPDATE … RETURNING at delivery.
    delivered             BOOLEAN NOT NULL DEFAULT 1,
    -- 'active' | 'elided' | 'inactive' — app-validated, sticky row state.
    window_state          TEXT NOT NULL DEFAULT 'active',
    -- Assembly-order override: effective sort key is COALESCE(seq, id).
    seq                   REAL
);
CREATE INDEX idx_messages_conversation     ON messages(conversation_id);
CREATE INDEX idx_messages_conversation_seq ON messages(conversation_id, (COALESCE(seq, id)));
CREATE INDEX idx_messages_undelivered     ON messages(conversation_id) WHERE delivered = 0;
CREATE INDEX idx_messages_claim           ON messages(claim_id) WHERE claim_id IS NOT NULL;
-- Per-user message reads; partial because most rows are agent-side (NULL user).
CREATE INDEX idx_messages_user            ON messages(user_id) WHERE user_id IS NOT NULL;

-- The runs era flattened every engagement into columns + an attempts
-- counter, so per-retry history is unrecoverable; mint ONE claim per
-- conversation that was ever claimed (executor_id survives requeues only on
-- the final owner). Terminal rows release with a mapped outcome; a parked
-- 'open' row releases as 'parked'; anything else was mid-flight at migration
-- time and stays an active claim for the boot sweep to reconcile.
INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
                    cred_pubkey, claimed_at, released_at, outcome,
                    duration_ms, num_turns)
SELECT
    c.id, c.org_id, c.id, r_executor_id, COALESCE(r_boot_epoch, 0),
    r_cred_pubkey, COALESCE(r_claimed_at, c.started_at),
    CASE
      WHEN c.status IN ('completed','failed','cancelled','task_unsolvable')
        THEN COALESCE(c.completed_at, c.started_at)
      WHEN c.status = 'open'
        THEN COALESCE(c.parked_at, c.completed_at, c.started_at)
    END,
    CASE c.status
      WHEN 'completed'        THEN 'completed'
      WHEN 'failed'           THEN 'failed'
      WHEN 'cancelled'        THEN 'cancelled'
      WHEN 'task_unsolvable'  THEN 'failed'
      WHEN 'open'             THEN 'parked'
    END,
    r_duration_ms, r_num_turns
FROM (SELECT id, org_id, status, started_at, completed_at, parked_at FROM conversations WHERE type = 'delegation') c
JOIN (SELECT id AS rid, executor_id AS r_executor_id, boot_epoch AS r_boot_epoch,
             cred_pubkey AS r_cred_pubkey, claimed_at AS r_claimed_at,
             duration_ms AS r_duration_ms, num_turns AS r_num_turns
        FROM conversations_claim_src) src ON src.rid = c.id;

-- Curator turns: one claim per non-queued request (a queued request never
-- had an engagement — it becomes an undelivered user message below). Local
-- mode has no homing, so executor_id falls back to the '' sentinel.
-- message_id stays NULL on migrated claims — exact failed-pickup attribution
-- applies only to pickups minted under the new model.
INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
                    cred_pubkey, claimed_at, released_at, outcome, error,
                    duration_ms, num_turns)
SELECT
    cr.id, cr.org_id, conv.id, COALESCE(cr.home_instance_id, ''), 0,
    NULLIF(cr.cred_pubkey, ''),
    COALESCE(cr.started_at, cr.created_at),
    CASE WHEN cr.status IN ('done','cancelled','failed')
         THEN COALESCE(cr.finished_at, cr.created_at) END,
    CASE cr.status
      WHEN 'done'      THEN 'completed'
      WHEN 'cancelled' THEN 'cancelled'
      WHEN 'failed'    THEN 'failed'
    END,
    cr.error_msg, cr.duration_ms, cr.num_turns
FROM curator_requests cr
JOIN conversations conv
  ON conv.project_id = cr.project_id
 AND conv.creator_user_id = cr.creator_user_id
 AND conv.type = 'curator'
WHERE cr.status <> 'queued';

-- ---------------------------------------------------------------------------
-- 5. The transcripts land in messages (DDL above, with the claims DDL).
-- Delegation transcripts copy with their ids preserved (id is the assembly
-- sort key) and attach to the conversation's single migrated claim when one
-- exists.
INSERT INTO messages (id, org_id, conversation_id, claim_id, role, content,
                      subtype, tool_calls, tool_call_id, is_error, metadata,
                      model, input_tokens, output_tokens, cache_read_tokens,
                      cache_creation_tokens, created_at)
SELECT rm.id, rm.org_id, rm.run_id,
       (SELECT cl.id FROM claims cl WHERE cl.conversation_id = rm.run_id),
       rm.role, rm.content, rm.subtype, rm.tool_calls, rm.tool_call_id,
       rm.is_error, rm.metadata, rm.model, rm.input_tokens, rm.output_tokens,
       rm.cache_read_tokens, rm.cache_creation_tokens, rm.created_at
FROM run_messages rm
ORDER BY rm.id;

-- Curator transcripts: each request becomes a user message (the former
-- curator_requests.user_input — queued requests arrive undelivered)
-- followed by its streamed rows, appended in (request, arm, row) order so
-- fresh AUTOINCREMENT ids preserve the transcript order under
-- COALESCE(seq, id). An executed request's user row carries its claim,
-- exactly as live delivery stamps it — which also makes the turn's rows
-- one claim-keyed group for the cost stamp below.
INSERT INTO messages (org_id, conversation_id, user_id, claim_id, role,
                      content, subtype, tool_calls, tool_call_id, is_error,
                      metadata, model, input_tokens, output_tokens,
                      cache_read_tokens, cache_creation_tokens, delivered,
                      created_at)
SELECT org_id, conversation_id, user_id, claim_id, role, content, subtype,
       tool_calls, tool_call_id, is_error, metadata, model, input_tokens,
       output_tokens, cache_read_tokens, cache_creation_tokens, delivered,
       created_at
FROM (
  SELECT cr.org_id AS org_id, conv.id AS conversation_id,
         cr.creator_user_id AS user_id,
         CASE WHEN cr.status <> 'queued' THEN cr.id END AS claim_id,
         'user' AS role, cr.user_input AS content, 'text' AS subtype,
         NULL AS tool_calls, NULL AS tool_call_id, 0 AS is_error,
         NULL AS metadata, NULL AS model, NULL AS input_tokens,
         NULL AS output_tokens, NULL AS cache_read_tokens,
         NULL AS cache_creation_tokens,
         CASE WHEN cr.status = 'queued' THEN 0 ELSE 1 END AS delivered,
         cr.created_at AS created_at,
         cr.created_at AS ord1, cr.rowid AS ord2, 0 AS ord3, 0 AS ord4
    FROM curator_requests cr
    JOIN conversations conv
      ON conv.project_id = cr.project_id
     AND conv.creator_user_id = cr.creator_user_id
     AND conv.type = 'curator'
  UNION ALL
  SELECT cm.org_id, conv.id, cm.creator_user_id,
         CASE WHEN cr.status <> 'queued' THEN cm.request_id END,
         cm.role, cm.content, cm.subtype, cm.tool_calls, cm.tool_call_id,
         cm.is_error, cm.metadata, cm.model, cm.input_tokens,
         cm.output_tokens, cm.cache_read_tokens, cm.cache_creation_tokens,
         1, cm.created_at,
         cr.created_at AS ord1, cr.rowid AS ord2, 1 AS ord3, cm.id AS ord4
    FROM curator_messages cm
    JOIN curator_requests cr ON cr.id = cm.request_id
    JOIN conversations conv
      ON conv.project_id = cr.project_id
     AND conv.creator_user_id = cr.creator_user_id
     AND conv.type = 'curator'
)
ORDER BY ord1, ord2, ord3, ord4;

-- ---------------------------------------------------------------------------
-- 5b. Historical dollars re-key onto the ledger: each conversation's / each
-- curator turn's settled total lands as one lump on its LAST message row —
-- delegation from the staged runs-era rollup keyed by conversation, curator
-- from each request's per-turn cost keyed by the claim its rows carry
-- (MAX(id) is the last stream row, or the user row itself when nothing
-- streamed). Runs BEFORE step 6 so a pending side-table row can never be
-- mistaken for a transcript's last message. A conversation/turn with
-- dollars but zero rows loses the stamp — accepted: a result-reported cost
-- implies streamed messages, so the residue is negligible.
UPDATE messages SET cost_usd = (
    SELECT s.total_cost_usd FROM conversations_cost_src s
    WHERE s.id = messages.conversation_id)
WHERE messages.id IN (
    SELECT MAX(m.id) FROM messages m
    JOIN conversations_cost_src s ON s.id = m.conversation_id
    GROUP BY m.conversation_id);

UPDATE messages SET cost_usd = (
    SELECT cr.cost_usd FROM curator_requests cr WHERE cr.id = messages.claim_id)
WHERE messages.id IN (
    SELECT MAX(m.id) FROM messages m
    JOIN curator_requests cr ON cr.id = m.claim_id
    WHERE cr.cost_usd IS NOT NULL
    GROUP BY m.claim_id);

-- ---------------------------------------------------------------------------
-- 6. The pending side-tables dissolve into undelivered message rows.
INSERT INTO messages (org_id, conversation_id, user_id, role, content,
                      delivered, created_at)
SELECT org_id, run_id, user_id, 'user', message, 0, created_at
FROM run_pending_input;

INSERT INTO messages (org_id, conversation_id, role, content, subtype,
                      metadata, delivered, created_at)
SELECT org_id, run_id, 'user', body, 'injection:system-note',
       json_object('producer', producer), 0, created_at
FROM staged_agent_injections
ORDER BY created_at, id;

-- Only unconsumed context notices are still pending; the change payload
-- rides metadata (rendering happens at delivery against live project
-- state, exactly as before).
-- The creator_user_id join leg is vestigial precision: every pre-refactor
-- write left the column at its sentinel default, so it always matches the
-- (equally sentinel-defaulted) request-derived conversation. Kept for shape
-- symmetry with the transcript join above; an unmatched row would drop, not
-- error.
INSERT INTO messages (org_id, conversation_id, user_id, role, content,
                      subtype, metadata, delivered, created_at)
SELECT cpc.org_id, conv.id, cpc.creator_user_id, 'user', '',
       'injection:context',
       json_object('change_type', cpc.change_type,
                   'baseline_value', cpc.baseline_value),
       0, cpc.created_at
FROM curator_pending_context cpc
JOIN conversations conv
  ON conv.project_id = cpc.project_id
 AND conv.creator_user_id = cpc.creator_user_id
 AND conv.type = 'curator'
WHERE cpc.consumed_at IS NULL
ORDER BY cpc.created_at, cpc.id;

-- ---------------------------------------------------------------------------
-- 7. Drop everything the model dissolved.
DROP TABLE run_messages;
DROP TABLE curator_messages;
DROP TABLE curator_requests;
DROP TABLE curator_pending_context;
DROP TABLE run_pending_input;
DROP TABLE staged_agent_injections;
DROP TABLE run_credentials;
DROP TABLE curator_turn_credentials;
DROP TABLE conversations_claim_src;
DROP TABLE conversations_cost_src;
ALTER TABLE projects DROP COLUMN curator_session_id;

-- ---------------------------------------------------------------------------
-- 8. llm_spend, re-keyed onto the ledger: ONE messages-based arm covers both
-- agent surfaces — a row participates when it carries tokens (an assistant
-- row) or settled dollars (any cost-stamped row) — attributed through the
-- owning conversation's team/creator/actor/trigger snapshot (curator
-- conversations carry NULL actor/trigger by construction); the system arm is
-- unchanged.
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
