package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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

// runTerminalStatusesSQL is the terminal conversation statuses as a SQL
// IN-list body — two names, one owner each: the agent concluded, or the
// infrastructure died. It describes stored rows as faithfully as new writes,
// because every retired status was rewritten by migration rather than carried
// forward (202608010002, SQLite; Postgres had no rows to migrate). Mirrors
// domain.AllTerminalRunStatuses.
//
// Every exclusion predicate in this package interpolates this rather than
// re-spelling the literals. That matters more than the saved keystrokes: these
// guards are exclusions (`status NOT IN (…)`), so a status missing from one
// doesn't fail closed — it readmits a finished run to parking, cancelling, or
// the active-work counters. Sixteen hand-copied copies is how the set drifted
// a value at a time.
const runTerminalStatusesSQL = `'completed','failed'`

// --- The needs-driving predicate ---------------------------------------
//
// The SQLite mirror of the Postgres fragments — see
// internal/db/postgres/run_queue.go for the model. Stored conversation
// status is outcome-or-nothing ('open' | a terminal | NULL); "queued" and
// "running" are derived from the claim table and these predicates, never
// stored. Every fragment is written against the conversation alias `r`.

// activeClaimExistsSQL is the derived "running": an unreleased claim is the
// engagement driving this conversation. Served by idx_claims_one_active.
const activeClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL)`

// undeliveredInputExistsSQL matches drivable input: a plain user message
// still awaiting delivery. Injections ride whatever engagement runs next and
// never wake one on their own; a withdrawn row (undelivered + window_state
// 'inactive') never happened. Nothing here reads `seq`, which compaction
// re-writes to fractional values. Served by idx_messages_undelivered.
const undeliveredInputExistsSQL = `EXISTS (
		SELECT 1 FROM messages m_i
		WHERE m_i.conversation_id = r.id AND m_i.delivered = 0
		  AND m_i.role = 'user' AND m_i.subtype = '' AND m_i.window_state = 'active')`

// needsDrivingSQL is the eligibility predicate, identical for every surface:
// nobody is driving it, it has not been retired, and it is either mid-flight
// (fresh mint, or a claim that released without writing an outcome) or
// parked and woken by new input. A terminal conversation is never eligible.
const needsDrivingSQL = `r.archived_at IS NULL
	  AND NOT ` + activeClaimExistsSQL + `
	  AND (r.status IS NULL OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `))`

// curatorNeedsTurnSQL is the curator type-conditional gate: a curator
// conversation's only unit of work is a user turn, so "mid-flight with
// nothing queued" is a finished transcript waiting for its next message.
const curatorNeedsTurnSQL = `(r.type <> 'curator' OR ` + undeliveredInputExistsSQL + `)`

// curatorHomedHereSQL is the second half of the curator gate: homing owns
// which executor runs a project's turns. A project with no home row is
// claimable by anyone, which is the local/role=all shape — one process,
// nothing to home to, so curator_homes is never populated and this arm is
// vacuously true. It exists for dialect parity: both backends run the same
// conformance suite over the same predicate.
const curatorHomedHereSQL = `(r.type <> 'curator'
		OR NOT EXISTS (SELECT 1 FROM curator_homes h
		               WHERE h.org_id = r.org_id AND h.project_id = r.project_id)
		OR EXISTS (SELECT 1 FROM curator_homes h
		           WHERE h.org_id = r.org_id AND h.project_id = r.project_id
		             AND h.home_instance_id = ?))`

// eligibleForDrivingSQL is the surface-agnostic "waiting to be driven" —
// what the queue-depth counters and the display projection's derived
// `queued` rung read.
const eligibleForDrivingSQL = needsDrivingSQL + ` AND ` + curatorNeedsTurnSQL

// blueprintDrivableSQL is the delegation arm's gate, applied over a LEFT
// JOIN so a conversation with no blueprint parent (curator today,
// interactive tomorrow) is not filtered out by the join itself. The rest —
// why a called-off blueprint drives nothing and is checked on both of its
// columns, and why a blueprint drives only the one conversation its
// `current_step_index` names, whatever its status — is the Postgres twin's;
// this is the same predicate in the other dialect.
const blueprintDrivableSQL = `(r.blueprint_run_id IS NULL
	    OR (br.cancel_requested = 0 AND br.status <> 'cancelled'
	        AND r.blueprint_step_index = br.current_step_index))`

// curatorTurnMessageSQL is the queued turn a curator claim is minted to
// drive — the conversation's oldest undelivered plain user row, stamped as
// the claim's mint intent. NULL for every other type.
const curatorTurnMessageSQL = `(SELECT MIN(m_t.id) FROM messages m_t
		WHERE m_t.conversation_id = r.id AND m_t.delivered = 0
		  AND m_t.role = 'user' AND m_t.subtype = '' AND m_t.window_state = 'active')`

// handedBackOutcomesSQL / episodeAttemptsSQL count the claim being minted
// within the conversation's CURRENT queue episode — the run of consecutive
// claims handed back without the engagement ever recording anything. The
// Postgres twin carries the model; this is the same predicate in the other
// dialect.
const handedBackOutcomesSQL = `'requeued','reaped'`

const episodeAttemptsSQL = `
	SELECT COUNT(*) + 1 FROM claims c2
	WHERE c2.conversation_id = ?
	  AND c2.outcome IN (` + handedBackOutcomesSQL + `)
	  AND NOT EXISTS (
	      SELECT 1 FROM claims c3
	      WHERE c3.conversation_id = c2.conversation_id
	        AND c3.outcome IS NOT NULL
	        AND c3.outcome NOT IN (` + handedBackOutcomesSQL + `)
	        AND c3.claimed_at >= COALESCE(c2.released_at, c2.claimed_at))`

// runQueueClaimCols is the column list ClaimNextRun returns, shared with the
// scan helper. visibility is left at its row default on enqueue; team_id is
// surfaced so the construction-path RunInfo built off a claimed run carries
// the owning team for the capture writers. The claim identity fields
// (ExecutorID/ClaimedAt/Attempts) are hydrated from the freshly minted
// claims row, not this projection.
const runQueueClaimCols = `r.id, r.org_id, COALESCE(r.type, ''), COALESCE(r.task_id, ''), COALESCE(r.prompt_id, ''),
	COALESCE(r.model, ''), COALESCE(r.runtime, ''),
	COALESCE(r.worktree_path, ''), COALESCE(r.sdk_session_id, ''), r.trigger_type, COALESCE(r.trigger_id, ''),
	COALESCE(r.creator_user_id, ''), COALESCE(r.team_id, ''), COALESCE(r.project_id, ''),
	COALESCE(r.blueprint_run_id, ''), r.blueprint_step_index,
	` + curatorTurnMessageSQL

// EnqueueRun mints a delegation conversation with NO status — the absence of
// an outcome is what makes it claimable, so the mint writes nothing to the
// column and queued_at carries the enqueue moment. runtime is stamped
// 'sdk': SQLite is local mode, which keeps the Claude Code SDK runtime.
// The Postgres sibling stamps 'native' — the dialect IS the mode, so the
// split lands where the row is written rather than as a caller-passed knob.
func (s *runQueueStore) EnqueueRun(ctx context.Context, orgID string, run domain.Conversation) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if err := db.AssertBlueprintStepIndexed(run); err != nil {
		return err
	}
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	if triggerType == "manual" && run.CreatorUserID == "" {
		run.CreatorUserID = runmode.LocalDefaultUserID
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO conversations (id, type, runtime, task_id, prompt_id, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index,
		                  preferred_executor_id, queued_at)
		VALUES (?, 'delegation', 'sdk', ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?, NULLIF(?, ''), CURRENT_TIMESTAMP)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.CreatorUserID), nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx, run.PreferredExecutorID)
	return err
}

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, _ db.ClaimPlacement) (*domain.Conversation, error) {
	// One scan, every surface: pick the oldest conversation matching the
	// needs-driving predicate (plus the type-conditional blueprint gate — a
	// sequence-cancelled blueprint's step is never claimed) and mint the
	// claims row that records this engagement's ownership, inside one short
	// transaction. Nothing on the conversation row changes: the claim IS the
	// ownership. No candidate means no row scanned, which the scan helper
	// reports as (nil, nil).
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
			SELECT `+runQueueClaimCols+`
			FROM conversations r
			LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			WHERE `+eligibleForDrivingSQL+`
			  AND `+blueprintDrivableSQL+`
			  AND `+curatorHomedHereSQL+`
			ORDER BY r.started_at, r.id
			LIMIT 1`, executorID)
		claimed, err := scanSqliteClaimedRun(row)
		if err != nil || claimed == nil {
			return err
		}
		// The claim id is minted here and handed back on the claimed run: the
		// executor needs to name this engagement at teardown, when it has
		// already been released and can no longer be found as the
		// conversation's active claim.
		// Un-park: a claim taken on the `open` arm of the predicate ends the
		// park by definition, so the row goes back to mid-flight (NULL).
		// That keeps "parked" and "an engagement is driving this" disjoint at
		// every instant, which is what every recovery guard downstream reads.
		//
		// All THREE park columns clear together, and park_reason is the one
		// that bites if it doesn't: it answers "why is this parked", so on a
		// row that is no longer parked it is not history, it is a wrong
		// answer. Left behind, it rides through this engagement onto whatever
		// terminal follows — and the run station prints it beside a failed
		// run as "stopped by user" on a run nobody stopped.
		if _, err := q.ExecContext(ctx, `
			UPDATE conversations SET status = NULL, parked_at = NULL, park_reason = NULL
			WHERE id = ? AND status IS NOT NULL
		`, claimed.ID); err != nil {
			return err
		}
		claimID := uuid.New().String()
		var msgID any
		if claimed.ClaimMessageID != 0 {
			msgID = claimed.ClaimMessageID
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, message_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, claimID, claimed.OrgID, claimed.ID, executorID, bootEpoch, claimedAt, msgID); err != nil {
			return err
		}
		claimed.ClaimID = claimID
		var attempts int
		if err := q.QueryRowContext(ctx, episodeAttemptsSQL, claimed.ID).Scan(&attempts); err != nil {
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
	// Releasing the claim IS the requeue: the conversation stays mid-flight
	// (status NULL), so the moment its last claim releases it matches the
	// needs-driving predicate again. Guarded on NULL status, matching the
	// Postgres impl, so a stale requeue can't resurrect a terminal or parked
	// row. The claim releases as 'requeued' (it already counted this try —
	// Attempts is the claim count), and the placement stamp clears: an
	// unclaimed row has no owner.
	return inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET result_summary = ?, preferred_executor_id = NULL
			WHERE id = ? AND status IS NULL
			  AND EXISTS (SELECT 1 FROM claims cl
			              WHERE cl.conversation_id = conversations.id AND cl.released_at IS NULL)
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
const awaitingCredentialsCols = `r.id, r.org_id, COALESCE(r.type, ''), COALESCE(r.team_id, ''), COALESCE(r.task_id, ''),
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
	`, orgID, runID).Scan(&r.RunID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey)
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

// RequeueAwaitingCredentials mirrors the Postgres impl: releases the claim
// of a run parked in phase='awaiting_credentials', which leaves the
// (still mid-flight) conversation claimable again. Never reached in
// practice — local mode is always role=all, which never parks a run
// awaiting credentials — but implemented for store-interface +
// conformance-test symmetry.
func (s *runQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var matched bool
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations SET preferred_executor_id = NULL
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
	// a parked claim by definition exists. No type filter: parking is a
	// property of the engagement, not the surface.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE cl.phase = 'awaiting_credentials'
		ORDER BY cl.claimed_at ASC, cl.id ASC
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
		if err := rows.Scan(&r.RunID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &claimedAt, &startedAt, &r.CredPubKey); err != nil {
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
	// earlier boot (boot_epoch < ?) on mid-flight conversations (status NULL
	// — nothing wrote an outcome, so the engagement was cut off) are
	// released ('reaped'). The release IS the requeue. Parked (`open`) and
	// terminal conversations are not mid-flight and stay untouched. SQLite is
	// N=1, so there is never a live sibling to protect, but the same
	// predicate keeps the two backends' semantics identical.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		rows, err := q.QueryContext(ctx, `
			SELECT cl.id, cl.conversation_id
			FROM claims cl
			JOIN conversations r ON r.id = cl.conversation_id
			WHERE cl.released_at IS NULL
			  AND cl.executor_id = ?
			  AND cl.boot_epoch < ?
			  AND r.status IS NULL
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
				UPDATE conversations SET preferred_executor_id = NULL WHERE id = ?
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
	// mirrors the Postgres impl so the conformance suite runs identically:
	// active is the org's unreleased claims, queued its conversations
	// matching the needs-driving predicate. A NULL or non-positive
	// max_concurrent_runs maps to a nil cap (unlimited).
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id, SUM(active) AS active, SUM(queued) AS queued
			FROM (
				SELECT org_id, 1 AS active, 0 AS queued FROM claims WHERE released_at IS NULL
				UNION ALL
				SELECT r.org_id, 0, 1 FROM conversations r WHERE `+eligibleForDrivingSQL+`
			)
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
	// Boot self-heal: park child runs left mid-flight under a blueprint_run
	// that is already terminal. This is the mirror of ResetProcessingRuns
	// (which requeues active runs under a *running* parent): a child alive
	// under a terminal parent will never be claimed (ClaimNextRun gates on a
	// running parent) nor reset, so without this it sits mid-flight forever —
	// the dispatcher treats it as live work and its worktree pins the feature
	// branch, requeuing any sibling fetch into a forever-failing loop.
	//
	// `open`, not a terminal: nothing about an orphan failed, and nothing
	// about it concluded either. Read the park as "stopped without
	// concluding", NOT as "resumable" — its blueprint is terminal, so the
	// claim gate refuses it and no resume path will wake it. The scope is
	// status IS NULL alone (mid-flight is exactly what "still looks live"
	// means); an already-parked orphan is already in the state this writes,
	// and re-stamping it every boot would just churn the row and its
	// snapshot-retention clock.
	// Any active claim on a parked row releases as 'cancelled' with it — the
	// engagement was ended from outside, which is what the claim outcome
	// vocabulary calls a cancellation.
	var count int
	err := inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE conversations
			SET status = 'open',
			    parked_at = COALESCE(parked_at, ?),
			    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
			    result_summary = COALESCE(NULLIF(result_summary, ''), ?)
			WHERE status IS NULL
			  AND blueprint_run_id IN (
			      SELECT id FROM blueprint_runs
			      WHERE status IN ('completed','aborted','failed','cancelled')
			  )
		`, time.Now().UTC(), "Stopped: owning blueprint run reached a terminal state")
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
			      WHERE status = 'open'
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

	// Claim-desync janitor arm — the SQLite mirror of the Postgres twin's
	// healClaimDesyncs (see internal/db/postgres/run_queue.go for the one
	// stranded shape that survives the derived model). SQLite's single
	// connection makes the non-System terminal writes atomic, so this is
	// conformance symmetry here rather than a live hazard; it runs after the
	// blueprint-terminal park above, whose own claim release it therefore
	// never has to redo.
	err = inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?,
			    outcome = CASE (SELECT c.status FROM conversations c WHERE c.id = claims.conversation_id)
			        WHEN 'completed' THEN 'completed'
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

		return nil
	})
	if err != nil {
		return count, err
	}

	// Mint-crash arm — the mirror of the blueprint-terminal park above, one
	// level up: that arm heals a live child under a dead parent, this one heals
	// a live parent with no child at all. The firing path commits the
	// blueprint_run first and enqueues its first step second, so a hard death
	// between the two leaves a 'running' parent that nothing drives and nothing
	// recovers — every other arm here (and the Postgres-only leader reaper)
	// joins through conversations, and this shape has none.
	//
	// Local mode has the crash window and no reaper, so boot is its only
	// recovery surface. It has no one-active-auto-run index either, so the
	// Postgres livelock this un-sticks isn't the local symptom; the local
	// symptom is a parent that reads in-flight forever. Failing frees both.
	//
	// Two clocks here on purpose, and the split is the opposite way round from
	// the Postgres arm's all-now() statement:
	//
	// The GRACE runs on SQLite's own clock — datetime() on both sides, so the
	// comparison survives whichever on-disk timestamp shape started_at carries
	// (see parseDBDatetime) rather than only the CURRENT_TIMESTAMP one both
	// insert paths write today.
	//
	// completed_at is bound from Go, like every other writer of this column
	// (MarkRunStatus, CreateRun) and of conversations.completed_at. What the
	// Postgres arm's now() buys is protection from DB/app clock skew, and an
	// embedded engine has none to protect against — it reads the same host
	// clock this process does. What the column does have is a text format:
	// _time_format=sqlite serializes a Go bind as
	// "2006-01-02 15:04:05.999999999-07:00", and datetime('now') would write a
	// second shape into a column where every other row carries the first, for
	// no gain.
	err = inTx(ctx, s.conn, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE blueprint_runs
			SET status = 'failed', completed_at = ?, abort_reason = ?
			WHERE status = 'running'
			  AND datetime(started_at) < datetime('now', ?)
			  AND NOT EXISTS (
			      SELECT 1 FROM conversations c WHERE c.blueprint_run_id = blueprint_runs.id
			  )
		`, time.Now().UTC(), domain.BlueprintAbortOrphanedAtMint,
			fmt.Sprintf("-%d seconds", int(domain.BlueprintOrphanedAtMintGrace.Seconds())))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
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
	err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r WHERE `+eligibleForDrivingSQL).Scan(&n)
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

// runTimingStatusSQL is the timing projection's status: the stored outcome
// when there is one, else the derived in-flight state. The percentile read
// buckets by failure kind, so a mid-flight row must still name itself
// rather than scan as NULL.
const runTimingStatusSQL = `COALESCE(status, CASE WHEN EXISTS (
		SELECT 1 FROM claims cl WHERE cl.conversation_id = conversations.id AND cl.released_at IS NULL)
	THEN 'running' ELSE 'queued' END)`

func (s *runQueueStore) RecentRunTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.RunTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT org_id, `+runTimingStatusSQL+`, COALESCE(failure_kind, ''),
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
	q := `SELECT r.org_id, r.started_at, COALESCE(r.preferred_executor_id, '')
	      FROM conversations r WHERE ` + eligibleForDrivingSQL
	args := []any{}
	if orgID != "" {
		q += ` AND r.org_id = ?`
		args = append(args, orgID)
	}
	q += ` ORDER BY r.started_at ASC`
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
	q := `SELECT org_id, ` + runTimingStatusSQL + `, COALESCE(failure_kind, ''),
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
	       COALESCE(v.status, CASE WHEN c.released_at IS NULL THEN 'running' ELSE 'queued' END, ''),
	       COALESCE(v.failure_kind, '')
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

// scanSqliteClaimedRun scans a claim candidate row into
// *domain.Conversation. (nil, nil) on sql.ErrNoRows so callers treat "nothing
// claimable" as a non-error empty result. Status is deliberately left empty:
// a claimable conversation's stored status is NULL by construction, and the
// dispatcher branches on Type and Runtime, never on it.
func scanSqliteClaimedRun(row *sql.Row) (*domain.Conversation, error) {
	var (
		r         domain.Conversation
		stepIdx   sql.NullInt64
		turnMsgID sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.Type, &r.TaskID, &r.PromptID, &r.Model, &r.Runtime,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.ProjectID, &r.BlueprintRunID, &stepIdx, &turnMsgID)
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
	r.ClaimMessageID = turnMsgID.Int64
	return &r, nil
}
