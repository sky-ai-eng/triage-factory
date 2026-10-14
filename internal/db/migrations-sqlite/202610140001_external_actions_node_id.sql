-- +goose Up
-- external_actions.node_id: the GraphQL node id of the object an action
-- created. The create verb stamps it on its pr_created row, and the
-- reconciler's PR backstop matches that row to the pull request's artifact by
-- it (artifacts.node_id), because a node id survives a repository rename and
-- the row's target does not. Part of the record of the act: written once,
-- never updated. NULL on every other action, and on the rows written before
-- this migration.
ALTER TABLE external_actions ADD COLUMN node_id TEXT;

-- The backstop's read (ExternalActionStore.ListPRCreatedWithoutArtifactSystem)
-- runs every reconcile cycle and wants one org's pr_created rows from a recent
-- window. idx_external_actions_org_occurred narrows on org and time but reads
-- every action in the window; this one reads only the pr_created rows.
CREATE INDEX idx_external_actions_org_action_occurred ON external_actions (org_id, action, occurred_at);

-- +goose Down
SELECT 'down not supported';
