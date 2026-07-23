// Package reaper is the leader-elected fleet reaper (TFAC-586, spec §4.3):
// requeue-or-terminal-fail runs whose owning executor's registry heartbeat
// has gone stale, and tombstone registry rows abandoned long enough that
// they can never come back. Postgres only — mirrors internal/lease's
// independence from db.Stores (a raw admin *sql.DB, no SQLite twin): a
// reaper only means something once more than one executor can exist, which
// is structurally a multi-mode-only condition. internal/app constructs a
// Store only for brain-capable roles in multi mode and never on local.
package reaper

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Counts is the outcome of one ReapDeadExecutors sweep — how many runs the
// reaper requeued, terminal-failed, or cancel-finalized. Logged by
// RunReaper on every non-empty tick.
type Counts struct {
	Requeued  int
	Failed    int
	Cancelled int
}

// Store is the reaper's persistence seam.
type Store interface {
	// ReapDeadExecutors sweeps every run claimed/running under an executor
	// whose registry heartbeat is missing or older than staleThreshold (own
	// DB time — now() - make_interval, never Go's wall clock, matching
	// internal/lease's discipline), restricted to runs whose owning
	// blueprint_run is still 'running' (spec §4.3's candidate predicate — a
	// row under an already-terminal blueprint_run belongs to
	// RunQueueStore.ReconcileOrphanedRuns, not here). A draining-but-
	// heartbeating executor is never touched: draining is not death, and
	// the predicate only looks at heartbeat staleness.
	//
	// Three disjoint outcomes per candidate:
	//   - blueprint_run.cancel_requested: finalize cancelled (run +
	//     blueprint_run), regardless of attempts — the existing
	//     cancel-finalization semantics, just with no live owner to signal.
	//   - not cancel-requested, attempts < maxAttempts: requeue with the
	//     same status/summary semantics as RunQueueStore.RequeueRun.
	//     attempts is left untouched — the eventual re-claim bumps it.
	//   - not cancel-requested, attempts >= maxAttempts: terminal-fail the
	//     run with failure_kind='executor_lost' and finalize the owning
	//     blueprint_run failed.
	ReapDeadExecutors(ctx context.Context, staleThreshold time.Duration, maxAttempts int) (Counts, error)

	// DeleteStaleInstances tombstones registry rows whose heartbeat is
	// older than staleAfter (own DB time). runs.executor_id carries no FK
	// to instances (by design — verified by a dedicated test), so this
	// never touches audit history; a resurrected id simply re-registers at
	// a fresh boot_epoch 1.
	DeleteStaleInstances(ctx context.Context, staleAfter time.Duration) (int, error)

	// CancelStrandedCuratorTurns releases the active claims of running
	// curator turns whose executor's heartbeat is missing or stale (curator
	// homing, spec §6.3). A permanently-dead home never runs its own
	// ownership-scoped boot sweep, so without this a turn stranded on it
	// would show as in-flight forever. Recovery here is retire-only, not
	// requeue: the user's NEXT turn re-homes the project to a live executor
	// (the Homer's sticky-until-death mint). A queued turn needs no reaping
	// at all anymore — it is an unowned undelivered message with no claim,
	// and simply waits for the re-home. Own DB time (now() -
	// make_interval), same discipline as ReapDeadExecutors. Returns the
	// count released.
	CancelStrandedCuratorTurns(ctx context.Context, staleThreshold time.Duration) (int, error)

	// HealClaimDesyncs runs the two janitor arms for the states the
	// app-pool terminal writes can strand a delegation conversation in
	// (their conversation flip and claim release commit independently):
	// a terminal conversation with a dangling active claim gets the claim
	// released (outcome mapped from status), and an in-flight delegation
	// conversation with no active claim under a running blueprint parent
	// goes back to 'queued' for re-claim. The same arms run at boot inside
	// RunQueueStore.ReconcileOrphanedRuns; the periodic repeat here bounds
	// the desync's lifetime to a reaper tick instead of the next restart.
	// Both arms are idempotent and safe against in-flight healthy writes.
	// Curator conversations (status NULL) are never touched — their claim
	// recovery belongs to the curator sweeps above.
	HealClaimDesyncs(ctx context.Context) (requeued, released int, err error)
}

// pgStore is the Postgres implementation.
type pgStore struct{ db *sql.DB }

// NewPostgresStore builds a Store against a pooled *sql.DB — the admin pool,
// same posture as internal/lease.NewPostgresStore (the tables touched here
// are system-wide, not org-scoped).
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

var _ Store = (*pgStore)(nil)

// nonProcessingStatusesSQL is the runs.status IN-list of every status the
// reaper must NOT touch: still-queued (no owner yet), every terminal, and
// the two dormant/parked statuses (no live process to have died). Mirrors
// RunQueueStore.ResetProcessingRuns' own inline list (internal/db/postgres/
// run_queue.go) — kept as its own literal here rather than shared, matching
// that file's own precedent ("other queries that add dormant statuses keep
// their own list").
const nonProcessingStatusesSQL = `'queued','completed','failed','cancelled','task_unsolvable','open','pending_approval'`

// reapCandidateJoin is the FROM/JOIN/base-WHERE shared by every reaper
// query below: claimed/running runs under a still-running blueprint_run
// whose executor's heartbeat row is missing (GC'd, or never registered —
// defensive) or older than the staleness threshold. Each caller appends its
// own cancel_requested/attempts predicate and SELECTs what it needs.
// $1 is always the staleness threshold in seconds.
const reapCandidateJoin = `
	FROM conversations r
	JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
	JOIN blueprint_runs br ON br.id = r.blueprint_run_id
	LEFT JOIN instances i ON i.id = cl.executor_id
	WHERE r.status NOT IN (` + nonProcessingStatusesSQL + `)
	  AND br.status = 'running'
	  AND (i.id IS NULL OR i.last_heartbeat_at < now() - make_interval(secs => $1))
`

// releaseReapedClaimsSQL releases the active claims of a just-swept
// conversation set. The claim-level outcome is always 'reaped' — the
// engagement ended because its executor died — while the conversation-level
// status carries the user-facing disposition (cancelled/failed/queued).
const releaseReapedClaimsSQL = `
	UPDATE claims SET released_at = now(), outcome = 'reaped'
	WHERE released_at IS NULL AND conversation_id = ANY($1)
`

func (s *pgStore) ReapDeadExecutors(ctx context.Context, staleThreshold time.Duration, maxAttempts int) (Counts, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Counts{}, err
	}
	defer tx.Rollback()

	staleSecs := staleThreshold.Seconds()
	var out Counts

	// 1. Cancel-requested candidates: finalize cancelled instead of
	// requeuing — "cancel-requested rows go through the existing cancel
	// finalization instead of requeue" (spec §4.3). No live owner to
	// signal, so this IS the finalization: a dead executor can't apply a
	// cancel signal to a process that no longer exists.
	cancelledBlueprintIDs, cancelledIDs, err := reapUpdateRuns(ctx, tx, staleSecs, nil, `
		UPDATE conversations SET status = 'cancelled', completed_at = now(), stop_reason = 'cancelled',
			result_summary = 'Cancelled: owning blueprint run was cancel-requested under a dead executor (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = true
		)
		RETURNING blueprint_run_id, id
	`)
	if err != nil {
		return Counts{}, err
	}
	out.Cancelled = len(cancelledBlueprintIDs)
	if len(cancelledBlueprintIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE blueprint_runs SET status = 'cancelled', completed_at = now()
			WHERE id = ANY($1) AND status = 'running'
		`, pgUUIDArray(cancelledBlueprintIDs)); err != nil {
			return Counts{}, err
		}
		if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(cancelledIDs)); err != nil {
			return Counts{}, err
		}
	}

	// 2. Not cancel-requested, attempts exhausted: terminal-fail.
	failedBlueprintIDs, failedIDs, err := reapUpdateRuns(ctx, tx, staleSecs, &maxAttempts, `
		UPDATE conversations SET status = 'failed', failure_kind = 'executor_lost', completed_at = now(),
			stop_reason = 'executor_lost',
			result_summary = 'Failed: executor lost and the attempt budget (TF_RUN_MAX_ATTEMPTS) is exhausted (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = false
			  AND (SELECT count(*) FROM claims a WHERE a.conversation_id = r.id) >= $2
		)
		RETURNING blueprint_run_id, id
	`)
	if err != nil {
		return Counts{}, err
	}
	out.Failed = len(failedBlueprintIDs)
	if len(failedBlueprintIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE blueprint_runs SET status = 'failed', completed_at = now(), abort_reason = 'executor_lost'
			WHERE id = ANY($1) AND status = 'running'
		`, pgUUIDArray(failedBlueprintIDs)); err != nil {
			return Counts{}, err
		}
		if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(failedIDs)); err != nil {
			return Counts{}, err
		}
	}

	// 3. Not cancel-requested, attempts remaining: requeue. Same status/
	// summary semantics as RunQueueStore.RequeueRun — clears the ownership
	// stamp (a queued row has no owner) and leaves attempts as-is; the
	// eventual re-claim bumps it. preferred_executor_id is cleared too
	// (TFAC-587): the reaper requeues precisely because the stamped executor
	// is dead, so its stamp is the staler-than-a-dwell case the placement
	// design calls out — NULL is "unowned, claimable by any live executor
	// now" (no aging delay), which is the correct advisory answer here.
	// Affinity is re-earned on the next enqueue, never carried toward a
	// corpse.
	_, requeuedIDs, err := reapUpdateRuns(ctx, tx, staleSecs, &maxAttempts, `
		UPDATE conversations SET status = 'queued', queued_at = now(),
			preferred_executor_id = NULL,
			result_summary = 'Requeued: executor heartbeat stale (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = false
			  AND (SELECT count(*) FROM claims a WHERE a.conversation_id = r.id) < $2
		)
		RETURNING blueprint_run_id, id
	`)
	if err != nil {
		return Counts{}, err
	}
	out.Requeued = len(requeuedIDs)
	if len(requeuedIDs) > 0 {
		if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(requeuedIDs)); err != nil {
			return Counts{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Counts{}, err
	}
	return out, nil
}

// reapUpdateRuns runs one of the RETURNING blueprint_run_id updates above
// and collects the affected blueprint_run ids, binding staleSecs as $1 and,
// when maxAttempts is non-nil, the attempts threshold as $2 — factored out
// since two of the three sweep queries share this exact shape.
func reapUpdateRuns(ctx context.Context, tx *sql.Tx, staleSecs float64, maxAttempts *int, query string) (blueprintIDs, conversationIDs []string, err error) {
	var rows *sql.Rows
	if maxAttempts != nil {
		rows, err = tx.QueryContext(ctx, query, staleSecs, *maxAttempts)
	} else {
		rows, err = tx.QueryContext(ctx, query, staleSecs)
	}
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var bid, cid string
		if err := rows.Scan(&bid, &cid); err != nil {
			return nil, nil, err
		}
		blueprintIDs = append(blueprintIDs, bid)
		conversationIDs = append(conversationIDs, cid)
	}
	return blueprintIDs, conversationIDs, rows.Err()
}

func (s *pgStore) DeleteStaleInstances(ctx context.Context, staleAfter time.Duration) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM instances WHERE last_heartbeat_at < now() - make_interval(secs => $1)
	`, staleAfter.Seconds())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *pgStore) HealClaimDesyncs(ctx context.Context) (int, int, error) {
	// Duplicates internal/db/postgres's healClaimDesyncs SQL rather than
	// importing it, keeping this package dialect-independent like the rest
	// of the file (see pgUUIDArray's rationale). Statement order matters
	// only for accounting, not correctness — each arm's predicate is
	// disjoint from the other's write set.
	res, err := s.db.ExecContext(ctx, `
		UPDATE claims SET released_at = now(),
		    outcome = CASE c.status
		        WHEN 'completed' THEN 'completed'
		        WHEN 'cancelled' THEN 'cancelled'
		        ELSE 'failed'
		    END
		FROM conversations c
		WHERE claims.conversation_id = c.id AND claims.released_at IS NULL
		  AND c.status IN ('completed','failed','cancelled','task_unsolvable')
	`)
	if err != nil {
		return 0, 0, err
	}
	rel, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}

	res, err = s.db.ExecContext(ctx, `
		UPDATE conversations SET status = 'queued', queued_at = now(),
		    preferred_executor_id = NULL
		WHERE type = 'delegation'
		  AND status NOT IN (`+nonProcessingStatusesSQL+`)
		  AND NOT EXISTS (
		      SELECT 1 FROM claims cl
		      WHERE cl.conversation_id = conversations.id AND cl.released_at IS NULL
		  )
		  AND blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
	`)
	if err != nil {
		return 0, 0, err
	}
	req, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	return int(req), int(rel), nil
}

func (s *pgStore) CancelStrandedCuratorTurns(ctx context.Context, staleThreshold time.Duration) (int, error) {
	// A home is dead when its instances row is missing (GC'd or never
	// registered) or its heartbeat is older than the threshold — the same
	// missing-or-stale predicate ReapDeadExecutors uses for a run's executor.
	// The token columns are rolled up from the curator_messages SUM, same as
	// every other terminal write (a 'running' turn that streamed before its home
	// died must still report its spend — TFAC-473); this reaper runs on the
	// leader's superuser admin pool, so no extra grant applies here.
	res, err := s.db.ExecContext(ctx, `
		UPDATE claims
		SET released_at = now(),
		    outcome = 'cancelled',
		    error = COALESCE(error, 'Cancelled: curator home executor lost (reaper) — re-send to re-home'),
		    input_tokens          = (SELECT COALESCE(SUM(m.input_tokens), 0)          FROM messages m WHERE m.claim_id = claims.id),
		    output_tokens         = (SELECT COALESCE(SUM(m.output_tokens), 0)         FROM messages m WHERE m.claim_id = claims.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(m.cache_read_tokens), 0)     FROM messages m WHERE m.claim_id = claims.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(m.cache_creation_tokens), 0) FROM messages m WHERE m.claim_id = claims.id)
		WHERE released_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM conversations c
		      WHERE c.id = claims.conversation_id AND c.type = 'curator'
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM instances i
		      WHERE i.id = claims.executor_id
		        AND i.last_heartbeat_at >= now() - make_interval(secs => $1)
		  )
	`, staleThreshold.Seconds())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// pgUUIDArray formats a Go string slice as a Postgres uuid[] literal for
// binding through a single $N parameter — the pgx stdlib driver accepts the
// textual array form for typed-array columns. Mirrors
// internal/db/postgres's own unexported helper of the same name and
// rationale; duplicated rather than imported to keep this package
// independent of a specific dialect package (internal/lease follows the
// same independence). Quoting rules: ids are uuid-shaped (no commas,
// braces, or backslashes), so raw element values are safe to emit inside
// the {…} envelope without escaping.
func pgUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	return "{" + strings.Join(ids, ",") + "}"
}
