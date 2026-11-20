-- +goose Up
-- A conversation keeps the system prompt its first claim sent. system_block
-- already holds the conversation's own block; system_prompt holds the shared
-- framework prompt beside it, and system_parts the parts both were composed
-- from (domain.SystemParts, as JSON), which a later claim compares its own
-- composition with. Every later claim sends the two blocks back unchanged,
-- because the provider binds each thinking block to the system prompt it was
-- produced under, and a change reaches the conversation as a system-update
-- message instead.
--
-- Rows saved before this migration hold no shared prompt and no parts. The
-- next claim of one sends the framework prompt it composes, as every claim did
-- before, and saves it with the parts.

ALTER TABLE conversations ADD COLUMN system_prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN system_parts TEXT;

-- +goose Down
SELECT 'down not supported';
