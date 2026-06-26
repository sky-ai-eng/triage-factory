-- +goose Up
-- Org-wide daily LLM spend cap (TFAC-477): a nullable per-org dollar ceiling on
-- "today's" total LLM spend. The delegation choke point (Spawner.Delegate) sums
-- the org's spend for the current UTC calendar day across EVERY category
-- (autonomous + manual + curator + system overhead) and refuses all new agent
-- runs — manual and autonomous alike — once that total is at or above this
-- value. A runaway-spend fuse, most relevant for a misconfigured autonomous
-- trigger looping. NULL = no cap; the app layer also treats 0 as "no cap".
-- Mirrors the existing nullable max_llm_model_tier shape on this row. In-flight
-- runs are unaffected (the cap blocks new spawns only) and the read is
-- best-effort — a settings/spend read error fails open so a transient failure
-- can't wedge all delegation.
ALTER TABLE org_settings ADD COLUMN max_daily_cost_usd REAL;

-- +goose Down
SELECT 'down not supported';
