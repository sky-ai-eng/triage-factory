-- +goose Up
-- traceparent carries the W3C trace context of whoever enqueued this row,
-- so the routing of an event can be tied back to the poll cycle that
-- emitted it. The queue row is a message envelope, which is the only place
-- in the schema a trace id belongs: the same pattern as a Kafka header or a
-- Sidekiq job payload, and its retention already matches a trace backend's
-- (done rows are pruned after a week). Domain tables — events, tasks,
-- conversations — deliberately carry no trace ids: they outlive the traces
-- by years, so the column would mostly point at spans that no longer exist,
-- and the reverse join (domain ids as span attributes) works without it.
--
-- Nullable, no default, no backfill. NULL is the normal state, not a
-- degraded one: a row enqueued with tracing disabled, from an untraced
-- path, or by a process whose exporter was never configured has no context
-- to carry, and every consumer routes such a row identically — it simply
-- gets no link. The consumer is a LINK, not a child: one poll cycle emits N
-- events routed later and possibly on another pod, so parenting would build
-- one sprawling multi-pod trace instead of N navigable ones.
ALTER TABLE event_queue ADD COLUMN traceparent TEXT;

-- +goose Down
SELECT 'down not supported';
