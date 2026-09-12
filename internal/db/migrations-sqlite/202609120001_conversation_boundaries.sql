-- +goose Up
-- A task owns its conversations, and until now nothing recorded the moment one
-- stopped being the live one. A requeued or taken-over conversation stayed
-- parked `open` and resumable; a blueprint's step N stayed `completed` with
-- nothing saying N+1 superseded it.
--
-- ended_at is that moment and ended_reason is why (domain.EndedReason:
-- requeued | delegated | taken_over | step_advanced | team_archived | failed).
-- Both NULL while the conversation is live, and they move together.
-- App-validated at the store door, no CHECK — the type/origin pattern.
--
-- Orthogonal to status: an ended conversation may be completed, failed or
-- parked open, because what ended it is the task moving on rather than the
-- transcript finishing.
ALTER TABLE conversations ADD COLUMN ended_at DATETIME;
ALTER TABLE conversations ADD COLUMN ended_reason TEXT;

-- No backfill, deliberately. A conversation that ended before this shipped
-- ended for a reason nothing recorded, and a guessed ended_reason is a wrong
-- answer printed to a person later. Every predicate built on these columns is
-- written as "the newest non-ended conversation on the task", so a legacy task
-- carrying several un-ended rows still behaves: the newest is the live one.

-- The boundary anti-join: "the task's conversations that have already ended".
-- Partial on the stamped arm because a live task has none of them and the
-- reads that consult it are asking which rows to exclude.
CREATE INDEX idx_conversations_task_ended ON conversations (task_id) WHERE ended_at IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
