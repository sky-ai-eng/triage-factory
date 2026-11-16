-- +goose Up
-- A stop that ends with no turn run takes the person messages in front of it
-- out of the wake predicate, and these two columns are where that position is
-- kept. Both hold a messages id: a message's created_at is written by the
-- process that inserted it, and the claim predicates never compare one
-- process's clock with another's.
--
-- stop_through_message_id is the conversation's newest message id when the
-- stop was requested. It belongs to the request: set with stop_requested_at,
-- cleared wherever it is cleared.
--
-- wake_after_message_id: a person's undelivered message at or before this id
-- does not wake the conversation. The park that settles a stop with no turn
-- run moves it forward to the stop's stop_through_message_id; nothing moves it
-- back or clears it. NULL lets every undelivered person message wake it.
ALTER TABLE conversations ADD COLUMN stop_through_message_id INTEGER;
ALTER TABLE conversations ADD COLUMN wake_after_message_id INTEGER;

-- +goose Down
SELECT 'down not supported';
