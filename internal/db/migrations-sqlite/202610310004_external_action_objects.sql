-- +goose Up
-- object_kind and object_key name the object an external action changed, set
-- exactly when the act landed a change and NULL on a refusal, a failure and a
-- catch-all row. The kind's identifying columns are read together
-- (domain.ObjectID): entity_id for a pull request or a GitHub issue,
-- repository_id for a repository, repository_id + object_key for a branch (the
-- ref), a label (its name), a release, a workflow or a workflow run (GitHub's
-- id), and scope + object_key for a Jira or Linear issue, a review, a comment
-- and a Slack message (the object's key). Written once, like the rest of the
-- record.
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

-- Existing rows name their object where the row, or a deliverable written
-- beside it, already holds the identity. Every other row stays NULL.

-- An act on an object the row's repository and external_id identify: a
-- branch's ref, a label's name, GitHub's release, workflow or workflow run id.
UPDATE external_actions
   SET object_kind = CASE
         WHEN action IN ('branch_pushed', 'branch_deleted', 'linked_branch_created') THEN 'branch'
         WHEN action IN ('label_defined', 'label_definition_edited', 'label_definition_deleted') THEN 'label'
         WHEN action IN ('release_created', 'release_edited', 'release_deleted') THEN 'release'
         WHEN action = 'workflow_dispatched' THEN 'workflow'
         ELSE 'workflow_run' END,
       object_key = external_id
 WHERE action IN (
        'branch_pushed', 'branch_deleted', 'linked_branch_created',
        'label_defined', 'label_definition_edited', 'label_definition_deleted',
        'release_created', 'release_edited', 'release_deleted',
        'workflow_dispatched', 'workflow_run_cancelled')
   AND repository_id IS NOT NULL
   AND external_id IS NOT NULL AND external_id <> '';

-- A GitHub pull request or issue act about the entity the row references.
-- GitHub numbers the two in one sequence and serves both through the issue
-- endpoints, so the kind is the entity's, not the action's. A reaction is left
-- out: its row does not say whether it landed on the object or on one of its
-- comments.
UPDATE external_actions
   SET object_kind = CASE (SELECT e.kind FROM entities e WHERE e.id = external_actions.entity_id)
                       WHEN 'pr' THEN 'pull_request'
                       WHEN 'issue' THEN 'issue' END
 WHERE provider = 'github'
   AND action IN (
        'pr_created', 'pr_marked_ready', 'pr_converted_to_draft', 'pr_edited',
        'pr_closed', 'pr_reopened', 'pr_merged', 'pr_auto_merge_enabled',
        'pr_auto_merge_disabled', 'pr_branch_updated', 'review_requested',
        'review_request_removed', 'label_added', 'label_removed',
        'conversation_locked', 'conversation_unlocked',
        'issue_created', 'issue_updated', 'issue_closed', 'issue_reopened',
        'issue_deleted', 'issue_pinned', 'issue_unpinned', 'issue_transferred')
   AND entity_id IS NOT NULL;

-- An act on the repository the row references.
UPDATE external_actions
   SET object_kind = 'repository'
 WHERE action IN ('repo_edited', 'repo_deleted', 'repo_forked', 'repo_archived', 'repo_unarchived')
   AND repository_id IS NOT NULL;

-- A submitted or dismissed review: GitHub's review id is the row's
-- external_id. A submit whose response carried no id recorded 0, which names
-- no review.
UPDATE external_actions
   SET object_kind = 'review', object_key = 'github:review:' || external_id
 WHERE action IN ('review_submitted', 'review_dismissed')
   AND provider = 'github'
   AND external_id GLOB '[1-9]*' AND external_id NOT GLOB '*[^0-9]*';

-- A Jira or Linear issue or comment act, a GitHub comment act, and a Slack
-- message act: the key of the deliverable the same conversation recorded for
-- the same object, which is the object's key. Where more than one deliverable
-- matches, the row stays NULL rather than take one at random. A Jira key that
-- names its issue by the issue's key rather than its site and id (it has no
-- '/') is not copied: a freed key can later name another issue. A GitHub
-- comment's number is unique only within its numbering, so an act whose action
-- names the numbering copies only that numbering's key.
UPDATE external_actions
   SET object_key = (
        SELECT CASE WHEN COUNT(DISTINCT d.dedup_key) = 1 THEN MIN(d.dedup_key) END
          FROM deliverables d
         WHERE d.org_id = external_actions.org_id
           AND d.scope = external_actions.scope
           AND d.provider = external_actions.provider
           AND d.external_id = external_actions.external_id
           AND d.conversation_id = external_actions.conversation_id
           AND d.kind = CASE
                 WHEN external_actions.action IN ('issue_created', 'issue_transitioned', 'issue_assigned', 'issue_updated')
                 THEN 'issue'
                 WHEN external_actions.provider = 'slack' THEN 'message'
                 ELSE 'comment' END
           AND (d.provider <> 'jira' OR instr(d.dedup_key, '/') > 0)
           AND CASE external_actions.action
                 WHEN 'comment_edited' THEN d.dedup_key = 'github:comment:' || external_actions.external_id
                 WHEN 'comment_deleted' THEN d.dedup_key = 'github:comment:' || external_actions.external_id
                 WHEN 'review_comment_edited' THEN d.dedup_key = 'github:comment:review:' || external_actions.external_id
                 WHEN 'review_comment_deleted' THEN d.dedup_key = 'github:comment:review:' || external_actions.external_id
                 ELSE 1 END)
 WHERE object_kind IS NULL
   AND external_id IS NOT NULL AND external_id <> ''
   AND conversation_id IS NOT NULL
   AND ((provider IN ('jira', 'linear')
         AND action IN ('issue_created', 'issue_transitioned', 'issue_assigned', 'issue_updated', 'issue_comment_posted'))
     OR (provider = 'github'
         AND action IN ('comment_posted', 'comment_edited', 'comment_deleted', 'review_comment_edited', 'review_comment_deleted'))
     OR (provider = 'slack'
         AND action IN ('slack_message_posted', 'slack_message_edited', 'slack_reaction_added')));

UPDATE external_actions
   SET object_kind = CASE
         WHEN action IN ('issue_created', 'issue_transitioned', 'issue_assigned', 'issue_updated') THEN 'issue'
         WHEN provider = 'slack' THEN 'message'
         ELSE 'comment' END
 WHERE object_kind IS NULL AND object_key IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
