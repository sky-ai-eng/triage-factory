-- +goose Up
-- Ledger rows recorded before object_kind and object_key existed name no
-- object, so their runs show no artifact for what they changed, and their
-- pull request and review deliverables match none. A pull request's rows and
-- a review's carry the object's identity in their own columns, so those name
-- it here. No other kind's row carries its identity, and those rows stay as
-- they are.

-- A pull request is identified by its entity.
UPDATE external_actions
   SET object_kind = 'pull_request'
 WHERE object_kind IS NULL
   AND provider = 'github'
   AND entity_id IS NOT NULL
   AND action IN ('pr_created', 'pr_marked_ready', 'pr_converted_to_draft', 'pr_edited', 'pr_closed',
                  'pr_reopened', 'pr_merged', 'pr_auto_merge_enabled', 'pr_auto_merge_disabled',
                  'pr_branch_updated', 'review_requested', 'review_request_removed');

-- A submitted review is identified by GitHub's review id, the row's
-- external_id, in the key domain.GitHubReviewObjectKey builds. An id that is
-- not a positive integer (a submit whose response carried none) names no
-- review.
UPDATE external_actions
   SET object_kind = 'review',
       object_key = 'github:review:' || external_id
 WHERE object_kind IS NULL
   AND provider = 'github'
   AND action IN ('review_submitted', 'review_dismissed')
   AND external_id GLOB '[1-9]*'
   AND external_id NOT GLOB '*[^0-9]*';

-- +goose Down
SELECT 'down not supported';
