-- +goose Up
-- The post-push check's work: the org's signatures whose pushed commit GitHub
-- has not yet been asked about, newest push first. Zero rows between cycles is
-- the steady state.
CREATE INDEX commit_signatures_unchecked_push_idx ON commit_signatures (org_id, pushed_at) WHERE pushed_at IS NOT NULL AND checked_at IS NULL;

-- +goose Down
SELECT 'down not supported';
