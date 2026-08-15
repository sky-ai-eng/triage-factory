-- +goose Up
-- GitHub webhook delivery dedup. X-GitHub-Delivery was read only to decorate
-- the published event's metadata — never persisted, never checked — so a
-- redelivered copy re-ran the installation upsert and re-published to the bus.
-- GitHub does not auto-retry a failed delivery, but it does redeliver on
-- demand (the Redeliver button; POST /app/hook/deliveries/{id}/attempts, 30-day
-- window), which is exactly the recovery path an operator reaches for after an
-- outage. A redelivery is ordinary traffic, not an anomaly.
--
-- Keyed (installation_id, delivery_id), NOT (org_id, delivery_id). The
-- org-keyed form cannot be written before the tenant is known, and "before the
-- tenant is known" is precisely the shape a shared-App receiver has — it routes
-- installation.id -> org_id and must drop an unmapped delivery before any side
-- effect. The installation-keyed form costs nothing today and does not have to
-- be re-keyed later. A delivery carrying no installation object
-- (github_app_authorization, for one) keys on the delivery id alone and stores
-- '' in this column: the empty string rather than NULL, because a NULL in a
-- primary key defeats the uniqueness the table exists for.
--
-- received_at is written from Go on every insert and deliberately has no
-- CURRENT_TIMESTAMP default: SQLite stores a bound time.Time in Go's own string
-- form and CURRENT_TIMESTAMP as 'YYYY-MM-DD HH:MM:SS', and the prune compares
-- this column against a bound Go time. One writer, one format, so that
-- comparison is of like with like — and NOT NULL with no default makes a
-- hand-written INSERT that omits it fail rather than quietly store the other
-- shape.
--
-- The receiver is normally a multi-mode surface, but a local install GitHub can
-- reach registers a live hook URL and runs this same handler against SQLite, so
-- the table is real in both dialects rather than a local stub.
CREATE TABLE github_webhook_deliveries (
    installation_id TEXT NOT NULL,
    delivery_id     TEXT NOT NULL,
    received_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (installation_id, delivery_id)
);

-- The prune (received_at < now - 72h, piggybacked on every insert) is the only
-- query that doesn't go through the primary key. Unlike the Slack dedup table
-- next door, this one sees every push / check_run / comment across every
-- tracked repo, so a sequential scan per delivery is not a safe assumption to
-- ship; with this index the usual "nothing to prune" case is a range probe that
-- matches nothing.
CREATE INDEX github_webhook_deliveries_received_at_idx
    ON github_webhook_deliveries (received_at);

-- +goose Down
SELECT 'down not supported';
