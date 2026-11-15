-- +goose Up
-- `idle` was written for a turn that stopped before it finished, never for a
-- turn that finished, which parks with no reason. The reason is now
-- `turn_incomplete`. A stored row does not say which case wrote it, and every
-- case is one of those, so every `idle` row is relabelled.
UPDATE conversations
SET park_reason = 'turn_incomplete'
WHERE park_reason = 'idle';

-- +goose Down
SELECT 'down not supported';
