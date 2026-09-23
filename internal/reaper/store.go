// Package reaper is the leader-elected fleet reaper (TFAC-586, spec §4.3):
// requeue-or-terminal-fail conversations whose owning executor's registry heartbeat
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

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// Counts is the outcome of one ReapDeadExecutors sweep — how many conversations the
// reaper requeued, terminal-failed, cancel-finalized, or released to the
// dispatcher's stop settlement. Logged by RunReaper on every non-empty tick.
type Counts struct {
	Requeued      int
	Failed        int
	Cancelled     int
	StopsReleased int
}

// Store is the reaper's persistence seam.
type Store interface {
	// ReapDeadExecutors sweeps every conversation claimed/running whose
	// engagement is over — either its executor's registry heartbeat is missing
	// or older than staleThreshold, or the claim's own lease has lapsed
	// whatever the instance says (own DB time — now() - make_interval, never
	// Go's wall clock, matching internal/lease's discipline) — restricted to
	// conversations whose owning
	// blueprint_run is still 'running' (spec §4.3's candidate predicate — a
	// row under an already-terminal blueprint_run belongs to
	// ConversationQueueStore.ReconcileOrphanedConversations, not here). A draining-but-
	// heartbeating executor is never touched: draining is not death, and
	// the predicate only looks at heartbeat staleness.
	//
	// maxAttempts is counted in loss EPISODES, not lifetime claims: the run
	// of consecutive handed-back claims at the tail of the conversation's
	// history, the one being reaped here included. Any claim that recorded an
	// outcome of its own ends the episode, so a conversation stopped and
	// resumed four times meets its first executor death at 1, not 5 — the
	// dispatcher's unit exactly (postgres.EpisodeAttemptsSQL, which this
	// shares rather than re-derives).
	//
	// Four disjoint outcomes per candidate:
	//   - a stop is pending: release the claim and write nothing else. The
	//     row is then one no live claim holds, which is exactly what
	//     ConversationQueueStore.SettleUnclaimedStopsSystem parks, so every
	//     stop nobody holds settles on that one path, with its derived park
	//     reason, its run cancel and its broadcast.
	//   - blueprint_run.cancel_requested: finalize cancelled (conversation +
	//     blueprint_run), regardless of attempts — the existing
	//     cancel-finalization semantics, just with no live owner to signal.
	//   - not cancel-requested, episode below maxAttempts: requeue with the
	//     same status/summary semantics as ConversationQueueStore.RequeueConversation. The
	//     'reaped' release below is what carries this attempt into the next
	//     claim's count.
	//   - not cancel-requested, episode at maxAttempts: terminal-fail the
	//     conversation with failure_kind='executor_lost' and finalize the owning
	//     blueprint_run failed.
	ReapDeadExecutors(ctx context.Context, staleThreshold time.Duration, maxAttempts int) (Counts, error)

	// DeleteStaleInstances tombstones registry rows whose heartbeat is
	// older than staleAfter (own DB time). claims.executor_id carries no FK
	// to instances (by design — verified by a dedicated test), so this
	// never touches audit history; a resurrected id simply re-registers at
	// a fresh boot_epoch 1.
	DeleteStaleInstances(ctx context.Context, staleAfter time.Duration) (int, error)
}

// pgStore is the Postgres implementation.
type pgStore struct{ db *sql.DB }

// NewPostgresStore builds a Store against a pooled *sql.DB — the admin pool,
// same posture as internal/lease.NewPostgresStore (the tables touched here
// are system-wide, not org-scoped).
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

var _ Store = (*pgStore)(nil)

// reapCandidateJoin is the FROM/JOIN/base-WHERE shared by every reaper
// query below: conversations still mid-flight (status NULL — no outcome was
// ever written) with an unreleased claim, under a still-running
// blueprint_run, whose engagement is over. Each caller appends its own
// cancel_requested/episode predicate and SELECTs what it needs. $1 is
// always the staleness threshold in seconds.
//
// The claim join is what makes "a live process died" expressible: a parked or
// terminal conversation has no engagement to have died. Two arms then say it
// died, and either is enough.
//
//   - Its executor's heartbeat row is missing (GC'd, or never registered —
//     defensive) or older than the staleness threshold: the whole process is
//     gone, so every claim it holds is.
//   - Its own lease lapsed, whatever the instance says: the process may be
//     alive and heartbeating while the goroutine driving THIS conversation is
//     not, and nothing but the claim's own expiry can tell.
//
// The lease arm is safe against a holder that is still working, because the
// claim-lease timings put the holder's self-fence strictly before its lease:
// a holder whose renewals stopped killed its own cell 30s before the lease
// this reads lapsed.
//
// Both arms read now(), so both are frozen at the sweep's BEGIN and the three
// UPDATEs below agree on one candidate set. Elsewhere a lease's liveness is
// read on statement_timestamp(), because it answers for this instant; here the
// sweep is the unit, and a claim that lapses between its statements waits for
// the next tick rather than landing in whichever arm happened to run after
// it.
const reapCandidateJoin = `
	FROM conversations r
	JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
	JOIN blueprint_runs br ON br.id = r.blueprint_run_id
	LEFT JOIN instances i ON i.id = cl.executor_id
	WHERE r.status IS NULL
	  AND br.status = 'running'
	  AND (i.id IS NULL
	       OR i.last_heartbeat_at < now() - make_interval(secs => $1)
	       OR cl.lease_expires_at <= now())
`

// reapEpisodeAttemptsSQL counts the loss episode the claim being reaped
// belongs to, against this file's conversation alias. Imported from the
// Postgres dialect package rather than restated here because it is the
// dispatcher's budget unit and the two must agree by construction: a local
// copy is how the reaper came to fail a conversation on its first executor
// death for the crime of having been resumed twice.
var reapEpisodeAttemptsSQL = postgres.EpisodeAttemptsSQL("r")

// releaseReapedClaimsSQL releases the active claims of a just-swept
// conversation set. The claim-level outcome is always 'reaped' — the
// engagement ended because its executor died — while the conversation-level
// status carries the user-facing disposition (parked/failed/queued).
const releaseReapedClaimsSQL = `
	UPDATE claims SET released_at = now(), outcome = 'reaped'
	WHERE released_at IS NULL AND conversation_id = ANY($1)
`

func (s *pgStore) ReapDeadExecutors(ctx context.Context, staleThreshold time.Duration, maxAttempts int) (Counts, error) {
	staleSecs := staleThreshold.Seconds()
	var out Counts
	if err := db.InTx(ctx, s.db, func(tx *sql.Tx) error {
		// 0. A stop is pending: release the claim only. The claim gate
		// refuses a stop-requested row, so a requeue would only have
		// relabelled it before the settlement parked it, and a fail would
		// record a crash loop for a conversation someone asked to stop. This
		// runs first because the arms below find their candidates through an
		// unreleased claim, so a row released here is out of their sets.
		res, err := tx.ExecContext(ctx, `
			UPDATE claims SET released_at = now(), outcome = 'reaped'
			WHERE released_at IS NULL AND conversation_id IN (
				SELECT r.id `+reapCandidateJoin+`
				  AND r.stop_requested_at IS NOT NULL
			)
		`, staleSecs)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		out.StopsReleased = int(n)

		// 1. Cancel-requested candidates: finalize instead of requeuing —
		// "cancel-requested rows go through the existing cancel finalization
		// instead of requeue" (spec §4.3). No live owner to signal, so this IS the
		// finalization: a dead executor can't apply a cancel signal to a process
		// that no longer exists.
		//
		// The conversation PARKS `open` while its blueprint below takes the
		// 'cancelled' terminal — the split every cancel path uses. Read the park as
		// "stopped without concluding", NOT as "resumable": the blueprint is
		// terminal, so the claim gate refuses the row. What it buys is the
		// workspace, which the retention TTL collects on its own schedule instead
		// of a reaper throwing it away the instant a host went quiet.
		parkedBlueprintIDs, parkedIDs, err := reapUpdateConversations(ctx, tx, staleSecs, nil, `
			UPDATE conversations SET status = 'open', parked_at = COALESCE(parked_at, now()), park_reason = 'system_cancelled',
				stop_requested_at = NULL, stop_requested_by = NULL,
				result_summary = 'Stopped: owning blueprint run was cancel-requested after its executor engagement was lost (reaper)'
			WHERE id IN (
				SELECT r.id `+reapCandidateJoin+`
				  AND br.cancel_requested = true
			)
			RETURNING blueprint_run_id, id
		`)
		if err != nil {
			return err
		}
		out.Cancelled = len(parkedBlueprintIDs)
		if len(parkedBlueprintIDs) > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE blueprint_runs SET status = 'cancelled', completed_at = now()
				WHERE id = ANY($1) AND status = 'running'
			`, pgUUIDArray(parkedBlueprintIDs)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(parkedIDs)); err != nil {
				return err
			}
		}

		// 2. Not cancel-requested, this loss episode exhausted: terminal-fail. The
		// episode is what makes this a crash loop rather than a conversation with
		// a long life behind it — see ReapDeadExecutors' contract.
		failedBlueprintIDs, failedIDs, err := reapUpdateConversations(ctx, tx, staleSecs, &maxAttempts, `
			-- No park_reason: this arm does not park, it fails. failure_kind is
			-- where a failure's cause is recorded, and stamping the same word into
			-- the park column would put a value on a row that was never parked.
			--
			-- ended_at/ended_reason ride the same statement because a failure IS
			-- a boundary: the brain is the only thing left that can say so for a
			-- conversation whose executor is gone, and a row failed here without
			-- the stamp would stay resumable and owe its memory to nobody.
			-- COALESCE holds the store doors' guard from raw SQL: a conversation
			-- a handler already ended (a takeover whose stop never landed, then
			-- an executor death) keeps the boundary that actually happened
			-- rather than having it relabelled as this failure.
			UPDATE conversations SET status = 'failed', failure_kind = 'executor_lost', completed_at = now(),
				ended_at = COALESCE(ended_at, now()), ended_reason = COALESCE(ended_reason, 'failed'),
				stop_requested_at = NULL, stop_requested_by = NULL,
				result_summary = 'Failed: the executor engagement was lost repeatedly (no heartbeat, or a lapsed claim lease) and the retry budget (TF_MAX_CLAIM_ATTEMPTS) for this loss episode is exhausted (reaper)'
			WHERE id IN (
				SELECT r.id `+reapCandidateJoin+`
				  AND br.cancel_requested = false
				  AND `+reapEpisodeAttemptsSQL+` >= $2
			)
			RETURNING blueprint_run_id, id
		`)
		if err != nil {
			return err
		}
		out.Failed = len(failedBlueprintIDs)
		if len(failedBlueprintIDs) > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE blueprint_runs SET status = 'failed', completed_at = now(), abort_reason = 'executor_lost'
				WHERE id = ANY($1) AND status = 'running'
			`, pgUUIDArray(failedBlueprintIDs)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(failedIDs)); err != nil {
				return err
			}
		}

		// 3. Not cancel-requested, this loss episode has room left: requeue. Same
		// semantics as ConversationQueueStore.RequeueConversation — releasing the claim (below) IS
		// the requeue, since the conversation is mid-flight and re-enters the
		// needs-driving predicate the moment it has no claim. That release is also
		// what keeps the episode open, so the next death counts this one.
		//
		// preferred_executor_id is cleared: the reaper requeues precisely because
		// the stamped executor is dead, so its stamp is the staler-than-a-dwell
		// case the placement design calls out — NULL is "unowned, claimable by any
		// live executor now" (no aging delay), which is the correct advisory
		// answer here. Affinity is re-earned on the next enqueue, never carried
		// toward a corpse.
		_, requeuedIDs, err := reapUpdateConversations(ctx, tx, staleSecs, &maxAttempts, `
			UPDATE conversations SET
				preferred_executor_id = NULL,
				result_summary = 'Requeued: the executor engagement was lost — no heartbeat, or a lapsed claim lease (reaper)'
			WHERE id IN (
				SELECT r.id `+reapCandidateJoin+`
				  AND br.cancel_requested = false
				  AND `+reapEpisodeAttemptsSQL+` < $2
			)
			RETURNING blueprint_run_id, id
		`)
		if err != nil {
			return err
		}
		out.Requeued = len(requeuedIDs)
		if len(requeuedIDs) > 0 {
			if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(requeuedIDs)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Counts{}, err
	}
	return out, nil
}

// reapUpdateConversations runs one of the RETURNING blueprint_run_id updates above
// and collects the affected blueprint_run ids, binding staleSecs as $1 and,
// when maxAttempts is non-nil, the attempts threshold as $2 — factored out
// since two of the three sweep queries share this exact shape.
func reapUpdateConversations(ctx context.Context, tx *sql.Tx, staleSecs float64, maxAttempts *int, query string) (blueprintIDs, conversationIDs []string, err error) {
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

// pgUUIDArray formats a Go string slice as a Postgres uuid[] literal for
// binding through a single $N parameter — the pgx stdlib driver accepts the
// textual array form for typed-array columns. A local twin of
// internal/db/postgres's unexported helper of the same name, kept rather
// than shared because it is a formatting rule and not a decision: two
// copies have nothing to disagree about. The episode counter above is
// imported for the opposite reason. Quoting rules: ids are uuid-shaped
// (no commas, braces, or backslashes), so raw element values are safe to
// emit inside the {…} envelope without escaping.
func pgUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	return "{" + strings.Join(ids, ",") + "}"
}
