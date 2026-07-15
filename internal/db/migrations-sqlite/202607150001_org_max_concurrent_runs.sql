-- +goose Up
-- Per-org concurrent-runs cap: a nullable ceiling on how many runs an org may
-- have executing at once across the executor fleet. NULL = unlimited (the
-- claim also treats <= 0 as unlimited, mirroring max_daily_cost_usd's "0 = no
-- cap"). Enforced in the Postgres ClaimNextRun — a queued run is invisible to
-- claims while its org's active-run count (status running/awaiting_credentials)
-- is at or above this value — so one tenant's burst can't monopolize the
-- fleet's slots, the admission ceiling the daily *spend* cap can't express.
--
-- Lands here for schema + store-interface symmetry across dialects, the same
-- rule preferred_executor_id / placement_overrides followed: SQLite/local is
-- N=1 (one org, one executor) and its claim keeps the global-oldest form,
-- ignoring this column exactly as it ignores placement — one org trivially
-- wins every fairness comparison and a single-executor semaphore already bounds
-- local concurrency. The multi-executor fairness + cap claim is Postgres-only.
ALTER TABLE org_settings ADD COLUMN max_concurrent_runs INTEGER;

-- +goose Down
SELECT 'down not supported';
