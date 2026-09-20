-- +goose Up
-- entity_poll_seq carries the entity's poll_seq at the moment this row's event
-- was judged: the value the tracker's snapshot CAS advanced TO when it
-- committed the batch the event rode in. It lives on the queue row for the
-- same reason traceparent does — the row is the envelope, and a version is a
-- property of one delivery, not of the durable event. A terminating close
-- reads it back and refuses to land against any other version of the entity,
-- which is what keeps a merged event judged before a reopen from closing the
-- reopened pull request minutes later.
--
-- Nullable, no default, no backfill. NULL is a row that arrived through the
-- ingest path rather than the CAS path (the Jira unreachable retirement is the
-- one such terminating event today); the close it implies is guarded on the
-- entity's state alone. Existing rows read NULL and route identically.
ALTER TABLE event_queue ADD COLUMN entity_poll_seq INTEGER;

-- +goose Down
SELECT 'down not supported';
