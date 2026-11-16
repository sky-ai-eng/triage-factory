-- +goose Up
-- conversations.sdk_session_cost_usd: the running total the SDK last reported
-- for the session in sdk_session_id. Claude Code restores a session's total on
-- --resume and reports it as every result's total_cost_usd, so a resumed
-- process's own spend is what it reports less this.
ALTER TABLE conversations ADD COLUMN sdk_session_cost_usd REAL NOT NULL DEFAULT 0;

-- Stored spend records each SDK engagement on a session at the session's
-- running total, so a conversation's newest cost row holds the total its
-- session had reached. A session that can still be resumed starts from it;
-- without it, the session's next engagement would record the whole total
-- again. Ended conversations, and ones with no cost row, keep 0.
UPDATE conversations
   SET sdk_session_cost_usd = COALESCE((
       SELECT m.cost_usd FROM messages m
        WHERE m.conversation_id = conversations.id
          AND m.cost_usd IS NOT NULL
        ORDER BY m.id DESC
        LIMIT 1
   ), 0)
 WHERE COALESCE(sdk_session_id, '') <> ''
   AND ended_at IS NULL;

-- +goose Down
SELECT 'down not supported';
