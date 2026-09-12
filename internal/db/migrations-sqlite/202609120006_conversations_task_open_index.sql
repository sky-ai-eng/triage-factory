-- +goose Up
-- The claim gate's task clause: a conversation is drivable only while it is
-- its task's live one — the newest row on the task that has not ended. The
-- workspace is keyed by the task, so two driven at once is two agents in one
-- git tree; the predicate states what storage already forces.
--
-- The complement of idx_conversations_task_ended, and ordered where that one
-- is not: the boundary anti-join reads an exclusion set, while this is a
-- LIMIT 1 lookup every candidate row of the hot claim scan runs.
--
-- The key carries the lookup's WHOLE sort order, tiebreak included. Stopping at
-- started_at leaves a sort over each tie group to settle id, which is
-- per-candidate work on that hot scan; with id in the key the lookup reads one
-- index entry, and id last also covers the subquery, which selects exactly it.
CREATE INDEX idx_conversations_task_open ON conversations (task_id, started_at DESC, id DESC) WHERE ended_at IS NULL;

-- +goose Down
SELECT 'down not supported';
