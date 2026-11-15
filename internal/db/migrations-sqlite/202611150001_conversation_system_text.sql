-- +goose Up
-- A conversation keeps the system text its first claim sent: the shared
-- framework prompt beside the conversation's own block, and the parts both
-- were composed from, as a JSON array of {key, name, text}. A NULL
-- system_parts is a conversation whose text was never recorded.
ALTER TABLE conversations ADD COLUMN system_prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN system_parts TEXT;

-- +goose Down
SELECT 'down not supported';
