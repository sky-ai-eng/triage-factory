-- +goose Up
-- Per-team daily LLM spend cap (TFAC-482, EE/governance-gated): a nullable
-- per-team dollar ceiling on "today's" spend, the team-scoped sibling of the
-- org-wide cap (org_settings.max_daily_cost_usd, migration …0007). The
-- delegation choke point (Spawner.Delegate) sums the team's spend for the
-- current UTC calendar day over its team_id rows ONLY — system overhead and
-- non-team curator carry a NULL team_id and are never apportioned to a team —
-- and, when the governance entitlement is active, refuses new agent runs for
-- that team once the total is at or above this value. Org-admin-configured (a
-- team admin cannot set their own team's cap). NULL = no cap; the app layer
-- also treats 0 as "no cap". Both caps apply: a run is blocked if the org cap
-- OR the team cap trips. In-flight runs are unaffected and the read is
-- best-effort — a settings/spend read error fails open so a transient failure
-- can't wedge delegation. SQLite is N=1 (one team), so this column is dormant in
-- local mode; it ships for schema parity with the Postgres baseline.
ALTER TABLE team_settings ADD COLUMN max_daily_cost_usd REAL;

-- +goose Down
SELECT 'down not supported';
