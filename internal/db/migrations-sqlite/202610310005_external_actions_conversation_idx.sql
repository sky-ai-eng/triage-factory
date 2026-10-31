-- +goose Up
-- A conversation's rows in one org, in time order: the conversation's action
-- list and its artifact reads filter on both columns. The single-column
-- conversation index serves the conversation foreign key's ON DELETE SET NULL.
CREATE INDEX idx_external_actions_org_conversation_occurred
    ON external_actions (org_id, conversation_id, occurred_at);

-- +goose Down
SELECT 'down not supported';
