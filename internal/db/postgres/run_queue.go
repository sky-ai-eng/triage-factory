package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wakebus"
)

// runQueueStore is the Postgres impl of db.RunQueueStore — the durable run
// queue the delegation dispatcher drains. Wired against the admin pool in
// postgres.New: the dispatcher is a system worker with no per-user identity.
// The RLS policies are defense-in-depth (admin bypasses them) and org_id
// is bound where it is known. Ownership state lives on claims: a claim row
// is minted at ClaimNextRun and released (requeued/reaped) by the recovery
// paths, all inside the claiming statement's data-modifying CTEs so a crash
// can't separate the status flip from the claim bookkeeping.
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

// EnqueueRun mints a delegation conversation with NO status — the absence of
// an outcome is what makes it claimable, so the mint writes nothing to the
// column and queued_at carries the enqueue moment. runtime is stamped
// 'native' here and 'sdk' in the SQLite sibling: the dialect IS the mode
// (Postgres is multi-only, SQLite is local-only), so the split lands where
// the row is written rather than as a caller-passed knob. The stamp is a
// one-way ratchet — a conversation already carrying 'sdk' keeps running
// through the SDK path regardless of what new rows mint.
func (s *runQueueStore) EnqueueRun(ctx context.Context, orgID string, run domain.Conversation) error {
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	// team_id is derived from the task in-SQL (multi-team). The dispatcher mints
	// work items on the admin pool, which bypasses RLS, so both manual and
	// event rows insert here; the schema CHECK pairing trigger_type with
	// creator_user_id nullability is satisfied by the branch below.
	if triggerType == "event" {
		_, err := s.conn.ExecContext(ctx, `
			INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
			                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
			                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
			                  queued_at)
			VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, 'event', $7,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $8, $9, $10, NULLIF($11, ''), now())
		`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), run.Model, run.WorktreePath,
			nullIfEmpty(run.TriggerID), nullIfEmpty(run.ActorAgentID),
			nullIfEmpty(run.BlueprintRunID), stepIdx, run.PreferredExecutorID)
		if err != nil {
			return err
		}
		s.notifyWake(ctx, orgID)
		return nil
	}
	// Manual: the local sentinel user has no FK target in multi-mode; filter it
	// so the COALESCE walks to the org owner. There is no tf.current_user_id()
	// on the admin pool, so the creator must arrive on the row or fall back to
	// the org owner (the schema CHECK requires a non-NULL creator for manual).
	creatorBind := run.CreatorUserID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
		                  queued_at)
		VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, 'manual', $7,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($8, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $9, $10, $11, NULLIF($12, ''), now())
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), creatorBind, nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx, run.PreferredExecutorID)
	if err != nil {
		return err
	}
	s.notifyWake(ctx, orgID)
	return nil
}

// notifyWake fires the tf_wake doorbell after a row lands
// claimable, so an idle executor claims within milliseconds instead of
// waiting out its scan interval. Best-effort by contract (wakebus's "never
// the only path" rule) — a notify failure is swallowed, never surfaced to
// the caller, since the write it announces already committed successfully
// and the scan-interval backstop covers a dropped doorbell.
func (s *runQueueStore) notifyWake(ctx context.Context, orgID string) {
	_ = wakebus.Publish(ctx, s.conn, wakebus.KindRun, orgID)
}

// --- The needs-driving predicate ---------------------------------------
//
// Stored conversation status is outcome-or-nothing: 'open' (a deliberate
// park), a terminal, or NULL. NULL is not "unknown" — it is the mid-flight
// state: either an active claim is driving the conversation right now, or
// its last claim released without an outcome and nobody has picked it up
// yet. "Queued" and "running" are therefore never stored; they are read off
// the claim table and these predicates.
//
// Every fragment below is written against the conversation alias `r`.

// activeClaimExistsSQL is the derived "running": an unreleased claim is the
// engagement driving this conversation. Served by idx_claims_one_active
// (the partial unique index that is also the sole mutual-exclusion
// primitive for the claim).
const activeClaimExistsSQL = `EXISTS (
		SELECT 1 FROM claims cl_a
		WHERE cl_a.conversation_id = r.id AND cl_a.released_at IS NULL)`

// undeliveredInputExistsSQL matches drivable input: a plain user message
// still awaiting delivery. Injections (subtype 'injection:…', including a
// compaction request — which is inserted delivered anyway) ride whatever
// engagement runs next and never wake one on their own; a withdrawn row
// (undelivered + window_state 'inactive') never happened. No assumption is
// made about `seq` — a compaction commit re-seqs undelivered rows to
// fractional values and nothing here reads the column. Served by
// idx_messages_undelivered.
const undeliveredInputExistsSQL = `EXISTS (
		SELECT 1 FROM messages m_i
		WHERE m_i.conversation_id = r.id AND m_i.delivered = false
		  AND m_i.role = 'user' AND m_i.subtype = '' AND m_i.window_state = 'active')`

// needsDrivingSQL is the eligibility predicate, identical for every surface:
// nobody is driving it, it has not been retired, and it is either mid-flight
// (fresh mint, or a claim that released without writing an outcome) or
// parked and woken by new input. A terminal conversation is never eligible,
// whatever rows it holds — resuming one takes the explicit un-terminal write
// (MarkQueuedForResume).
//
// The NULL arm is what makes both reap cases work with no write beyond the
// claim release: a claim reaped mid-setup still has its undelivered prompt
// row (either arm matches), and a claim reaped mid-run has all its input
// delivered and matches here.
const needsDrivingSQL = `r.archived_at IS NULL
	  AND NOT ` + activeClaimExistsSQL + `
	  AND (r.status IS NULL OR (r.status = 'open' AND ` + undeliveredInputExistsSQL + `))`

// curatorNeedsTurnSQL is the first half of the curator type-conditional
// gate: a curator conversation has no autonomous work — its only unit of
// work is a user turn — so "mid-flight with nothing queued" is a finished
// transcript waiting for its next message, not work to pick up. It is also
// what keeps curator crash recovery retire-only: a turn reaped mid-run has
// its message delivered, so it is not re-driven.
const curatorNeedsTurnSQL = `(r.type <> 'curator' OR ` + undeliveredInputExistsSQL + `)`

// curatorHomedHereSQL is the second half: homing owns which executor runs a
// project's turns. A project with no home row is claimable by anyone, which
// is the local/role=all shape (one process, no homing). $1 is the claiming
// executor id.
const curatorHomedHereSQL = `(r.type <> 'curator' OR
		NOT EXISTS (SELECT 1 FROM curator_homes h
		            WHERE h.org_id = r.org_id::text AND h.project_id = r.project_id::text)
		OR EXISTS (SELECT 1 FROM curator_homes h
		           WHERE h.org_id = r.org_id::text AND h.project_id = r.project_id::text
		             AND h.home_instance_id = $1))`

// eligibleForDrivingSQL is the surface-agnostic "waiting to be driven" —
// the needs-driving predicate plus the curator input gate, without the
// placement/home/blueprint gates that decide WHICH executor may take it.
// This is the queue-depth answer the fleet counters and the display
// projection's derived `queued` rung read.
const eligibleForDrivingSQL = needsDrivingSQL + ` AND ` + curatorNeedsTurnSQL

// blueprintDrivableSQL is the delegation arm's gate, applied as a LEFT JOIN
// rather than an inner one: a queued step of a cancel-requested or already
// terminal blueprint is deliberately never claimed (the sequence-level
// cancel is honored here), while a conversation with no blueprint parent —
// curator today, interactive tomorrow — must not be filtered out by the
// join at all.
const blueprintDrivableSQL = `(r.blueprint_run_id IS NULL
	    OR (br.cancel_requested = false AND br.status = 'running'))`

// curatorTurnMessageSQL is the queued turn a curator claim is minted to
// drive — the conversation's oldest undelivered plain user row, stamped as
// the claim's mint intent so a pickup that fails before attaching any
// message stays attributable to its exact turn. NULL for every other type.
const curatorTurnMessageSQL = `(SELECT MIN(m_t.id) FROM messages m_t
		WHERE m_t.conversation_id = r.id AND m_t.delivered = false
		  AND m_t.role = 'user' AND m_t.subtype = '' AND m_t.window_state = 'active')`

// runQueueClaimSelect is the candidate CTE's projection — everything the
// dispatcher needs to branch on and drive the claimed conversation,
// including the type and (curator) the queued turn the claim is minted to.
const runQueueClaimSelect = `r.id, r.org_id,
	COALESCE(r.type, '')                  AS type,
	COALESCE(r.task_id::text, '')         AS task_id,
	COALESCE(r.prompt_id, '')             AS prompt_id,
	COALESCE(r.model, '')                 AS model,
	COALESCE(r.runtime, '')               AS runtime,
	COALESCE(r.worktree_path, '')         AS worktree_path,
	COALESCE(r.sdk_session_id, '')        AS sdk_session_id,
	r.trigger_type,
	COALESCE(r.trigger_id::text, '')      AS trigger_id,
	COALESCE(r.creator_user_id::text, '') AS creator_user_id,
	COALESCE(r.team_id::text, '')         AS team_id,
	COALESCE(r.project_id::text, '')      AS project_id,
	COALESCE(r.blueprint_run_id::text, '') AS blueprint_run_id,
	r.blueprint_step_index,
	` + curatorTurnMessageSQL + ` AS turn_message_id`

// runQueueClaimReturning is the outer-SELECT projection of ClaimNextRun. The
// claim identity (executor/claim id/claimed_at/attempts) rides the same
// statement: the id + claimed_at come from the freshly inserted claims row,
// attempts as the prior-claim count + 1 (data-modifying CTEs share one
// snapshot, so the count can't see the row the sibling CTE inserts). The
// claim id is returned because the executor needs to name this engagement
// later — at teardown, once it has been released and can no longer be found
// by looking for the conversation's active claim.
const runQueueClaimReturning = `candidate.id::text, candidate.org_id::text, candidate.type, candidate.task_id, candidate.prompt_id,
	candidate.model, candidate.runtime, candidate.worktree_path, candidate.sdk_session_id,
	candidate.trigger_type, candidate.trigger_id, candidate.creator_user_id,
	candidate.team_id, candidate.project_id,
	candidate.blueprint_run_id, candidate.blueprint_step_index, candidate.turn_message_id,
	minted.id::text AS claim_id, minted.claimed_at,
	(SELECT COUNT(*) + 1 FROM claims c2 WHERE c2.conversation_id = candidate.id)::int AS attempts`

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, placement db.ClaimPlacement) (*domain.Conversation, error) {
	// One scan, every surface: the needs-driving predicate is type-agnostic
	// and the two type-conditional gates (blueprint for delegation, queued
	// turn + home for curator) ride alongside it, so a single claim serves
	// the dispatcher and the curator both. An empty queue matches no row and
	// the scan reports ErrNoRows -> (nil, nil).
	//
	// Mutual exclusion is idx_claims_one_active, the partial unique index on
	// unreleased claims: nothing on the conversation row changes at claim
	// time any more, so FOR UPDATE SKIP LOCKED is a contention optimizer
	// (concurrent claimers pick different rows) rather than the guarantee.
	// The guarantee is the index, and claimRetryAttempts is how a loser
	// finds out — see the unique-violation retry below.
	//
	// The claims row is minted in the same statement (a data-modifying CTE)
	// so ownership is never ambiguous between claim and the process actually
	// going live — see ResetProcessingRuns. An empty executorID (the un-wired
	// test-spawner path) stores the '' sentinel; claims.executor_id is NOT
	// NULL by schema.
	//
	// The one conversation write left on this path is the un-park: a claim
	// taken on the `open` arm of the predicate ends the park by definition,
	// so the row goes back to mid-flight (NULL). That keeps "parked" and
	// "an engagement is driving this" disjoint at every instant, which is
	// what every recovery guard downstream reads — a claimed conversation is
	// always NULL, a NULL conversation with no claim is always claimable.
	//
	// Per-org fairness + cap are ALWAYS applied here, independent of
	// placement: the org_active CTE counts each org's active (slot-occupying)
	// runs once per statement, the cap filter hides a queued run whose org is
	// at or above its max_concurrent_runs, and the fairness key orders
	// claimable rows fewest-active-org first. Both degrade to a no-op at N=1 /
	// single org — the fairness key is constant across all candidates (so the
	// order collapses to started_at, id — the old global-oldest) and the
	// default NULL cap never filters — so existing single-org callers see the
	// same run claimed. The active count reads committed state, so under a
	// burst of concurrent claimers an org can momentarily exceed its cap by the
	// number of in-flight claims (a soft ceiling); sequential claims are exact.
	//
	// candidatePredicate + the tier prefix on the order are the only things
	// placement changes. Placement composes with fairness by
	// prefixing the tier term to the ORDER BY — fairness then orders WITHIN
	// each tier. Disabled: no tier term, no candidate predicate — the claim is
	// the globally-oldest claimable run modulo fairness/cap. Enabled: the
	// two-tier claim (see the SQL below).
	candidatePredicate := ""
	tierPrefix := "" // placement tier ordering, prepended to the fairness key
	args := []any{executorID, bootEpoch}
	if placement.Enabled {
		// $3 = aging seconds, $4 = liveness seconds. A run is claimable by me
		// when it is mine (tier 1), unowned, aged past the tier-2 window, or
		// its stamped preferred is not a live claimant — dead (heartbeat
		// stale past liveness), draining, or dispatch-gated. Tier 1 sorts
		// first, so a fresh run with a live owner is exclusively that owner's
		// until it ages (warm cache), while a saturated/dead owner never
		// head-of-line-blocks its shard.
		candidatePredicate = `
			  AND (
			    (r.preferred_executor_id IS NOT NULL AND r.preferred_executor_id = $1)
			    OR r.preferred_executor_id IS NULL
			    OR r.started_at < now() - make_interval(secs => $3)
			    OR NOT EXISTS (
			        SELECT 1 FROM instances i
			        WHERE i.id = r.preferred_executor_id
			          AND i.last_heartbeat_at >= now() - make_interval(secs => $4)
			          AND i.draining = false
			          AND i.dispatch_gated IS NOT TRUE
			    )
			  )`
		tierPrefix = "(r.preferred_executor_id IS NOT NULL AND r.preferred_executor_id = $1) DESC, "
		args = append(args, placement.AgingInterval.Seconds(), placement.Liveness.Seconds())
	}
	// Fairness orders WITHIN each placement tier: fewest-active-org first, then
	// oldest. COALESCE covers an org with zero active runs (no org_active row).
	orderBy := tierPrefix + "COALESCE(oa.active, 0), r.started_at, r.id"
	// org_active counts engagements, not stored statuses: an unreleased claim
	// IS an occupied slot, on either surface. Read straight off claims (whose
	// org_id is the conversation's) so the count is an index-only scan of
	// idx_claims_one_active rather than a pass over conversation history.
	query := `
		WITH org_active AS (
			SELECT org_id, count(*)::int AS active
			FROM claims
			WHERE released_at IS NULL
			GROUP BY org_id
		),
		candidate AS (
			SELECT ` + runQueueClaimSelect + `
			FROM conversations r
			LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			LEFT JOIN org_active oa ON oa.org_id = r.org_id
			LEFT JOIN org_settings os ON os.org_id = r.org_id
			WHERE ` + eligibleForDrivingSQL + `
			  AND ` + blueprintDrivableSQL + `
			  AND ` + curatorHomedHereSQL + `
			  AND (
			    os.max_concurrent_runs IS NULL
			    OR os.max_concurrent_runs <= 0
			    OR COALESCE(oa.active, 0) < os.max_concurrent_runs
			  )` + candidatePredicate + `
			ORDER BY ` + orderBy + `
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		),
		unparked AS (
			UPDATE conversations SET status = NULL, parked_at = NULL
			FROM candidate
			WHERE conversations.id = candidate.id AND conversations.status IS NOT NULL
			RETURNING conversations.id
		),
		minted AS (
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at, message_id)
			SELECT candidate.org_id, candidate.id, $1, $2::bigint, now(), candidate.turn_message_id FROM candidate
			RETURNING claims.id, claims.conversation_id, claims.claimed_at
		)
		SELECT ` + runQueueClaimReturning + `
		FROM candidate JOIN minted ON minted.conversation_id = candidate.id
	`
	// A concurrent claimer that read its snapshot before our claim committed
	// can pick the same conversation and lose on idx_claims_one_active. That
	// is the index doing its job, not an error worth surfacing: re-scan and
	// take the next eligible row instead.
	for attempt := 0; ; attempt++ {
		run, err := scanPgClaimedRun(s.conn.QueryRowContext(ctx, query, args...))
		if isActiveClaimConflict(err) && attempt < claimRetryAttempts {
			continue
		}
		if run != nil {
			run.ExecutorID = executorID
		}
		if isActiveClaimConflict(err) {
			return nil, nil
		}
		return run, wrapAdminPoolPermErr(err, "run_queue.ClaimNextRun")
	}
}

// claimRetryAttempts bounds the re-scan after a lost race on
// idx_claims_one_active. Losing twice in a row means real contention, and
// the caller's next scan tick is a better place to try again than a spin.
const claimRetryAttempts = 2

// isActiveClaimConflict reports whether err is the unique violation on the
// one-active-claim index — the "somebody else claimed this conversation
// between my snapshot and my insert" signal.
func isActiveClaimConflict(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == "idx_claims_one_active"
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	// Releasing the claim IS the requeue: the conversation stays mid-flight
	// (status NULL), so the moment its last claim releases it matches the
	// needs-driving predicate again. Guarded on NULL status so a stale
	// requeue can't resurrect a terminal or parked row. The claim releases as
	// 'requeued' (it already counted this try — Attempts is the claim count),
	// so any owner-keyed reader sees an unowned, claimable row.
	// preferred_executor_id is cleared for the same reason: a requeue's stamp
	// likely points at the executor that just failed the run; NULL means
	// "unowned, claimable by anyone now" — a live executor re-warms it with
	// no aging delay, the correct placement-is-advisory answer on a recovery
	// path (affinity is re-earned on the next enqueue, never carried stale).
	_, err := s.conn.ExecContext(ctx, `
		WITH req AS (
			UPDATE conversations AS r SET result_summary = $3, preferred_executor_id = NULL
			WHERE r.org_id = $1 AND r.id = $2 AND r.status IS NULL
			  AND `+activeClaimExistsSQL+`
			RETURNING id
		)
		UPDATE claims SET released_at = now(), outcome = 'requeued'
		FROM req
		WHERE claims.conversation_id = req.id AND claims.released_at IS NULL
	`, orgID, runID, lastErr)
	return err
}

func (s *runQueueStore) MarkAwaitingCredentials(ctx context.Context, orgID, runID, credPubKey string) (bool, error) {
	// The sidecar pubkey lands on the ACTIVE claim in the same statement as
	// the phase park, so the provisioner never sees a parked run without
	// the key it needs; the conversation row is untouched (it stays
	// 'running'). Guarded on phase IS NULL — the same protection window
	// the former status='running' guard gave: a duplicate can't re-park or
	// overwrite the pubkey while the claim is parked or mid-setup.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE claims SET phase = 'awaiting_credentials', cred_pubkey = NULLIF($1, '')
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL AND phase IS NULL
	`, credPubKey, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		_ = ctlbus.Publish(ctx, s.conn, ctlbus.Message{Kind: "cred_request", OrgID: orgID, RunID: runID})
	}
	return n > 0, nil
}

// awaitingCredentialsCols is the shared claim-identity projection: the
// conversation joined to its active (unreleased) claim, with the claim's
// executor/boot/pubkey and claimed_at falling back to started_at for a
// never-claimed row.
const awaitingCredentialsCols = `r.id::text, r.org_id::text, COALESCE(r.type, ''), COALESCE(r.team_id::text, ''), COALESCE(r.task_id::text, ''),
	COALESCE(cl.executor_id, ''), COALESCE(cl.boot_epoch, 0), COALESCE(cl.claimed_at, r.started_at), COALESCE(cl.cred_pubkey, '')`

func (s *runQueueStore) GetClaim(ctx context.Context, orgID, runID string) (db.AwaitingCredentialsRun, bool, error) {
	var r db.AwaitingCredentialsRun
	err := s.conn.QueryRowContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		LEFT JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, runID).Scan(&r.RunID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AwaitingCredentialsRun{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsRun{}, false, err
	}
	return r, true, nil
}

// RequeueAwaitingCredentials releases a run whose active claim is parked in
// phase='awaiting_credentials' — the executor-side timeout path: the brain
// never responded to this run's cred_request within the awaiting-credentials
// deadline, so the claim is released and the (still mid-flight) conversation
// is claimable again by the next dispatcher, this instance or a sibling.
// Guarded on that parked claim so a stale/duplicate timeout can't act on a
// row that already moved on (bundle arrived just after the deadline check,
// or the run was reaped).
func (s *runQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (bool, error) {
	var flipped int
	err := s.conn.QueryRowContext(ctx, `
		WITH req AS (
			UPDATE conversations SET preferred_executor_id = NULL
			WHERE org_id = $1 AND id = $2
			  AND EXISTS (
			      SELECT 1 FROM claims cl
			      WHERE cl.conversation_id = conversations.id
			        AND cl.released_at IS NULL AND cl.phase = 'awaiting_credentials'
			  )
			RETURNING id
		),
		rel AS (
			UPDATE claims SET released_at = now(), outcome = 'requeued'
			FROM req
			WHERE claims.conversation_id = req.id AND claims.released_at IS NULL
			RETURNING claims.id
		)
		SELECT count(*) FROM req
	`, orgID, runID).Scan(&flipped)
	if err != nil {
		return false, err
	}
	return flipped > 0, nil
}

func (s *runQueueStore) ListAwaitingCredentials(ctx context.Context) ([]db.AwaitingCredentialsRun, error) {
	// The parked set is keyed off the active claim's phase (served by the
	// idx_claims_active_phase partial index); an inner join is right here —
	// a parked claim by definition exists. No type filter: parking is a
	// property of the engagement, not the surface, so this one scan is the
	// whole provisioner's input and the caller routes on the type it
	// returns.
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
	// The active-claim join IS the "actively running" filter (a claim is the
	// engagement; there is no stored status to check); the bundle age check
	// joins claim_credentials on the claim id. A claim parked awaiting
	// credentials is excluded — its bundle is being minted fresh, not
	// refreshed.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		JOIN claim_credentials cc ON cc.claim_id = cl.id
		WHERE cl.phase IS DISTINCT FROM 'awaiting_credentials'
		  AND cc.created_at < $1
	`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAwaitingCredentialsRuns(rows)
}

func scanAwaitingCredentialsRuns(rows *sql.Rows) ([]db.AwaitingCredentialsRun, error) {
	var out []db.AwaitingCredentialsRun
	for rows.Next() {
		var r db.AwaitingCredentialsRun
		if err := rows.Scan(&r.RunID, &r.OrgID, &r.ConversationType, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *runQueueStore) ResetProcessingRuns(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Ownership-scoped through claims: only ACTIVE claims this instance
	// itself holds (executor_id = $1) from a strictly earlier boot
	// (boot_epoch < $2) on mid-flight conversations (status NULL — nothing
	// wrote an outcome, so the engagement was cut off) are released
	// ('reaped'). The release IS the requeue: the conversation was already
	// mid-flight, so the moment it has no claim it matches the needs-driving
	// predicate again. A live sibling's claim carries a different
	// executor_id and is never touched — that's what makes a booting process
	// safe alongside a still-live one. Parked (`open`) and terminal
	// conversations are not mid-flight and stay put.
	var count int
	err := s.conn.QueryRowContext(ctx, `
		WITH victims AS (
			SELECT cl.id AS claim_id, cl.conversation_id
			FROM claims cl
			JOIN conversations r ON r.id = cl.conversation_id
			WHERE cl.released_at IS NULL
			  AND cl.executor_id = $1
			  AND cl.boot_epoch < $2
			  AND r.status IS NULL
			  AND r.blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		),
		clr AS (
			UPDATE conversations SET preferred_executor_id = NULL
			FROM victims WHERE conversations.id = victims.conversation_id
			RETURNING conversations.id
		),
		rel AS (
			UPDATE claims SET released_at = now(), outcome = 'reaped'
			FROM victims WHERE claims.id = victims.claim_id
			RETURNING claims.id
		)
		SELECT count(*) FROM rel
	`, executorID, bootEpoch).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *runQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// Both counts are derived: active is the org's unreleased claims (an
	// engagement IS the occupied slot), queued is the org's conversations
	// matching the needs-driving predicate. Two index-backed scans unioned
	// into one grouping, then a PK lookup on org_settings for the cap. An
	// operator-cadence read, not the claim hot path. Ordered most-pressure
	// first so the fleet view leads with the busiest tenants.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id::text, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id, SUM(active)::int AS active, SUM(queued)::int AS queued
			FROM (
				SELECT org_id, 1 AS active, 0 AS queued FROM claims WHERE released_at IS NULL
				UNION ALL
				SELECT r.org_id, 0, 1 FROM conversations r WHERE `+eligibleForDrivingSQL+`
			) parts
			GROUP BY org_id
		) counts
		LEFT JOIN org_settings os ON os.org_id = counts.org_id
		ORDER BY (counts.active + counts.queued) DESC, counts.org_id
	`)
	if err != nil {
		return nil, wrapAdminPoolPermErr(err, "run_queue.FleetQueueShares")
	}
	defer rows.Close()
	return scanOrgQueueShares(rows)
}

func (s *runQueueStore) ReconcileOrphanedRuns(ctx context.Context) (int, error) {
	// Boot self-heal — see RunQueueStore.ReconcileOrphanedRuns and the
	// SQLite mirror. Admin pool (BYPASSRLS): a cross-org system sweep with no
	// per-user identity, the same posture as ResetProcessingRuns. Any active
	// claim on a cancelled row releases as 'cancelled' in the same statement.
	var total int
	err := inTx(ctx, s.conn, func(q queryer) error {
		var cancelled int
		if err := q.QueryRowContext(ctx, `
			WITH cancelled AS (
				UPDATE conversations
				SET status = 'cancelled',
				    completed_at = COALESCE(completed_at, now()),
				    stop_reason = COALESCE(stop_reason, 'blueprint_terminal'),
				    result_summary = COALESCE(NULLIF(result_summary, ''), $1)
				WHERE (status IS NULL OR status NOT IN (`+runTerminalStatusesSQL+`))
				  AND blueprint_run_id IN (
				      SELECT id FROM blueprint_runs
				      WHERE status IN ('completed','aborted','failed','cancelled')
				  )
				RETURNING id
			),
			rel AS (
				UPDATE claims SET released_at = now(), outcome = 'cancelled'
				FROM cancelled
				WHERE claims.conversation_id = cancelled.id AND claims.released_at IS NULL
				RETURNING claims.id
			)
			SELECT count(*) FROM cancelled
		`, "Cancelled: owning blueprint run reached a terminal state").Scan(&cancelled); err != nil {
			return err
		}

		// Claim-desync janitor arm (after the blueprint-terminal cancel, so
		// a row it just cancelled reads terminal here and is never touched).
		released, err := healClaimDesyncs(ctx, q)
		if err != nil {
			return err
		}
		total = cancelled + released
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// healClaimDesyncs is the janitor for the one state the app-pool
// (non-System) terminal writes can still strand a delegation conversation
// in: a terminal conversation with a still-active claim, because the
// conversation flip and the claim release commit independently (tf_app holds
// no claims UPDATE grant, so one atomic tx is structurally unavailable) and
// the flip can land without the release. Release the claim, outcome mapped
// from the status the same way the terminal writes map it (task_unsolvable →
// failed). Idempotent, and safe to run concurrently with healthy terminal
// writes.
//
// The former second arm — an in-flight conversation whose claim released but
// whose status flip rolled back, requeued by writing 'queued' — no longer
// exists as a desync at all: a released claim on a mid-flight (status NULL)
// conversation IS the requeue, so the shape it healed is now the ordinary
// claimable state.
func healClaimDesyncs(ctx context.Context, q queryer) (released int, err error) {
	res, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(),
		    outcome = CASE c.status
		        WHEN 'completed' THEN 'completed'
		        WHEN 'cancelled' THEN 'cancelled'
		        ELSE 'failed'
		    END
		FROM conversations c
		WHERE claims.conversation_id = c.id AND claims.released_at IS NULL
		  AND c.status IN (`+runTerminalStatusesSQL+`)
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// scanOrgQueueShares reads FleetQueueShares rows, mapping a NULL or
// non-positive max_concurrent_runs to a nil cap (unlimited).
func scanOrgQueueShares(rows *sql.Rows) ([]db.OrgQueueShare, error) {
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

func (s *runQueueStore) CountQueuedSystem(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversations r WHERE `+eligibleForDrivingSQL).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// runTimingClaimLateral derives the timing projection's claim fields — the
// LATEST claim's executor + claimed_at (the engagement that most recently
// drove the conversation, released or not), whether one is still live, plus
// the SUM of the per-engagement duration telemetry.
const runTimingClaimLateral = `
	LEFT JOIN LATERAL (
		SELECT MAX(c2.claimed_at) AS claimed_at,
		       (ARRAY_AGG(c2.executor_id ORDER BY c2.claimed_at DESC))[1] AS executor_id,
		       COUNT(*) FILTER (WHERE c2.released_at IS NULL) > 0 AS has_active,
		       SUM(c2.duration_ms)::bigint AS duration_ms
		FROM claims c2
		WHERE c2.conversation_id = r.id
	) cl ON true`

// runTimingStatusSQL is the timing projection's status: the stored outcome
// when there is one, else the derived in-flight state. The percentile read
// buckets by failure kind, so a mid-flight row must still name itself
// rather than scan as NULL.
const runTimingStatusSQL = `COALESCE(r.status, CASE WHEN cl.has_active THEN 'running' ELSE 'queued' END)`

func (s *runQueueStore) RecentRunTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.RunTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT r.org_id::text, COALESCE(cl.executor_id, ''), `+runTimingStatusSQL+`, COALESCE(r.failure_kind, ''),
		       r.started_at, cl.claimed_at, r.completed_at, cl.duration_ms
		FROM conversations r
		`+runTimingClaimLateral+`
		WHERE r.type = 'delegation' AND r.started_at >= $1
		ORDER BY r.started_at DESC
		LIMIT $2
	`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRunTimings(rows)
}

func (s *runQueueStore) QueuedRunAgesSystem(ctx context.Context) ([]domain.QueuedRun, error) {
	return s.queuedRunAges(ctx, "")
}

func (s *runQueueStore) QueuedRunAgesForOrgSystem(ctx context.Context, orgID string) ([]domain.QueuedRun, error) {
	return s.queuedRunAges(ctx, orgID)
}

func (s *runQueueStore) queuedRunAges(ctx context.Context, orgID string) ([]domain.QueuedRun, error) {
	q := `SELECT r.org_id::text, r.started_at, COALESCE(r.preferred_executor_id, '')
	      FROM conversations r WHERE ` + eligibleForDrivingSQL
	args := []any{}
	if orgID != "" {
		q += ` AND r.org_id = $1`
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
	q := `SELECT r.org_id::text, COALESCE(cl.executor_id, ''), ` + runTimingStatusSQL + `, COALESCE(r.failure_kind, ''),
	             r.started_at, cl.claimed_at, r.completed_at, cl.duration_ms
	      FROM conversations r
	      ` + runTimingClaimLateral + `
	      WHERE r.type = 'delegation' AND r.org_id = $1 AND r.started_at >= $2`
	args := []any{orgID, since.UTC()}
	if !until.IsZero() {
		args = append(args, until.UTC())
		q += ` AND r.started_at < $3`
	}
	args = append(args, limit)
	q += ` ORDER BY r.started_at DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRunTimings(rows)
}

// executorClaimCols is the shared projection behind both operator claim reads,
// so the per-executor list and the single-claim lookup can never drift into
// disagreeing about the same row. LEFT JOIN on the conversation: the claim is
// the subject here, and a claim whose conversation is gone must still report
// its measured cost rather than vanishing from the box's occupancy.
const executorClaimCols = `
	SELECT c.id::text, c.org_id::text, c.conversation_id::text,
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
		WHERE c.executor_id = $1
		ORDER BY c.claimed_at DESC, c.id DESC
		LIMIT $2
	`, executorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutorClaims(rows)
}

func (s *runQueueStore) ClaimByIDSystem(ctx context.Context, claimID string) (*domain.ExecutorClaim, error) {
	// A non-uuid id is a caller typo, not a server fault: Postgres would
	// reject the bind with a 22P02 and the handler would 500 on what is
	// really a miss. Report it as "no such claim" and let the caller 404.
	if _, err := uuid.Parse(claimID); err != nil {
		return nil, nil
	}
	rows, err := s.conn.QueryContext(ctx, executorClaimCols+` WHERE c.id = $1`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanExecutorClaims(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

func scanExecutorClaims(rows *sql.Rows) ([]domain.ExecutorClaim, error) {
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

func scanRunTimings(rows *sql.Rows) ([]domain.RunTiming, error) {
	var out []domain.RunTiming
	for rows.Next() {
		var t domain.RunTiming
		var claimedAt, completedAt sql.NullTime
		var durationMS sql.NullInt64
		if err := rows.Scan(
			&t.OrgID, &t.ExecutorID, &t.Status, &t.FailureKind,
			&t.StartedAt, &claimedAt, &completedAt, &durationMS,
		); err != nil {
			return nil, err
		}
		if claimedAt.Valid {
			v := claimedAt.Time
			t.ClaimedAt = &v
		}
		if completedAt.Valid {
			v := completedAt.Time
			t.CompletedAt = &v
		}
		t.DurationMS = intPtrFromNull(durationMS)
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanPgClaimedRun scans ClaimNextRun's outer projection into
// *domain.Conversation, including the freshly minted claim's claimed_at and the
// attempts count. (nil, nil) on sql.ErrNoRows so callers treat "nothing
// claimable" as a non-error empty result. Status is deliberately left empty:
// a claimed conversation's stored status is NULL by construction, and the
// dispatcher branches on Type and Runtime, never on it.
func scanPgClaimedRun(row *sql.Row) (*domain.Conversation, error) {
	var (
		r         domain.Conversation
		stepIdx   sql.NullInt64
		turnMsgID sql.NullInt64
		claimedAt time.Time
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.Type, &r.TaskID, &r.PromptID, &r.Model, &r.Runtime,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.ProjectID, &r.BlueprintRunID, &stepIdx, &turnMsgID,
		&r.ClaimID, &claimedAt, &r.Attempts)
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
	r.ClaimedAt = &claimedAt
	return &r, nil
}
