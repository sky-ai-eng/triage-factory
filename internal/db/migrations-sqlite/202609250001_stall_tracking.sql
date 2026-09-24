-- +goose Up
-- The park reason a stop settles as, when it is not the requester's identity
-- that decides it. NULL derives the reason from stop_requested_by. Set only
-- with stop_requested_at, cleared with it.
ALTER TABLE conversations ADD COLUMN stop_requested_reason TEXT;
-- When the engagement last did anything the stall watchdog counts as
-- activity, on database time; and the operation it had in flight at the last
-- renewal, or NULL. Stamped by the renewal only. No backfill: a live claim
-- across the upgrade shows no activity until its next renewal.
ALTER TABLE claims ADD COLUMN last_activity_at DATETIME;
ALTER TABLE claims ADD COLUMN current_op TEXT;
-- +goose Down
SELECT 'down not supported';
