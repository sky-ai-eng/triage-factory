-- +goose Up
-- artifacts.node_id: the backing object's GraphQL node id. GitHub comment rows
-- carry it, because a GraphQL delete names a comment by node id alone and has
-- to find the comment's row without its number
-- (ArtifactStore.GetByNodeIDSystem, which idx_artifacts_node_id serves). NULL
-- when no writer learned one.
ALTER TABLE artifacts ADD COLUMN node_id TEXT;
CREATE INDEX idx_artifacts_node_id ON artifacts (org_id, scope, node_id) WHERE node_id IS NOT NULL;

-- A comment row exists only for a comment TF posted, so a reader tells TF's
-- comments from people's by whether a row exists. Rows written before this
-- migration break that for GitHub: an edit or a delete of a comment TF had not
-- posted also left a row. The posting verb always stamped the row's target
-- (owner/repo#N), and the edit and delete verbs never did, and the store never
-- lets an empty target overwrite a stamped one, so a GitHub comment row with no
-- target is one of those. The audit ledger keeps a row for each such edit and
-- delete.
DELETE FROM artifacts
 WHERE provider = 'github' AND kind = 'comment' AND target = '';

-- +goose Down
SELECT 'down not supported';
