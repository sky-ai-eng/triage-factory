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

	"github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	// maxAttempts is counted in loss EPISODES, not lifetime claims: the run
	// of consecutive handed-back claims at the tail of the conversation's
	// history, the one being reaped here included. Any claim that recorded an
	// outcome of its own ends the episode, so a conversation stopped and
	// resumed four times meets its first executor death at 1, not 5 — the
	// dispatcher's unit exactly (postgres.EpisodeAttemptsSQL, which this
	// shares rather than re-derives).
	//
	// Three disjoint outcomes per candidate:
	//   - blueprint_run.cancel_requested: finalize cancelled (run +
	//     blueprint_run), regardless of attempts — the existing
	//     cancel-finalization semantics, just with no live owner to signal.
	//   - not cancel-requested, episode below maxAttempts: requeue with the
	//     same status/summary semantics as RunQueueStore.RequeueRun. The
	//     'reaped' release below is what carries this attempt into the next
	//     claim's count.
	//   - not cancel-requested, episode at maxAttempts: terminal-fail the
	//     run with failure_kind='executor_lost' and finalize the owning
	//     blueprint_run failed.
	ReapDeadExecutors(ctx context.Context, staleThreshold time.Duration, maxAttempts int) (Counts, error)

	// DeleteStaleInstances tombstones registry rows whose heartbeat is
	// older than staleAfter (own DB time). claims.executor_id carries no FK
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

	// HealClaimDesyncs runs the janitor arm for the one state the app-pool
	// terminal writes can still strand a conversation in (their conversation
	// flip and claim release commit independently): a terminal conversation
	// with a dangling active claim gets the claim released, outcome mapped
	// from the status. The same arm runs at boot inside
	// RunQueueStore.ReconcileOrphanedRuns; the periodic repeat here bounds
	// the desync's lifetime to a reaper tick instead of the next restart.
	// Idempotent and safe against in-flight healthy writes.
	//
	// The former second arm — a mid-flight conversation whose claim released
	// but whose status flip rolled back, requeued by writing 'queued' — is
	// no longer a desync at all: a released claim on a mid-flight
	// conversation IS the requeue.
	HealClaimDesyncs(ctx context.Context) (released int, err error)

	// FailBlueprintRunsOrphanedAtMint terminal-fails every 'running'
	// blueprint_run that holds no child conversation at all and is older than
	// grace, stamping abort_reason=domain.BlueprintAbortOrphanedAtMint. This
	// is the one recovery shape no other arm can reach: every predicate above
	// joins through conversations, and an orphan minted by a crash between the
	// firing path's two commits has none — so it is invisible to them, keeps
	// holding the one-active-auto-run index against its task, and livelocks
	// the router's pending-firing drain (the busy gate reads conversations and
	// sees idle, the index refuses the insert, forever).
	//
	// Own DB time (now() - make_interval), same discipline as
	// ReapDeadExecutors. Failing rather than re-minting the step is what makes
	// it self-healing: the terminal write frees the index, and the firing
	// intent already queued for the task drains into a fresh, fully-minted
	// run. The same arm runs at boot inside
	// RunQueueStore.ReconcileOrphanedRuns (both dialects — local mode has the
	// same crash window and no reaper); the periodic repeat here bounds an
	// orphan's lifetime to a reaper tick instead of the next restart.
	FailBlueprintRunsOrphanedAtMint(ctx context.Context, grace time.Duration) (int, error)
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
// ever written) with a live claim, under a still-running blueprint_run,
// whose executor's heartbeat row is missing (GC'd, or never registered —
// defensive) or older than the staleness threshold. The claim join is what
// makes "a live process died" expressible: a parked or terminal
// conversation has no engagement to have died. Each caller appends its own
// cancel_requested/episode predicate and SELECTs what it needs. $1 is
// always the staleness threshold in seconds.
const reapCandidateJoin = `
	FROM conversations r
	JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
	JOIN blueprint_runs br ON br.id = r.blueprint_run_id
	LEFT JOIN instances i ON i.id = cl.executor_id
	WHERE r.status IS NULL
	  AND br.status = 'running'
	  AND (i.id IS NULL OR i.last_heartbeat_at < now() - make_interval(secs => $1))
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Counts{}, err
	}
	defer tx.Rollback()

	staleSecs := staleThreshold.Seconds()
	var out Counts

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
	parkedBlueprintIDs, parkedIDs, err := reapUpdateRuns(ctx, tx, staleSecs, nil, `
		UPDATE conversations SET status = 'open', parked_at = COALESCE(parked_at, now()), stop_reason = 'system_cancelled',
			result_summary = 'Stopped: owning blueprint run was cancel-requested under a dead executor (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = true
		)
		RETURNING blueprint_run_id, id
	`)
	if err != nil {
		return Counts{}, err
	}
	out.Cancelled = len(parkedBlueprintIDs)
	if len(parkedBlueprintIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE blueprint_runs SET status = 'cancelled', completed_at = now()
			WHERE id = ANY($1) AND status = 'running'
		`, pgUUIDArray(parkedBlueprintIDs)); err != nil {
			return Counts{}, err
		}
		if _, err := tx.ExecContext(ctx, releaseReapedClaimsSQL, pgUUIDArray(parkedIDs)); err != nil {
			return Counts{}, err
		}
	}

	// 2. Not cancel-requested, this loss episode exhausted: terminal-fail. The
	// episode is what makes this a crash loop rather than a conversation with
	// a long life behind it — see ReapDeadExecutors' contract.
	failedBlueprintIDs, failedIDs, err := reapUpdateRuns(ctx, tx, staleSecs, &maxAttempts, `
		UPDATE conversations SET status = 'failed', failure_kind = 'executor_lost', completed_at = now(),
			stop_reason = 'executor_lost',
			result_summary = 'Failed: executor lost repeatedly and the retry budget (TF_RUN_MAX_ATTEMPTS) for this loss episode is exhausted (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = false
			  AND `+reapEpisodeAttemptsSQL+` >= $2
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

	// 3. Not cancel-requested, this loss episode has room left: requeue. Same
	// semantics as RunQueueStore.RequeueRun — releasing the claim (below) IS
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
	_, requeuedIDs, err := reapUpdateRuns(ctx, tx, staleSecs, &maxAttempts, `
		UPDATE conversations SET
			preferred_executor_id = NULL,
			result_summary = 'Requeued: executor heartbeat stale (reaper)'
		WHERE id IN (
			SELECT r.id `+reapCandidateJoin+`
			  AND br.cancel_requested = false
			  AND `+reapEpisodeAttemptsSQL+` < $2
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

func (s *pgStore) HealClaimDesyncs(ctx context.Context) (int, error) {
	// Restates internal/db/postgres's healClaimDesyncs SQL, which is
	// unexported there — see pgUUIDArray on what this file shares and what
	// it keeps its own copy of.
	res, err := s.db.ExecContext(ctx, `
		UPDATE claims SET released_at = now(),
		    outcome = CASE c.status
		        WHEN 'completed' THEN 'completed'
		        ELSE 'failed'
		    END
		FROM conversations c
		WHERE claims.conversation_id = c.id AND claims.released_at IS NULL
		  AND c.status IN ('completed','failed')
	`)
	if err != nil {
		return 0, err
	}
	rel, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(rel), nil
}

func (s *pgStore) FailBlueprintRunsOrphanedAtMint(ctx context.Context, grace time.Duration) (int, error) {
	// One statement, no claim/conversation join to make: the absence of a
	// child IS the predicate. Duplicates the boot reconcile's SQL rather than
	// importing it, keeping this package dialect-independent like the rest of
	// the file (see pgUUIDArray's rationale).
	//
	// No conversation to park and no claim to release — a run with no child
	// has neither, which is exactly why nothing else recovers it. Nothing
	// touches the task either: freeing the index is what lets the task's
	// already-queued firing intent produce a fresh run on the next drain.
	res, err := s.db.ExecContext(ctx, `
		UPDATE blueprint_runs
		SET status = 'failed', completed_at = now(), abort_reason = $2
		WHERE status = 'running'
		  AND started_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM conversations c WHERE c.blueprint_run_id = blueprint_runs.id
		  )
	`, grace.Seconds(), domain.BlueprintAbortOrphanedAtMint)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *pgStore) CancelStrandedCuratorTurns(ctx context.Context, staleThreshold time.Duration) (int, error) {
	// A home is dead when its instances row is missing (GC'd or never
	// registered) or its heartbeat is older than the threshold — the same
	// missing-or-stale predicate ReapDeadExecutors uses for a run's executor.
	// An outcome/error release only: the messages ledger already holds
	// whatever the stranded turn streamed, and a turn whose home died never
	// reported a cost lump to settle.
	res, err := s.db.ExecContext(ctx, `
		UPDATE claims
		SET released_at = now(),
		    outcome = 'cancelled',
		    error = COALESCE(error, 'Cancelled: curator home executor lost (reaper) — re-send to re-home')
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
