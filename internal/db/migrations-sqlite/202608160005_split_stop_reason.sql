-- +goose Up
-- conversations.stop_reason stored two unrelated facts, depending on which
-- writer touched the row last: a park recorded WHY IT WAS PARKED, and the
-- terminal write recorded the MODEL's stop reason straight through from the
-- runtime's result event. They are split here — one per scope, each under the
-- name of the fact it holds.

-- 1. The park half keeps the column and gets the honest name.
ALTER TABLE conversations RENAME COLUMN stop_reason TO park_reason;

-- Any value that isn't in the domain.ParkReason vocabulary is a model stop
-- reason the terminal write left behind ('end_turn', 'max_tokens', 'tool_use',
-- …). Cleared rather than carried over: a row reading `end_turn` under a
-- column called park_reason is a new lie, and the run station renders this
-- value to a person.
--
-- Matched as an exclusion list rather than by naming the model's spellings —
-- the provider vocabulary is open (a new stop reason ships whenever a model
-- does) and this one is closed, so the closed set is the only side that can
-- be enumerated correctly.
UPDATE conversations
   SET park_reason = NULL
 WHERE park_reason IS NOT NULL
   AND park_reason NOT IN ('idle', 'user_cancelled', 'system_cancelled',
                           'blueprint_cancelled', 'blueprint_terminal',
                           'launch_failed', 'model_not_enabled', 'drained');

-- 2. The model half moves to the row it describes. `end_turn` / `max_tokens` /
-- `tool_use` are facts about ONE assistant turn, so at conversation scope the
-- last terminal write won over every turn before it. Beside the token and cost
-- columns, which are the same kind of per-call measurement.
--
-- Nullable, no default, no backfill: the per-turn value was never stored, so
-- historical rows genuinely have none. NULL is "not reported", never a reason
-- of its own.
ALTER TABLE messages ADD COLUMN stop_reason TEXT;

-- +goose Down
SELECT 'down not supported';
