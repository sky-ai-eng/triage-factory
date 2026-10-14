-- +goose Up
-- artifacts.node_id: the backing object's GraphQL node id. GitHub comment rows
-- carry it, because a GraphQL edit or delete names a comment by node id alone
-- and has to find the comment's row without its number
-- (ArtifactStore.GetByNodeIDSystem, which idx_artifacts_node_id serves). NULL
-- when no writer learned one.
ALTER TABLE artifacts ADD COLUMN node_id TEXT;
CREATE INDEX idx_artifacts_node_id ON artifacts (org_id, scope, node_id) WHERE node_id IS NOT NULL;

-- artifacts.posted_by_tf: the comment this row records was posted by TF. A run
-- that edits or deletes someone else's comment also leaves a row, so a row
-- existing is not enough to call the comment TF's. Mirrors the Postgres
-- baseline's column.
ALTER TABLE artifacts ADD COLUMN posted_by_tf BOOLEAN NOT NULL DEFAULT 0;

-- The backfill holds only for the rows this migration finds, all written before
-- the column existed. Each Jira and Linear comment row among them was written
-- by the verb that posts the comment. A GitHub comment row among them was
-- posted by TF exactly when it has a target: the posting verb stamped
-- owner/repo#N, and the edit and delete verbs, which also mint a row for a
-- comment someone else wrote, left the target empty, which the store never lets
-- overwrite a stamped one. On a row written after this migration a target says
-- nothing about who posted the comment; posted_by_tf does.
UPDATE artifacts SET posted_by_tf = 1
 WHERE kind = 'comment'
   AND (provider IN ('jira', 'linear') OR (provider = 'github' AND target <> ''));

-- +goose Down
SELECT 'down not supported';
