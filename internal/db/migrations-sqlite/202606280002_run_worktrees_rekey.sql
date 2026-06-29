-- +goose Up
-- Re-key run_worktrees to (run_id, repo_id, ref) so one run can materialize
-- several worktrees in a single repo (e.g. two PRs reviewed in one interactive
-- run), and drop the vestigial feature_branch column (the push gate reads the
-- worktree's live current branch, never this row — TFAC-498/TFAC-502).
--
-- run_worktrees is ephemeral run-coordination state: every row is
-- REFERENCES runs(id) ON DELETE CASCADE, it is a leaf (only
-- idx_run_worktrees_run references it), orphan-swept on startup, and
-- re-materialized on demand. So this is a destructive drop+recreate — no
-- data is preserved, no temp-table copy, no PRAGMA foreign_keys dance.
DROP TABLE IF EXISTS run_worktrees;
CREATE TABLE run_worktrees (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    repo_id    TEXT NOT NULL,
    path       TEXT NOT NULL,
    ref        TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    org_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    PRIMARY KEY (run_id, repo_id, ref)
);
CREATE INDEX idx_run_worktrees_run ON run_worktrees(run_id);

-- +goose Down
SELECT 'down not supported';
