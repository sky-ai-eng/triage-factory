-- +goose Up
-- The claim gate's task clause: a conversation is drivable only while it is
-- its task's live one — the newest row on the task that has not ended. The
-- workspace is keyed by the task, so two driven at once is two agents in one
-- git tree; the predicate states what storage already forces.
--
-- The complement of idx_conversations_task_ended, and ordered where that one
-- is not: the boundary anti-join reads an exclusion set, while this is a
-- LIMIT 1 lookup every candidate row of the hot claim scan runs.
CREATE INDEX idx_conversations_task_open ON conversations (task_id, started_at DESC) WHERE ended_at IS NULL;

-- +goose Down
SELECT 'down not supported';
