-- +goose Up
-- The brain posts a delegated conversation's failure notice and opens its
-- paused notice from these rows, and marks each one it has told.
-- failure_noticed_at is when the failure notice was posted. A conversation
-- fails at most once, so a timestamp is enough. pause_noticed_parked_at is the
-- parked_at of the park whose notice was opened: a park keeps its parked_at
-- until a claim or a resume clears it, so each park has its own value. Only the
-- brain writes either.
ALTER TABLE conversations ADD COLUMN failure_noticed_at DATETIME;
ALTER TABLE conversations ADD COLUMN pause_noticed_parked_at DATETIME;

-- Failed rows accumulate, and the few still owed a notice are all the read
-- takes. Likewise for parks: stored 'open' covers every concluded step, and
-- only the live parks not yet told are read.
CREATE INDEX IF NOT EXISTS idx_conversations_failure_notice_due ON conversations (status, completed_at, id)
    WHERE status = 'failed' AND failure_noticed_at IS NULL AND creator_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_pause_notice_due ON conversations (status, parked_at, id)
    WHERE status = 'open' AND park_reason IS NOT NULL AND parked_at IS NOT NULL
      AND creator_user_id IS NOT NULL AND ended_at IS NULL AND archived_at IS NULL
      AND pause_noticed_parked_at IS NOT parked_at;

-- Every failure and park already on disk was told, or lost, by the build that
-- recorded it. Marking them keeps the upgrade from telling them again.
UPDATE conversations SET failure_noticed_at = COALESCE(completed_at, CURRENT_TIMESTAMP)
 WHERE status = 'failed';
UPDATE conversations SET pause_noticed_parked_at = parked_at
 WHERE parked_at IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
