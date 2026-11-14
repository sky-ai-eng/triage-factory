-- +goose Up
-- llm_spend counts an interactive conversation's spend beside a delegation's.
-- A chat is a person's spend, so it is a 'manual' row whatever trigger_type
-- its mint stamped. Its creator is the person whose message started the turn
-- (messages.user_id stamps a turn's rows), so after a reassign that keeps the
-- conversation each turn lands on the person who asked for it; a delegation
-- row keeps the conversation's creator. source stays 'run', the stored value
-- readers key on.
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
