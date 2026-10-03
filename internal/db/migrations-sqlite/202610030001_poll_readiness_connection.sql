-- +goose Up
-- The connection state of each (org, source): whether the last poll cycle that
-- made requests reached the upstream. The poller writes it at the end of every
-- cycle from that cycle's own request outcomes, and logs the loss and the
-- restoration once each instead of every failed request.
--
-- connection_state is 'unknown' until a cycle first records one, then 'up' or
-- 'down', and 'unknown' again once the poller stops checking the source (turned
-- off, no credential, nothing tracked). connection_changed_at is when the
-- current state began, NULL while it is 'unknown'. connection_failure_class is
-- the request outcome class that put the connection down ('transient' or
-- 'auth'), and NULL whenever the state is not 'down'.
--
-- App-validated, not CHECK-constrained, matching the other ADD COLUMN text
-- columns: widening a SQLite CHECK means a full table rebuild.
ALTER TABLE poll_readiness ADD COLUMN connection_state TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE poll_readiness ADD COLUMN connection_changed_at DATETIME;
ALTER TABLE poll_readiness ADD COLUMN connection_failure_class TEXT;

-- +goose Down
SELECT 'down not supported';
