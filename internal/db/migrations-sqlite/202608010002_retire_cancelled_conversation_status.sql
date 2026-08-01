-- +goose Up
-- Retire the last two conversation statuses nothing writes any more, by
-- rewriting the rows rather than teaching every predicate to recognize them.
--
-- 'cancelled' was a third spelling of a concept the task layer
-- (return-to-queue, drag-to-done) and the blueprint layer (cancel_requested /
-- blueprint_runs.status='cancelled') already owned. A stopped run now parks
-- `open` and its blueprint carries the cancellation, so this UPDATE is not a
-- lossy approximation: every row it touches has a terminal blueprint already
-- (the old cancel path cascaded), which means `open` + terminal parent IS the
-- faithful new spelling of what happened to it. Migrated rows end up
-- indistinguishable from a run stopped by this build.
--
-- Why data rather than guards: every predicate over conversations.status is an
-- exclusion (`status NOT IN (…)`), so a retired value left out of one doesn't
-- fail closed — it readmits a run that ended months ago to parking, to
-- cancelling, or to the active-work counters. Carrying the old names forward
-- means carrying them in every such list forever, and getting one wrong is
-- silent. Rewriting the rows once leaves a single vocabulary that every guard
-- can just read.
--
-- 'pending_approval' deliberately does NOT get the same treatment and stays in
-- the exclusion lists: those rows self-resolve (a human resolves the artifact
-- and the row retires itself), so they are dormant-but-live work rather than
-- history that needs restating.
--
-- parked_at is backfilled from the old terminal stamp so the
-- snapshot-retention sweep reads each row's true age instead of treating a
-- year-old cancellation as freshly parked. These rows have no snapshot to reap
-- — the old cancel path discarded it on the way out — which is what makes a
-- follow-up on one answer 410 Gone ("this workspace has expired") rather than
-- pretending it can be resumed.
UPDATE conversations
SET status = 'open',
    parked_at = COALESCE(parked_at, completed_at, started_at)
WHERE status = 'cancelled';

-- 'task_unsolvable' never had a writer in the queue-driven model, so this is
-- expected to touch nothing; it exists so the vocabulary is provably closed
-- rather than closed-by-assumption. 'failed' is the honest landing spot for
-- any row that does turn up: it ended without a usable conclusion, and unlike
-- `open` it makes no claim that a follow-up could continue it.
UPDATE conversations SET status = 'failed' WHERE status = 'task_unsolvable';

-- +goose Down
SELECT 'down not supported';
