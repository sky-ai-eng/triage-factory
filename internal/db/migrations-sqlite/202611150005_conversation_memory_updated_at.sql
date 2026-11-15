-- +goose Up
-- When a conversation's memory content last changed. The insert stamps it
-- beside created_at, and a write that stores the content the row already holds
-- leaves it alone. It is what tells how much of a conversation has happened
-- since its memory was last written. Backfilled to created_at, the only time an
-- existing row records. Its content may have changed after that, so such a row
-- reads as older than it is: a conversation holding one is asked to update its
-- memory sooner, never later.
ALTER TABLE conversation_memory ADD COLUMN updated_at DATETIME;
UPDATE conversation_memory SET updated_at = created_at;

-- +goose Down
SELECT 'down not supported';
