-- +goose Up
-- The budget a refusal by the per-minute or per-claim signing budget ran into,
-- which the run's refusal line names. Like retry_after_seconds it serves multi
-- mode's control-plane queue, where the limit is read on the control pod that
-- refused and the run reads it back off the row; a local daemon records no
-- refusals. The column is described on the Postgres table.
ALTER TABLE commit_signatures ADD COLUMN refusal_limit INTEGER;

-- +goose Down
SELECT 'down not supported';
