-- +goose Up
-- Drop pending_prs: the local-staging PR table is retired in favor of
-- GitHub-native draft PRs recorded as `pull_request` artifacts. The draft PR is
-- a real GitHub object created at `pr create` time (artifact state=draft),
-- edited 1:1 via the GitHub API, promoted to ready-for-review on approval, and
-- closed on abandonment — so the local pending_prs row, its preview gating, and
-- its by-run lookup no longer have a consumer. The agent-drafted title/body now
-- live in the artifact's details_json proposed snapshot; the human verdict is
-- still captured into run_memory at approval. Forward-only per the SQLite
-- (shipped) migration rule — never edit the baseline.
DROP TABLE IF EXISTS pending_prs;

-- +goose Down
SELECT 'down not supported';
