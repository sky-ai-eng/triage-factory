-- +goose Up
-- Unified spend view (TFAC-472): one row-per-spend shape UNION-ing the three
-- source tables — delegated runs, curator turns, and headless system jobs —
-- onto a single category axis so the team dashboard + safety cap read from one
-- place and totals reconcile with the Anthropic bill. Read-side only: no
-- normalized table, no write-path change, no backfill — the view IS the
-- abstraction boundary (materialize later only if it's ever measured too slow).
-- The first VIEW in the SQLite tree; plain SQL, goose handles it.
--
-- Follows TFAC-473's 202606260003_run_curator_token_breakdown.sql, which added
-- the four token columns (input/output/cache_read/cache_creation, INTEGER NOT
-- NULL DEFAULT 0) to runs + curator_requests — mirroring system_llm_runs
-- (TFAC-451). So every token column is read NATIVELY: the view does NO COALESCE
-- on tokens; only runs.total_cost_usd (nullable until a terminal write) is
-- wrapped in COALESCE(…,0).
--
-- category derivation: runs split on trigger_type (manual → per-user, anything
-- else → autonomous); curator is 'curator'; system jobs are 'system_overhead'
-- with the job (scorer/repo_profiler/classifier) carried as subtype.
--
-- team_id: runs are team-scoped; system-overhead has no team (org-level) and
-- curator is org-level in v1 — so curator/system surface a NULL team_id.
-- actor_agent_id is the org agent that executed the run (audit passthrough);
-- only runs are agent-executed → NULL for curator + system.
--
-- No security_invoker — SQLite is local mode (N=1), no RLS. occurred_at is
-- runs.started_at / curator_requests.created_at / system_llm_runs.started_at.
--
-- The view reflects *settled* spend: in-flight (non-terminal) rows show 0 cost
-- (COALESCE) + 0 tokens (the columns stay 0 until a terminal write), and every
-- terminal write — completion, cancel, infra-failure, and the boot-time orphan
-- sweeps — rolls the token breakdown up from the per-message tables, so a
-- cancelled/failed run or curator turn still carries its real token spend.
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
         NULL AS team_id,
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
