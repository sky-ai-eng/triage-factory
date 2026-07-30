package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// runQueueStore is the SQLite impl of db.RunQueueStore — the durable run
// queue the delegation dispatcher drains. SQLite/local is single-worker (N=1),
// so ClaimNextRun doesn't need the FOR UPDATE SKIP LOCKED the Postgres impl
// uses; a short transaction pairing the status flip with the claims-row mint
// is atomic enough with one dispatcher.
type runQueueStore struct {
	conn *sql.DB
}

func newRunQueueStore(conn *sql.DB) db.RunQueueStore {
	return &runQueueStore{conn: conn}
}

var _ db.RunQueueStore = (*runQueueStore)(nil)

// runTerminalStatusesSQL is the canonical set of terminal conversation
// status values as a SQL IN-list body. The reconcile sweep and the
// orphaned-child cancel (blueprints.go) interpolate it instead of
// re-spelling the literal, so the "non-terminal child" predicate has one
// definition. Other queries that add dormant statuses
// (open/pending_approval) to this set keep their own list.
const runTerminalStatusesSQL = `'completed','failed','cancelled','task_unsolvable'`

// runQueueClaimCols is the column list ClaimNextRun returns, shared with the
// scan helper. visibility is left at its row default on enqueue; team_id is
// surfaced so the construction-path RunInfo built off a claimed run carries
// the owning team for the capture writers. The claim identity fields
// (ExecutorID/ClaimedAt/Attempts) are hydrated from the freshly minted
// claims row, not this projection.
const runQueueClaimCols = `id, org_id, task_id, COALESCE(prompt_id, ''), status, COALESCE(model, ''),
	COALESCE(worktree_path, ''), COALESCE(sdk_session_id, ''), trigger_type, COALESCE(trigger_id, ''),
	COALESCE(creator_user_id, ''), team_id, COALESCE(blueprint_run_id, ''), blueprint_step_index`

func (s *runQueueStore) EnqueueRun(ctx context.Context, orgID string, run domain.Conversation) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	if triggerType == "manual" && run.CreatorUserID == "" {
		run.CreatorUserID = runmode.LocalDefaultUserID
	}
	status := run.Status
	if status == "" {
		status = "queued"
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO conversations (id, type, runtime, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index,
		                  preferred_executor_id, queued_at)
		VALUES (?, 'delegation', 'sdk', ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?, NULLIF(?, ''), CURRENT_TIMESTAMP)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.CreatorUserID), nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx, run.PreferredExecutorID)
	return err
}

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, _ db.ClaimPlacement) (*domain.Conversation, error) {
	// Flip the oldest claimable queued run to 'running' and mint the claims
	// row that records this engagement's ownership, inside one short
	// transaction. Claimable = its owning blueprint_run is still 'running'
	// and not cancel-requested (a sequence-cancelled blueprint's queued step
	// is never claimed). A NULL subquery (nothing claimable) matches no row,
	// RETURNING yields nothing, and the scan reports ErrNoRows -> (nil, nil).
	//
	// The ClaimPlacement arg is ignored: SQLite is N=1 (local mode, always
	// role=all), so there is exactly one executor and the two-tier claim is
	// vacuous — every queued run's preferred_executor_id is either this one
	// instance (tier 1 self-hits) or NULL, and both resolve to "claim the
	// oldest". Placement is a multi-executor concern, Postgres-only.
	//
	// Per-org fairness + the max_concurrent_runs cap are likewise
	// Postgres-only and absent here: SQLite is one org and one executor, so the
	// fairness comparison is trivially won by the sole org and a single-process
	// semaphore already bounds local concurrency.
	//
	// An empty executorID (the un-wired test-spawner path) stores the ''
	// sentinel on the claim — claims.executor_id is NOT NULL by schema.
	var run *domain.Conversation
	claimedAt := time.Now().UTC()
	err := inTx(ctx, s.conn, func(q queryer) error {
		row := q.QueryRowContext(ctx, `
			UPDATE conversations
			SET status = 'running'
			WHERE id = (
				SELECT r.id FROM conversations r
				JOIN blueprint_runs br ON br.id = r.blueprint_run_id
				WHERE r.status = 'queued'
				  AND br.cancel_requested = 0
				  AND br.status = 'running'
				ORDER BY r.started_at, r.id
				LIMIT 1
			)
			RETURNING `+runQueueClaimCols)
		claimed, err := scanSqliteClaimedRun(row)
		if err != nil || claimed == nil {
			return err
		}
		// The claim id is minted here and handed back on the claimed run: the
		// executor needs to name this engagement at teardown, when it has
		// already been released and can no longer be found as the
		// conversation's active claim.
		claimID := uuid.New().String()
		if _, err := q.ExecContext(ctx, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, claimID, claimed.OrgID, claimed.ID, executorID, bootEpoch, claimedAt); err != nil {
			return err
		}
		claimed.ClaimID = claimID
		var attempts int
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM claims WHERE conversation_id = ?
		`, claimed.ID).Scan(&attempts); err != nil {
			return err
		}
		claimed.ExecutorID = executorID
		claimed.ClaimedAt = &claimedAt
		claimed.Attempts = attempts
		run = claimed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Guarded on 'running' — the one status a claimed run holds from claim
	// until terminal/park, whatever setup phase its claim is in — matching
	// the Postgres impl, so a stale requeue can't resurrect a
	// terminal/parked/queued row. The active claim releases as 'requeued'
	// (the claim already counted this try — Attempts is the claim count),
	// and the placement stamp clears: a queued row has no owner.
	return inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = 'queued', queued_at = CURRENT_TIMESTAMP, result_summary = ?,
				preferred_executor_id = NULL
			WHERE id = ? AND status = 'running'
		`, lastErr, runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		return releaseActiveClaim(ctx, q, runID, "requeued")
	})
}

// MarkAwaitingCredentials mirrors the Postgres impl: the phase park and
// sidecar pubkey land on the ACTIVE claim in one statement (the
// conversation stays 'running'), guarded on phase IS NULL so a duplicate
// can't re-park or overwrite the key. No ctlbus doorbell — the tf_ctl
// fabric is Postgres-only (local mode has no LISTEN/NOTIFY), and never
// reached in practice anyway: local mode is always role=all, which never
// parks a run awaiting credentials.
func (s *runQueueStore) MarkAwaitingCredentials(ctx context.Context, orgID, runID, credPubKey string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET phase = 'awaiting_credentials', cred_pubkey = NULLIF(?, '')
		WHERE conversation_id = ? AND released_at IS NULL AND phase IS NULL
	`, credPubKey, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// awaitingCredentialsCols is the shared projection for the claim-identity
// reads: the conversation joined to its active (unreleased) claim.
// claimed_at/started_at are selected bare and coalesced in Go: wrapping
// them in COALESCE strips the declared column type the driver needs to
// hand back a time.Time, so the scan would see a raw string.
const awaitingCredentialsCols = `r.id, r.org_id, COALESCE(r.team_id, ''), COALESCE(r.task_id, ''),
	COALESCE(cl.executor_id, ''), COALESCE(cl.boot_epoch, 0), cl.claimed_at, r.started_at,
	COALESCE(cl.cred_pubkey, '')`

func (s *runQueueStore) GetClaim(ctx context.Context, orgID, runID string) (db.AwaitingCredentialsRun, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.AwaitingCredentialsRun{}, false, err
	}
	var (
		r         db.AwaitingCredentialsRun
		claimedAt sql.NullTime
		startedAt time.Time
	)
	err := s.conn.QueryRowContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		LEFT JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE r.org_id = ? AND r.id = ?
	`, orgID, runID).Scan(&r.RunID, &r.OrgID, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey)
	if err == sql.ErrNoRows {
		return db.AwaitingCredentialsRun{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsRun{}, false, err
	}
	r.ClaimedAt = startedAt
	if claimedAt.Valid {
		r.ClaimedAt = claimedAt.Time
	}
	return r, true, nil
}

// RequeueAwaitingCredentials mirrors the Postgres impl: releases a run
// whose active claim is parked in phase='awaiting_credentials' back to
// 'queued', releasing that claim. Never reached in practice — local mode is
// always role=all, which never parks a run awaiting credentials — but
// implemented for store-interface + conformance-test symmetry.
func (s *runQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var matched bool
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = 'queued', queued_at = CURRENT_TIMESTAMP,
				preferred_executor_id = NULL
			WHERE id = ? AND EXISTS (
			    SELECT 1 FROM claims cl
			    WHERE cl.conversation_id = conversations.id
			      AND cl.released_at IS NULL AND cl.phase = 'awaiting_credentials'
			)
		`, runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		matched = n > 0
		if !matched {
			return nil
		}
		return releaseActiveClaim(ctx, q, runID, "requeued")
	})
	return matched, err
}

func (s *runQueueStore) ListAwaitingCredentials(ctx context.Context) ([]db.AwaitingCredentialsRun, error) {
	// The parked set is keyed off the active claim's phase (served by the
	// idx_claims_active_phase partial index); an inner join is right here —
	// a parked run by definition has the claim.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE cl.phase = 'awaiting_credentials'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAwaitingCredentialsRuns(rows)
}

func (s *runQueueStore) ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]db.AwaitingCredentialsRun, error) {
	// Local mode has no claim_credentials table (the bundle channel is
	// Postgres-only; local reads the live secret store), so there is never
	// a sealed bundle to refresh.
	return nil, nil
}

func scanAwaitingCredentialsRuns(rows *sql.Rows) ([]db.AwaitingCredentialsRun, error) {
	var out []db.AwaitingCredentialsRun
	for rows.Next() {
		// Same bare-column + Go-side coalesce as GetClaim — see the comment
		// on awaitingCredentialsCols.
		var (
			r         db.AwaitingCredentialsRun
			claimedAt sql.NullTime
			startedAt time.Time
		)
		if err := rows.Scan(&r.RunID, &r.OrgID, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey); err != nil {
			return nil, err
		}
		r.ClaimedAt = startedAt
		if claimedAt.Valid {
			r.ClaimedAt = claimedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *runQueueStore) ResetProcessingRuns(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// The boot self-sweep, ownership-scoped through claims: only ACTIVE
	// claims this instance itself holds (executor_id = ?) from a strictly
	// earlier boot (boot_epoch < ?) on 'running' conversations (claimed —
	// mid-setup or executing; setup progress is claim phase, never status)
	// are released ('reaped'), and their conversations flipped back to
	// 'queued' for re-claim. Terminal + dormant (open/pending_approval) +
	// already-queued conversations are not 'running' and stay untouched —
	// dormant runs resume through their own
	// paths, not the queue. SQLite is N=1, so there is never a live sibling
	// to protect, but the same predicate keeps the two backends' semantics
	// identical.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT cl.id, cl.conversation_id
			FROM claims cl
			JOIN conversations r ON r.id = cl.conversation_id
			WHERE cl.released_at IS NULL
			  AND cl.executor_id = ?
			  AND cl.boot_epoch < ?
			  AND r.status = 'running'
			  AND r.blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		`, executorID, bootEpoch)
		if err != nil {
			return err
		}
		var claimIDs, convIDs []string
		for rows.Next() {
			var claimID, convID string
			if err := rows.Scan(&claimID, &convID); err != nil {
				rows.Close()
				return err
			}
			claimIDs = append(claimIDs, claimID)
			convIDs = append(convIDs, convID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(convIDs) == 0 {
			return nil
		}
		now := time.Now().UTC()
		for i := range claimIDs {
			if _, err := q.ExecContext(ctx, `
				UPDATE claims SET released_at = ?, outcome = 'reaped' WHERE id = ?
			`, now, claimIDs[i]); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, `
				UPDATE conversations SET status = 'queued', queued_at = CURRENT_TIMESTAMP,
					preferred_executor_id = NULL
				WHERE id = ?
			`, convIDs[i]); err != nil {
				return err
			}
		}
		count = len(convIDs)
		return nil
	})
	return count, err
}

func (s *runQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// SQLite is N=1 — at most the one local org has any rows — but the shape
	// mirrors the Postgres impl so the conformance suite runs identically. No
	// FILTER (portability); SUM(CASE ...) is the equivalent. A NULL or
	// non-positive max_concurrent_runs maps to a nil cap (unlimited).
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id,
			       SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END) AS active,
			       SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END)  AS queued
			FROM conversations
			WHERE status IN ('running', 'queued')
			GROUP BY org_id
		) counts
		LEFT JOIN org_settings os ON os.org_id = counts.org_id
		ORDER BY (counts.active + counts.queued) DESC, counts.org_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []db.OrgQueueShare
	for rows.Next() {
		var (
			share   db.OrgQueueShare
			maxRuns sql.NullInt64
		)
		if err := rows.Scan(&share.OrgID, &share.Active, &share.Queued, &maxRuns); err != nil {
			return nil, err
		}
		if maxRuns.Valid && maxRuns.Int64 > 0 {
			v := int(maxRuns.Int64)
			share.MaxConcurrentRuns = &v
		}
		out = append(out, share)
	}
	return out, rows.Err()
}

func (s *runQueueStore) ReconcileOrphanedRuns(ctx context.Context) (int, error) {
	// Boot self-heal: cancel child runs left non-terminal under a
	// blueprint_run that is already terminal. This is the mirror of
	// ResetProcessingRuns (which requeues active runs under a *running* parent):
	// a child alive under a terminal parent will never be claimed (ClaimNextRun
	// gates on a running parent) nor reset, so without this it sits 'running'
	// forever — the dispatcher treats it as live work and its worktree pins the
	// feature branch, requeuing any sibling fetch into a forever-failing loop.
	// Any active claim on a cancelled row releases as 'cancelled' with it.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations
			SET status = 'cancelled',
			    completed_at = COALESCE(completed_at, ?),
			    stop_reason = COALESCE(stop_reason, 'blueprint_terminal'),
			    result_summary = COALESCE(NULLIF(result_summary, ''), ?)
			WHERE status NOT IN (`+runTerminalStatusesSQL+`)
			  AND blueprint_run_id IN (
			      SELECT id FROM blueprint_runs
			      WHERE status IN ('completed','aborted','failed','cancelled')
			  )
		`, time.Now().UTC(), "Cancelled: owning blueprint run reached a terminal state")
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		count = int(n)
		if count == 0 {
			return nil
		}
		_, err = q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = 'cancelled'
			WHERE released_at IS NULL
			  AND conversation_id IN (
			      SELECT id FROM conversations
			      WHERE status = 'cancelled'
			        AND blueprint_run_id IN (
			            SELECT id FROM blueprint_runs
			            WHERE status IN ('completed','aborted','failed','cancelled')
			        )
			  )
		`, time.Now().UTC())
		return err
	})
	if err != nil {
		return 0, err
	}

	// Claim-desync janitor arms — the SQLite mirror of the Postgres twin's
	// healClaimDesyncs (see internal/db/postgres/run_queue.go for the two
	// stranded shapes and why they exist). SQLite's single connection makes
	// the non-System terminal writes atomic, so these arms are conformance
	// symmetry here rather than a live hazard; runs after the
	// blueprint-terminal cancel above so a just-cancelled row reads terminal
	// and is never requeued.
	err = inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?,
			    outcome = CASE (SELECT c.status FROM conversations c WHERE c.id = claims.conversation_id)
			        WHEN 'completed' THEN 'completed'
			        WHEN 'cancelled' THEN 'cancelled'
			        ELSE 'failed'
			    END
			WHERE released_at IS NULL
			  AND conversation_id IN (
			      SELECT id FROM conversations WHERE status IN (`+runTerminalStatusesSQL+`)
			  )
		`, time.Now().UTC())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		count += int(n)

		res, err = q.ExecContext(ctx, `
			UPDATE conversations SET status = 'queued', queued_at = CURRENT_TIMESTAMP,
			    preferred_executor_id = NULL
			WHERE type = 'delegation'
			  AND status = 'running'
			  AND NOT EXISTS (
			      SELECT 1 FROM claims cl
			      WHERE cl.conversation_id = conversations.id AND cl.released_at IS NULL
			  )
			  AND blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		count += int(n)
		return nil
	})
	return count, err
}

func (s *runQueueStore) CountQueuedSystem(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE status = 'queued'`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// runTimingClaimCols selects the claim-derived timing fields: duration_ms
// is the SUM of the per-engagement telemetry, then the latest claim's
// identity. claimed_at loses its declared column type inside the subselect,
// so it scans as text and parses via parseDBDatetime.
const runTimingClaimCols = `
	(SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = conversations.id),
	(SELECT cl.executor_id FROM claims cl WHERE cl.conversation_id = conversations.id ORDER BY cl.claimed_at DESC, cl.rowid DESC LIMIT 1),
	(SELECT MAX(cl.claimed_at) FROM claims cl WHERE cl.conversation_id = conversations.id)`

func (s *runQueueStore) RecentRunTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.RunTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT org_id, status, COALESCE(failure_kind, ''),
		       started_at, completed_at, `+runTimingClaimCols+`
		FROM conversations
		WHERE type = 'delegation' AND started_at >= ?
		ORDER BY started_at DESC
		LIMIT ?
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteRunTimings(rows)
}

func (s *runQueueStore) QueuedRunAgesSystem(ctx context.Context) ([]domain.QueuedRun, error) {
	return s.queuedRunAges(ctx, "")
}

func (s *runQueueStore) QueuedRunAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedRun, error) {
	return s.queuedRunAges(ctx, orgID)
}

func (s *runQueueStore) queuedRunAges(ctx context.Context, orgID string) ([]domain.QueuedRun, error) {
	q := `SELECT org_id, started_at, COALESCE(preferred_executor_id, '')
	      FROM conversations WHERE status = 'queued'`
	args := []any{}
	if orgID != "" {
		q += ` AND org_id = ?`
		args = append(args, orgID)
	}
	q += ` ORDER BY started_at ASC`
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.QueuedRun
	for rows.Next() {
		var qr domain.QueuedRun
		if err := rows.Scan(&qr.OrgID, &qr.EnqueuedAt, &qr.PreferredExecutor); err != nil {
			return nil, err
		}
		out = append(out, qr)
	}
	return out, rows.Err()
}

func (s *runQueueStore) RecentRunTimingsForOrgSystem(ctx context.Context, orgID string, since, until time.Time, limit int) ([]domain.RunTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	// Raw string comparison on started_at — the house convention for the
	// conversations table (ClaimNextRun orders/filters started_at raw); both
	// the stored CURRENT_TIMESTAMP value and the bound serialize sortably.
	q := `SELECT org_id, status, COALESCE(failure_kind, ''),
	             started_at, completed_at, ` + runTimingClaimCols + `
	      FROM conversations WHERE type = 'delegation' AND org_id = ? AND started_at >= ?`
	args := []any{orgID, since.UTC()}
	if !until.IsZero() {
		q += ` AND started_at < ?`
		args = append(args, until.UTC())
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteRunTimings(rows)
}

// executorClaimCols is the shared projection behind both operator claim reads,
// so the per-executor list and the single-claim lookup can never drift into
// disagreeing about the same row. LEFT JOIN on the conversation: the claim is
// the subject here, and a claim whose conversation is gone must still report
// its measured cost rather than vanishing from the box's occupancy.
//
// No org predicate, and none is possible: this is the deployment-wide operator
// read. SQLite is N=1, so the distinction is moot locally — the arm exists
// because the store is one dual-dialect contract with one conformance suite.
const executorClaimCols = `
	SELECT c.id, c.org_id, c.conversation_id,
	       c.claimed_at, c.released_at, COALESCE(c.outcome, ''),
	       c.peak_mem_mb, c.cpu_usec,
	       COALESCE(v.status, ''), COALESCE(v.failure_kind, '')
	FROM claims c
	LEFT JOIN conversations v ON v.id = c.conversation_id`

func (s *runQueueStore) RecentClaimsForExecutorSystem(ctx context.Context, executorID string, limit int) ([]domain.ExecutorClaim, error) {
	if limit <= 0 {
		limit = 25
	}
	// Tie-break on id so a batch of claims minted in the same instant orders
	// deterministically across repeated polls — the console refetches on a
	// timer and a shuffling table reads as churn that isn't happening.
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+`
		WHERE c.executor_id = ?
		ORDER BY c.claimed_at DESC, c.id DESC
		LIMIT ?
	`, executorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSqliteExecutorClaims(rows)
}

func (s *runQueueStore) ClaimByIDSystem(ctx context.Context, claimID string) (*domain.ExecutorClaim, error) {
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+` WHERE c.id = ?`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanSqliteExecutorClaims(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func scanSqliteExecutorClaims(rows *sql.Rows) ([]domain.ExecutorClaim, error) {
	var out []domain.ExecutorClaim
	for rows.Next() {
		var c domain.ExecutorClaim
		var releasedAt sql.NullTime
		var peakMem, cpuUsec sql.NullInt64
		if err := rows.Scan(
			&c.ID, &c.OrgID, &c.ConversationID,
			&c.ClaimedAt, &releasedAt, &c.Outcome,
			&peakMem, &cpuUsec, &c.Status, &c.FailureKind,
		); err != nil {
			return nil, err
		}
		if releasedAt.Valid {
			v := releasedAt.Time
			c.ReleasedAt = &v
		}
		c.PeakMemMB = intPtrFromNull(peakMem)
		c.CPUUsec = int64PtrFromNull(cpuUsec)
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanSqliteRunTimings(rows *sql.Rows) ([]domain.RunTiming, error) {
	var out []domain.RunTiming
	for rows.Next() {
		var t domain.RunTiming
		var executorID, claimedAt sql.NullString
		var completedAt sql.NullTime
		var durationMS sql.NullInt64
		if err := rows.Scan(
			&t.OrgID, &t.Status, &t.FailureKind,
			&t.StartedAt, &completedAt, &durationMS, &executorID, &claimedAt,
		); err != nil {
			return nil, err
		}
		t.ExecutorID = executorID.String
		if claimedAt.Valid && claimedAt.String != "" {
			at, err := parseDBDatetime(claimedAt.String)
			if err != nil {
				return nil, err
			}
			t.ClaimedAt = &at
		}
		if completedAt.Valid {
			v := completedAt.Time
			t.CompletedAt = &v
		}
		if durationMS.Valid {
			v := int(durationMS.Int64)
			t.DurationMS = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanSqliteClaimedRun scans a claimed conversations row into
// *domain.Conversation. (nil, nil) on sql.ErrNoRows so callers treat "nothing
// claimable" as a non-error empty result.
func scanSqliteClaimedRun(row *sql.Row) (*domain.Conversation, error) {
	var (
		r       domain.Conversation
		stepIdx sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.TaskID, &r.PromptID, &r.Status, &r.Model,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.BlueprintRunID, &stepIdx)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stepIdx.Valid {
		v := int(stepIdx.Int64)
		r.BlueprintStepIndex = &v
	}
	return &r, nil
}
