-- +goose Up
-- blueprint_runs.actor_agent_id: the bot that executes a blueprint run, resolved
-- once at the delegation entry point and frozen here at mint (alongside
-- creator_user_id / trigger_id — the "who/why" provenance axes). Every step run
-- inherits it onto runs.actor_agent_id, so the execution attribution is stable
-- across all steps and immune to a later task-claim change: a user takeover
-- clears tasks.claimed_by_agent_id but must not rewrite who ran the bot's steps.
--
-- Plain (not composite) FK with ON DELETE SET NULL, mirroring runs.actor_agent_id
-- in the SQLite baseline: ALTER TABLE ADD COLUMN can't carry a composite FK, and
-- at N=1 there is one org so cross-org leakage is structurally impossible. The
-- column defaults to NULL, which is what SQLite requires to attach a REFERENCES
-- clause via ALTER. Local mode never deletes the bootstrapped agent, so the
-- SET NULL is a safety contract rather than an expected transition.
ALTER TABLE blueprint_runs ADD COLUMN actor_agent_id TEXT REFERENCES agents(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_blueprint_runs_actor_agent ON blueprint_runs(actor_agent_id) WHERE actor_agent_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
