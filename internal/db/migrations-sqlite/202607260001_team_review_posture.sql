-- +goose Up
-- Per-team review-posting posture (TFAC-680): whether a delegated agent's
-- finalized review is staged for human approval or submitted to GitHub on
-- finalize. 'identity' (the default) decides from the acting credential — an
-- App installation posts as a bot and goes direct, a borrowed PAT posts as the
-- lending user and stages. 'draft' always stages, 'auto' always submits, and
-- 'auto_unless_blocking' stages only a consequential review (REQUEST_CHANGES,
-- or any BLOCKER-severity inline comment). NOT NULL with a literal DEFAULT so
-- existing rows and partial upserts (e.g. SetDailyCostCapSystem) materialize
-- the default without the writer naming the column. The value set is validated
-- app-side, not by a CHECK — the MaxLLMModelTier precedent.
ALTER TABLE team_settings ADD COLUMN review_posture TEXT NOT NULL DEFAULT 'identity';

-- +goose Down
SELECT 'down not supported';
