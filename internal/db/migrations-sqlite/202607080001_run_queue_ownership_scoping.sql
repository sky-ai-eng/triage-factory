-- +goose Up
-- Ownership-scoped boot recovery (TFAC-578). Boot recovery used to reset
-- EVERY in-flight run/event_queue row, global and unscoped — safe at N=1
-- (there is only ever one process) but a real hazard the moment a second
-- instance exists: a booting process would re-queue rows a live sibling is
-- actively executing, and the re-claim would re-run them (duplicate agent
-- runs, duplicate external writes, out-of-order routing).
--
-- runs.executor_id already exists (shipped with runs), but the reset sweep
-- (ResetProcessingRuns) needs a second fencing field: an instance's
-- persistent id survives restarts, so executor_id alone can't distinguish
-- "still live under an older boot of me" from "genuinely orphaned by a
-- crash". boot_epoch is that fence — it's stamped onto the row by
-- ClaimNextRun/ClaimNext at claim time (atomically, so there is no
-- claim-to-live window where a running row's ownership is unknown), and the
-- self-sweep only resets rows where executor_id = mine AND boot_epoch is
-- from a strictly earlier boot than my current one.
--
-- event_queue never had an ownership column at all (it was always reset
-- unconditionally); it gets both columns here so its claim path can be
-- fenced the same way runs already partially was.
ALTER TABLE runs ADD COLUMN boot_epoch INTEGER;
ALTER TABLE event_queue ADD COLUMN executor_id TEXT;
ALTER TABLE event_queue ADD COLUMN boot_epoch INTEGER;

-- +goose Down
SELECT 'down not supported';
