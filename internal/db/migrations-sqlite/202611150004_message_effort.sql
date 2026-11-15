-- +goose Up
-- The reasoning effort a person's message runs at, beside the model it runs
-- on (messages.model): one of 'low', 'medium', 'high', 'xhigh' and 'max', NULL
-- for the provider's default. Written on a person's own rows only; the newest
-- one holds the conversation's current settings.
ALTER TABLE messages ADD COLUMN effort TEXT;

-- +goose Down
SELECT 'down not supported';
