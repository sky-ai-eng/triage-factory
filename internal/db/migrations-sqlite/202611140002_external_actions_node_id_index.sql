-- +goose Up
-- A comment TF posts carries its node id in node_id, and a read that names
-- the comment by node id finds the pull request or issue it is on from that
-- row (ExternalActionStore.PostedCommentEntitySystem). Rows already written
-- are left as recorded: the ledger's record of an act is written once.
CREATE INDEX idx_external_actions_node_id ON external_actions (org_id, scope, node_id)
    WHERE node_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
