-- +goose Up
ALTER TABLE conversations ADD COLUMN stop_requested_at DATETIME;
ALTER TABLE conversations ADD COLUMN stop_requested_by TEXT;
CREATE INDEX idx_conversations_stop_requested ON conversations (id) WHERE stop_requested_at IS NOT NULL;
-- +goose Down
SELECT 'down not supported';
