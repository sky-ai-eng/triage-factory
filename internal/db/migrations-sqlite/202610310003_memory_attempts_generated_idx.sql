-- +goose Up
-- The org's newest generated memory, which the memory provisioner's
-- failing-generation read measures every failure against.
CREATE INDEX idx_conversation_memory_attempts_generated
    ON conversation_memory_attempts (org_id, completed_at DESC) WHERE outcome = 'generated';

-- +goose Down
SELECT 'down not supported';
