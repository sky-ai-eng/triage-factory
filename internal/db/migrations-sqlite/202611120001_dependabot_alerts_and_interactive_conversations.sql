-- +goose Up
-- Tasks carry an optimistic-concurrency token, and a pending firing can name
-- the note its event was handed to a conversation as. The poll records whether
-- it can read each repository's Dependabot alerts, and a refresh mark can name
-- them. A person's claim mints the conversation they work the task in, which
-- tasks already held get here. The brain marks the delegated failures and
-- parks it has told, interactive conversations count toward llm_spend, a
-- person's message records its reasoning effort, and conversation memory
-- records when its content last changed.

-- ===========================================================================
-- 1. Tasks and pending firings
-- ===========================================================================

-- The task's optimistic-concurrency token. A write that changes a column a
-- person acts on (status, either claim, snooze_until, closed_at, close_reason,
-- close_event_type, team_id) raises it in the same UPDATE, through
-- db.TaskSetSQL; a write to any other column leaves it. A person's write names
-- the version it read and lands only on a task still at it.
--
-- The bump is in each writer's SET rather than in a trigger: SQLite's
-- RETURNING reports the row as the statement itself wrote it, without what a
-- trigger changed afterwards, so a writer's returned row would carry the old
-- version.
ALTER TABLE tasks ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

-- handed_note_id names the staged note a firing's event was handed to a
-- conversation as (the messages row id, in decimal), while the firing's task
-- had a blueprint running and one of its conversations started an
-- engagement. NULL on a firing nothing was handed over for. The firing stays
-- unsettled: the firing worker settles it once the run ends, by whether the
-- note was read.
ALTER TABLE pending_firings ADD COLUMN handed_note_id TEXT NULL;

-- ===========================================================================
-- 2. Dependabot alerts
-- ===========================================================================

-- Whether the org's GitHub credential can read a repository's Dependabot
-- alerts, as GitHub last answered the poll's probe: readable, no_permission
-- (the credential was refused), or disabled (alerts are turned off on the
-- repository). One row per repository, replaced by each probe; a repository
-- the poll has not probed has no row.
--
-- alerts_etag is the ETag GitHub answered the poll's last listing of the
-- repository's open alerts with: the next listing's condition, '' when there
-- is none to send. updates_read_at is when the poll last read what Dependabot
-- did about those alerts (the pull request it opened, or the error it
-- reported), which the listing does not carry; NULL until the first read.
--
-- checked_at and updates_read_at are written from Go as UTC, like
-- github_refresh_marks.marked_at.
CREATE TABLE repository_dependabot_access (
    repository_id   TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    access          TEXT NOT NULL CHECK (access IN ('readable', 'no_permission', 'disabled')),
    checked_at      DATETIME NOT NULL,
    alerts_etag     TEXT NOT NULL DEFAULT '',
    updates_read_at DATETIME,
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

-- A refresh mark can name a repository's Dependabot alerts: kind
-- dependabot_alerts, with an empty target, since one listing reads every
-- alert of the repository. Every other kind still needs a target. SQLite
-- cannot change a CHECK in place, so the table is rebuilt with its rows. No
-- table references it, so dropping it cascades nowhere.
CREATE TABLE github_refresh_marks_new (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    repository_id   TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('pull_request', 'head_sha', 'branch', 'dependabot_alerts')),
    target          TEXT NOT NULL,
    revision        INTEGER NOT NULL DEFAULT 1,
    first_marked_at DATETIME NOT NULL,
    marked_at       DATETIME NOT NULL,
    CHECK ((kind = 'dependabot_alerts') = (target = '')),
    UNIQUE (repository_id, kind, target),
    FOREIGN KEY (repository_id, org_id) REFERENCES repositories(id, org_id) ON DELETE CASCADE
);

INSERT INTO github_refresh_marks_new (id, org_id, repository_id, kind, target, revision, first_marked_at, marked_at)
SELECT id, org_id, repository_id, kind, target, revision, first_marked_at, marked_at
FROM github_refresh_marks;

DROP TABLE github_refresh_marks;
ALTER TABLE github_refresh_marks_new RENAME TO github_refresh_marks;

-- The poll reads an org's marks oldest first.
CREATE INDEX idx_github_refresh_marks_org ON github_refresh_marks (org_id, first_marked_at, id);

-- ===========================================================================
-- 3. Conversations
-- ===========================================================================

-- The brain posts a delegated conversation's failure notice and opens its
-- paused notice from these rows, and marks each one it has told.
-- failure_noticed_at is when the failure notice was posted. A conversation
-- fails at most once, so a timestamp is enough. pause_noticed_parked_at is the
-- parked_at of the park whose notice was opened: a park keeps its parked_at
-- until a claim or a resume clears it, so each park has its own value. Only the
-- brain writes either.
ALTER TABLE conversations ADD COLUMN failure_noticed_at DATETIME;
ALTER TABLE conversations ADD COLUMN pause_noticed_parked_at DATETIME;

-- Failed rows accumulate, and the few still owed a notice are all the read
-- takes. Likewise for parks: stored 'open' covers every concluded step, and
-- only the live parks not yet told are read.
CREATE INDEX idx_conversations_failure_notice_due ON conversations (status, completed_at, id)
    WHERE status = 'failed' AND failure_noticed_at IS NULL AND creator_user_id IS NOT NULL;
CREATE INDEX idx_conversations_pause_notice_due ON conversations (status, parked_at, id)
    WHERE status = 'open' AND park_reason IS NOT NULL AND parked_at IS NOT NULL
      AND creator_user_id IS NOT NULL AND ended_at IS NULL AND archived_at IS NULL
      AND pause_noticed_parked_at IS NOT parked_at;

-- A turn that stops before it finishes parks as `turn_incomplete`. Rows earlier
-- builds stored spell it `idle`, which nothing else wrote (a turn that
-- finishes parks with no reason), so every `idle` row is relabelled.
UPDATE conversations
SET park_reason = 'turn_incomplete'
WHERE park_reason = 'idle';

-- A person's claim mints the conversation they work the task in. Tasks a
-- person already holds were claimed with no such write, so each open one with
-- no un-ended top-level conversation gets the row the claim would have
-- minted: an interactive conversation on the SDK runtime, manual with the
-- claimant as its creator, team-visible on the local team, and parked with no
-- verdict (status 'open' and parked_at set, with no park reason, completion,
-- or outcome), so it reads as live and nothing drives it until a person sends
-- a message.
INSERT INTO conversations (
    id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
    trigger_type, trigger_id, origin, team_id, visibility, creator_user_id,
    actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
    status, parked_at, park_reason, completed_at, outcome, outcome_reason,
    queued_at)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       t.org_id, 'interactive', 'sdk', t.id, NULL, NULL, NULL,
       'manual', NULL, 'interactive', '00000000-0000-0000-0000-000000000010', 'team', t.claimed_by_user_id,
       NULL, NULL, NULL, NULL,
       'open', CURRENT_TIMESTAMP, NULL, NULL, NULL, NULL,
       NULL
  FROM tasks t
 WHERE t.status NOT IN ('done', 'dismissed')
   AND t.claimed_by_user_id IS NOT NULL
   AND NOT EXISTS (
       SELECT 1 FROM conversations live
        WHERE live.task_id = t.id
          AND live.ended_at IS NULL AND live.parent_conversation_id IS NULL);

-- Every failure and park already on disk was told, or lost, by the build that
-- recorded it. Marking them keeps the upgrade from telling them again.
UPDATE conversations SET failure_noticed_at = COALESCE(completed_at, CURRENT_TIMESTAMP)
 WHERE status = 'failed';
UPDATE conversations SET pause_noticed_parked_at = parked_at
 WHERE parked_at IS NOT NULL;

-- The reasoning effort a person's message runs at, beside the model it runs
-- on (messages.model): one of 'low', 'medium', 'high', 'xhigh' and 'max', NULL
-- for the provider's default. Written on a person's own rows only; the newest
-- one holds the conversation's current settings.
ALTER TABLE messages ADD COLUMN effort TEXT;

-- When a conversation's memory content last changed. The insert stamps it
-- beside created_at, and a write that stores the content the row already holds
-- leaves it alone. It is what tells how much of a conversation has happened
-- since its memory was last written. Backfilled to created_at, the only time an
-- existing row records: its content may have changed after that, so such a row
-- reads as older than it is, never newer.
ALTER TABLE conversation_memory ADD COLUMN updated_at DATETIME;
UPDATE conversation_memory SET updated_at = created_at;

-- ===========================================================================
-- 4. Spend
-- ===========================================================================

-- llm_spend counts an interactive conversation's spend beside a delegation's.
-- A chat is a person's spend, so it is a 'manual' row whatever trigger_type
-- its mint stamped, and it is charged to the row's own user rather than the
-- conversation's creator: a reassignment within the team keeps the
-- conversation, and each turn is the spend of the person who asked for it. A
-- row with no user falls back to the creator. A delegation row keeps the
-- conversation's creator. source stays 'run', the stored value readers key on.
DROP VIEW llm_spend;
CREATE VIEW llm_spend AS
  SELECT 'run' AS source,
         m.id AS source_id,
         m.org_id,
         c.team_id,
         CASE WHEN c.type = 'interactive' OR c.trigger_type = 'manual' THEN 'manual'
              ELSE 'autonomous' END AS category,
         NULL AS subtype,
         CASE WHEN c.type = 'interactive' THEN COALESCE(m.user_id, c.creator_user_id)
              ELSE c.creator_user_id END AS creator_user_id,
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
  WHERE c.type IN ('delegation', 'interactive')
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
