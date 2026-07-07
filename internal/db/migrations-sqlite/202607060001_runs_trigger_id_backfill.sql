-- +goose Up
-- Backfill runs.trigger_id from the parent blueprint_run. Since the blueprint
-- orchestrator unification, trigger provenance is frozen on
-- blueprint_runs.trigger_id at mint, but the per-step runs rows minted by
-- enqueueBlueprintStep never carried it — so every event-fired run landed in
-- llm_spend as category='autonomous' with trigger_id NULL: counted by the
-- usage page's by-category split yet invisible to the by-rule breakdown
-- ("no automated runs" under a non-zero Automated share).
--
-- The write path now denormalizes the blueprint_run's trigger_id onto every
-- step run (mirroring actor_agent_id inheritance); this one-time UPDATE heals
-- the rows minted before that fix. Deliberately a column backfill, NOT a view
-- JOIN: llm_spend stays JOIN-free by design (see the curator team_id
-- denormalization in 202606260006 — same pattern, and load-bearing for the
-- security_invoker Postgres twin). No view change; the runs arm already reads
-- runs.trigger_id (202606260008).
--
-- FK-safe: blueprint_runs.trigger_id itself references event_handlers(id), so
-- every copied value satisfies runs' identical FK. Manual blueprint_runs carry
-- a NULL trigger_id and are excluded by the EXISTS guard, so manual step runs
-- stay NULL (they are 'manual' in llm_spend and never enter the by-rule
-- breakdown anyway).
UPDATE runs
   SET trigger_id = (SELECT br.trigger_id
                       FROM blueprint_runs br
                      WHERE br.id = runs.blueprint_run_id)
 WHERE runs.trigger_id IS NULL
   AND runs.blueprint_run_id IS NOT NULL
   AND EXISTS (SELECT 1
                 FROM blueprint_runs br
                WHERE br.id = runs.blueprint_run_id
                  AND br.trigger_id IS NOT NULL);

-- +goose Down
SELECT 'down not supported';
