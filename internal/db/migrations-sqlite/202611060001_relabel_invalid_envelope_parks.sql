-- +goose Up
-- `invalid_envelope` is not a park reason: an SDK run concludes through
-- stop_blueprint or by ending its turn, and parks on no completion envelope.
-- A stored row that carries it had its blueprint aborted when it parked and
-- waits on a person like any other idle park, so it is relabelled one.
UPDATE conversations
SET park_reason = 'idle'
WHERE park_reason = 'invalid_envelope';

-- +goose Down
SELECT 'down not supported';
