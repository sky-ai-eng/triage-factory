-- +goose Up
-- The curator (the per-project chat agent) is removed. It was the sole
-- writer of every column and table this migration drops: nothing else ever
-- populated a conversation's project_id, a claim's message_id, or a row in
-- curator_homes, and no shipped seed other than the curator's own
-- ticket-spec / Jira-formatting prompts ever wrote those two system_slugs.

-- Drop the seeded prompt/blueprint pair the curator authored tickets and
-- Jira comments from. Deleting the blueprint cascades to its single step,
-- which clears blueprint_steps.step_prompt_id's ON DELETE RESTRICT hold on
-- the prompt row, so the prompt delete below can proceed. One team may hold
-- zero or one copy of each (prompts_org_team_slug_unique), so this clears
-- every team's copy in one pass.
DELETE FROM blueprints WHERE system_slug IN ('system-ticket-spec', 'system-jira-formatting');
DELETE FROM prompts WHERE system_slug IN ('system-ticket-spec', 'system-jira-formatting');

-- conversations.project_id anchored a curator conversation to its project
-- (knowledge base, homing, cascade-delete). The index must go first — SQLite
-- refuses to drop a column an index still references.
DROP INDEX idx_conversations_project;
ALTER TABLE conversations DROP COLUMN project_id;

-- claims.message_id was the queued turn a curator pickup was minted to
-- drive, stamped so a failed (zero-message) claim stayed attributable to its
-- exact turn.
ALTER TABLE claims DROP COLUMN message_id;

-- curator_homes: the durable (org, project) -> home executor map that kept a
-- curator session's sticky worktree cache on one executor at N>1.
DROP TABLE curator_homes;

-- llm_spend, re-keyed onto the one surviving agent type: every row is now a
-- 'run' (delegation), split into 'manual'/'autonomous' by trigger_type; the
-- 'curator' source and category values no longer occur.
DROP VIEW llm_spend;
CREATE VIEW llm_spend AS
  SELECT 'run' AS source,
         m.id AS source_id,
         m.org_id,
         c.team_id,
         CASE WHEN c.trigger_type = 'manual' THEN 'manual'
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
  WHERE c.type = 'delegation'
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
