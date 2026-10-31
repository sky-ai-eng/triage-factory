-- +goose Up
-- object_kind and object_key name the object an external action changed, set
-- exactly when the act landed a change and NULL on a refusal, a failure and a
-- catch-all row. The kind's identifying columns are read together
-- (domain.ObjectID): entity_id for a pull request or a GitHub issue,
-- repository_id for a repository, repository_id + object_key for a branch (the
-- ref), a label (its name), a release, a workflow or a workflow run (GitHub's
-- id), and scope + object_key for a Jira or Linear issue, a review, a comment
-- and a Slack message (the object's key). Rows recorded before this migration
-- name no object.
ALTER TABLE external_actions ADD COLUMN object_kind TEXT
    CHECK (object_kind IN (
        'pull_request', 'issue', 'repository', 'branch', 'label', 'release',
        'workflow', 'workflow_run', 'review', 'comment', 'message'));
ALTER TABLE external_actions ADD COLUMN object_key TEXT;

-- The rows that changed one keyed object, found by its key within a scope.
-- TODO(TFAC-1265): nothing reads by it until the tracker's comment check
-- looks TF's own comments up by key.
CREATE INDEX idx_external_actions_object_key ON external_actions (org_id, scope, object_key)
    WHERE object_key IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
