-- +goose Up
-- Retire every conversation status nothing writes any more, by rewriting the
-- rows rather than teaching every predicate to recognize them.
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

-- 'pending_approval' is the same story one step later. Runs no longer park for
-- approval at all: the "needs approval" state is a view over the unresolved
-- artifact set, so a human resolves the draft PR / review and the board follows
-- from the artifact rather than from the run's status. `open` is the faithful
-- restatement — the run concluded its turn, the artifact it queued still exists
-- and still drives the approval column, and `open` is inert (excluded from
-- IsActiveRunStatus) until someone messages it. It also puts the row back in
-- front of the snapshot-retention sweep, which enumerates `open` and
-- `completed`: left as-is these rows pinned a workspace blob nothing could ever
-- enumerate, so the leak closes here rather than as a special case in the query.
--
-- stop_reason is deliberately NOT stamped. That column already carries two
-- vocabularies (a park reason, and the model's own stop reason on the SDK
-- path) and renders raw in the run station, so a synthetic third value would
-- show a user a word nothing explains. result_summary is left alone too: it
-- holds the agent's own summary of the turn, which is not ours to overwrite.
UPDATE conversations
SET status = 'open',
    parked_at = COALESCE(parked_at, completed_at, started_at)
WHERE status = 'pending_approval';

-- 'task_unsolvable' never had a writer in the queue-driven model, so this is
-- expected to touch nothing; it exists so the vocabulary is provably closed
-- rather than closed-by-assumption. 'failed' is the honest landing spot for
-- any row that does turn up: it ended without a usable conclusion, and unlike
-- `open` it makes no claim that a follow-up could continue it.
UPDATE conversations SET status = 'failed' WHERE status = 'task_unsolvable';

-- +goose Down
SELECT 'down not supported';
