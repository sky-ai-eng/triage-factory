-- pending_firings 'draining' crash recovery + dedup visibility.
--
-- The claiming pop (202607080002) made a popped firing invisible: a
-- drainer that died between PopForEntity and MarkFired/MarkSkipped left
-- the row in 'draining' forever, and nothing recovered it. claimed_at
-- stamps the claim so the drain sweeper can requeue stale ones.
--
-- The dedup index also widens to cover 'draining': a firing mid-drain is
-- still queued intent for its (task, trigger), and a duplicate enqueued
-- during the drain window would fire a second run for the same intent
-- once the first one's run terminates. (Pre-claiming-pop, the row stayed
-- 'pending' through the drain and the old predicate covered this
-- naturally.)
--
-- +goose Up
ALTER TABLE pending_firings ADD COLUMN claimed_at DATETIME;

DROP INDEX idx_pending_firings_dedup;
CREATE UNIQUE INDEX idx_pending_firings_dedup ON pending_firings(task_id, trigger_id) WHERE status IN ('pending', 'draining');

-- +goose Down
SELECT 'down not supported';
