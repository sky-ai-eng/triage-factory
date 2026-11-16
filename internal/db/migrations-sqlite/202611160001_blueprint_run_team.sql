-- +goose Up
-- blueprint_runs.team_id: the team a run belongs to, set at mint to the team
-- its first step conversation is created on, and never changed. A task that
-- moves to another team gets a new run there, so the run stays with the team
-- that made it, as its conversations do.
--
-- Nullable, unlike the Postgres twin: SQLite cannot add a NOT NULL column
-- without a default, and a run with no conversation has no team to fill in.
ALTER TABLE blueprint_runs ADD COLUMN team_id TEXT REFERENCES teams(id);

-- Existing runs take the team of their lowest-index step conversation, which
-- is the first step the mint created with the run.
UPDATE blueprint_runs
   SET team_id = (
       SELECT c.team_id FROM conversations c
        WHERE c.blueprint_run_id = blueprint_runs.id
          AND c.blueprint_step_index IS NOT NULL
        ORDER BY c.blueprint_step_index ASC, c.started_at ASC, c.id ASC
        LIMIT 1
   );

-- +goose Down
SELECT 'down not supported';
