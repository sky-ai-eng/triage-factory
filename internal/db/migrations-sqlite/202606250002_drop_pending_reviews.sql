-- +goose Up
-- Drop pending_reviews + pending_review_comments: the local-staging review
-- tables are retired in favor of GitHub-native *pending reviews* (TFAC-463). The
-- review is now a real GitHub pending review created at start-review, edited 1:1
-- via the GitHub API (per-comment add/edit/delete live; body + event staged), and
-- submitted to GitHub on approval — recorded as a `review` artifact whose
-- details_json carries the staged body/event + the agent's proposed snapshot. So
-- the local pending_reviews row, its comment children (severity now lives baked
-- into the GitHub comment body, not a column), the preview-mode submit gate, and
-- the by-run lookup no longer have a consumer. Child table first to satisfy the
-- FK. Forward-only per the SQLite (shipped) migration rule — never edit the baseline.
DROP TABLE IF EXISTS pending_review_comments;
DROP TABLE IF EXISTS pending_reviews;

-- +goose Down
SELECT 'down not supported';
