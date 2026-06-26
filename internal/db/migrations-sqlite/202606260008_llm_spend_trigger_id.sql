-- +goose Up
-- Expose runs.trigger_id on the llm_spend view (TFAC-478) so the team usage
-- dashboard can attribute autonomous spend to the rule/trigger that fired it
-- (the by-rule breakdown). Read-side only: no table change, no backfill — the
-- runs.trigger_id column already exists (the baseline runs table). SQLite can't
-- ALTER a view in place, so drop + recreate.
--
-- Re-emit llm_spend verbatim from 202606260006 (which carries TFAC-476's curator
-- team_id) EXCEPT for a new trigger_id column placed after actor_agent_id and
-- before model: the runs arm reads runs.trigger_id; the curator + system arms
-- emit NULL (only delegated runs are trigger-fired). The curator arm keeps
-- reading curator_requests.team_id from 476; the system arm stays org-level
-- NULL team. Column order is kept identical to the Postgres baseline view and to
-- spendSelectCols in both store impls. See TFAC-478 / TFAC-476 / TFAC-472.
DROP VIEW IF EXISTS llm_spend;
CREATE VIEW llm_spend AS
  SELECT 'run' AS source,
         id AS source_id,
         org_id,
         team_id,
         CASE trigger_type WHEN 'manual' THEN 'manual' ELSE 'autonomous' END AS category,
         NULL AS subtype,
         creator_user_id,
         actor_agent_id,
         trigger_id,
         model,
         COALESCE(total_cost_usd, 0) AS total_cost_usd,
         input_tokens,
         output_tokens,
         cache_read_tokens,
         cache_creation_tokens,
         started_at AS occurred_at
  FROM runs
  UNION ALL
  SELECT 'curator' AS source,
         id AS source_id,
         org_id,
         team_id,
         'curator' AS category,
         NULL AS subtype,
         creator_user_id,
         NULL AS actor_agent_id,
         NULL AS trigger_id,
         NULL AS model,
         cost_usd AS total_cost_usd,
         input_tokens,
         output_tokens,
         cache_read_tokens,
         cache_creation_tokens,
         created_at AS occurred_at
  FROM curator_requests
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
