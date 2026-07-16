-- +goose Up
-- When the run last entered the queue. started_at is the mint stamp and never
-- resets, so after any re-queue (transient-setup retry, crash reconcile,
-- resume-by-enqueue) it stops describing the current wait. queued_at is
-- re-stamped on every flip to status='queued', so claimed_at − queued_at is
-- the latest queue episode's dwell and now − queued_at is the live wait a
-- QUEUED card shows — keeping queue time out of the working-time readouts.
-- Backfilled to started_at, the best available estimate for existing rows.
ALTER TABLE runs ADD COLUMN queued_at DATETIME;
UPDATE runs SET queued_at = started_at;

-- +goose Down
SELECT 'down not supported';
