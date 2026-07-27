-- +goose Up
-- Per-team base-branch push policy: whether a delegated agent may push to a
-- repo's base / default branch (main, master, the profile's default branch,
-- the configured base branch). 'never' (the default) refuses every such push,
-- 'manual_only' permits it on a human-dispatched run and refuses it on an
-- event-triggered one, 'always' permits it. A safety guard against a mistaken
-- agent — enforced at the per-run git proxy's ref gate in multi mode and at
-- the TF-controlled pre-push hook in local mode, where the agent runs as the
-- operator and could bypass it. NOT NULL with a literal DEFAULT so existing
-- rows and partial upserts (e.g. SetDailyCostCapSystem) materialize the
-- default without the writer naming the column. The value set is validated
-- app-side, not by a CHECK — the review_posture / MaxLLMModelTier precedent
-- (SQLite cannot add a CHECK without a whole-table rebuild).
ALTER TABLE team_settings ADD COLUMN base_branch_push_policy TEXT NOT NULL DEFAULT 'never';

-- +goose Down
SELECT 'down not supported';
