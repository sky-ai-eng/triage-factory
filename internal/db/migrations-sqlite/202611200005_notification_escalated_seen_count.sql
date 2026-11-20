-- +goose Up
-- A notification keeps the seen_count of the sighting that opened it or last
-- escalated it, which is how an upsert tells an escalation from a bump in the
-- same clock tick: the two stamp escalated_at and last_seen_at with one
-- instant either way.
--
-- Rows saved before this migration hold 1. The value decides only what the
-- next write to the row reports, and any value up to seen_count reports it
-- correctly.

ALTER TABLE notifications ADD COLUMN escalated_seen_count INTEGER NOT NULL DEFAULT 1
    CHECK (escalated_seen_count >= 1 AND escalated_seen_count <= seen_count);

-- +goose Down
SELECT 'down not supported';
