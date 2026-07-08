-- One-time normalization of in-flight queue rows that predate the
-- persistent instance registry.
--
-- Ownership-scoped boot recovery resets only rows whose executor_id
-- matches this process's persistent registry id. Rows claimed by builds
-- BEFORE the registry existed carry a per-boot random uuid (stamped at
-- go-live) or NULL (crashed before go-live) — values no future boot's
-- sweep can ever match. Without this normalization, a run that was
-- mid-flight when the operator stopped the old binary stays 'running'
-- forever after the upgrade (blueprint wedged, task pinned in-progress),
-- and an event_queue row mid-processing wedges silently — the forward-only
-- tracker never re-emits its event, so the loss is permanent.
--
-- Safe to run exactly once, here, because migrations execute at boot
-- before any worker starts and SQLite is single-process by construction:
-- nothing can be legitimately in flight while this statement runs. It is
-- the old global boot reset (which ownership scoping replaced), replayed
-- one last time.
--
-- +goose Up
UPDATE runs SET status = 'queued', claimed_at = NULL, executor_id = NULL, boot_epoch = NULL
WHERE status NOT IN (
	'queued',
	'completed','failed','cancelled','task_unsolvable',
	'open','pending_approval'
)
AND blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running');

UPDATE event_queue SET status = 'pending', claimed_at = NULL, executor_id = NULL, boot_epoch = NULL
WHERE status = 'processing';

-- +goose Down
SELECT 'down not supported';
