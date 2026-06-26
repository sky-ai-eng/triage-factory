-- +goose Up
-- Curator spend team attribution (TFAC-476): denormalize a nullable team_id onto
-- curator_requests, snapshotted from the project at creation (point-in-time,
-- mirroring runs.team_id), and flip the llm_spend view's curator arm to read it.
-- A Curator session is tied to a project and projects are team-scoped, so curator
-- spend can attribute to the project's team instead of always reading org-level.
-- projects.team_id is nullable (only 'team' visibility is CHECK-forced to carry a
-- team), so curator team_id is correctly nullable too: team project ->
-- team-attributed; private/org project -> NULL (still creator- and org-visible,
-- just absent from team dashboards — correct, they aren't team work). Forward-only:
-- the shipped baseline curator_requests (202605130001) has no team_id.

ALTER TABLE curator_requests ADD COLUMN team_id TEXT;

-- Backfill existing rows from their project's current team. Point-in-time is the
-- best we can do retroactively; going forward the INSERT subquery captures the
-- team at creation. A project with a NULL team (private/org) leaves the row NULL.
UPDATE curator_requests
   SET team_id = (SELECT team_id FROM projects WHERE projects.id = curator_requests.project_id)
 WHERE team_id IS NULL;

-- Re-emit llm_spend verbatim from 202606260004 EXCEPT the curator arm's team_id,
-- which now reads curator_requests.team_id instead of a hardcoded NULL. SQLite
-- can't ALTER a view in place, so drop + recreate. (runs + system arms unchanged;
-- system stays org-level NULL.) See TFAC-476 / TFAC-472.
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
