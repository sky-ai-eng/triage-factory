package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

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

// EnqueueRun mints a queued delegation conversation. runtime is stamped
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
	status := run.Status
	if status == "" {
		status = "queued"
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
			INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, status, model, worktree_path,
			                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
			                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
			                  queued_at)
			VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, $7, 'event', $8,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $9, $10, $11, NULLIF($12, ''), now())
		`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
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
		INSERT INTO conversations (id, org_id, type, runtime, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id,
		                  queued_at)
		VALUES ($1, $2, 'delegation', 'native', $3, $4, $5, $6, $7, 'manual', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($9, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $10, $11, $12, NULLIF($13, ''), now())
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
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

// activeRunStatusesSQL is the stored conversation status that occupies a
// live executor slot — claimed (mid-setup or executing; setup progress is
// the active claim's phase, never a stored status), but not yet terminal
// and not hibernated (open/pending_approval park off the fleet). It is what
// the per-org fairness/cap claim counts as an org's "active" runs. The
// count is still served by the active-by-org partial index: a WHERE that
// provably implies the index predicate (a strict subset qualifies) keeps
// the count an index scan over the small live set rather than a seq scan of
// the whole history.
const activeRunStatusesSQL = `'running'`

// runQueueClaimReturning is the outer-SELECT projection of ClaimNextRun's
// claimed CTE. The claim identity (executor/claimed_at/attempts) rides the
// same statement: claimed_at from the freshly inserted claims row, attempts
// as the prior-claim count + 1 (data-modifying CTEs share one snapshot, so
// the count can't see the row the sibling CTE inserts).
const runQueueClaimReturning = `claimed.id::text, claimed.org_id::text, claimed.task_id::text, COALESCE(claimed.prompt_id, ''), claimed.status, COALESCE(claimed.model, ''), COALESCE(claimed.runtime, ''),
	COALESCE(claimed.worktree_path, ''), COALESCE(claimed.sdk_session_id, ''), claimed.trigger_type, COALESCE(claimed.trigger_id::text, ''),
	COALESCE(claimed.creator_user_id::text, ''), claimed.team_id::text, COALESCE(claimed.blueprint_run_id::text, ''), claimed.blueprint_step_index,
	minted.claimed_at,
	(SELECT COUNT(*) + 1 FROM claims c2 WHERE c2.conversation_id = claimed.id)::int AS attempts`

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, placement db.ClaimPlacement) (*domain.Conversation, error) {
	// FOR UPDATE SKIP LOCKED on the inner select so a multi-worker fleet never
	// claims the same queued run. Claimable = the owning blueprint_run is still
	// 'running' and not cancel-requested. An empty queue matches no row and the
	// scan reports ErrNoRows -> (nil, nil).
	//
	// The claims row is minted in the same statement (a data-modifying CTE)
	// so the row's ownership is never ambiguous between claim and the process
	// actually going live — see ResetProcessingRuns. An empty executorID (the
	// un-wired test-spawner path) stores the '' sentinel; claims.executor_id
	// is NOT NULL by schema.
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
	row := s.conn.QueryRowContext(ctx, `
		WITH org_active AS (
			SELECT org_id, count(*)::int AS active
			FROM conversations
			WHERE status IN (`+activeRunStatusesSQL+`)
			GROUP BY org_id
		),
		claimed AS (
			UPDATE conversations
			SET status = 'running'
			WHERE id = (
				SELECT r.id FROM conversations r
				JOIN blueprint_runs br ON br.id = r.blueprint_run_id
				LEFT JOIN org_active oa ON oa.org_id = r.org_id
				LEFT JOIN org_settings os ON os.org_id = r.org_id
				WHERE r.status = 'queued'
				  AND br.cancel_requested = false
				  AND br.status = 'running'
				  AND (
				    os.max_concurrent_runs IS NULL
				    OR os.max_concurrent_runs <= 0
				    OR COALESCE(oa.active, 0) < os.max_concurrent_runs
				  )`+candidatePredicate+`
				ORDER BY `+orderBy+`
				FOR UPDATE OF r SKIP LOCKED
				LIMIT 1
			)
			RETURNING *
		),
		minted AS (
			INSERT INTO claims (org_id, conversation_id, executor_id, boot_epoch, claimed_at)
			SELECT claimed.org_id, claimed.id, $1, $2::bigint, now() FROM claimed
			RETURNING claims.conversation_id, claims.claimed_at
		)
		SELECT `+runQueueClaimReturning+`
		FROM claimed JOIN minted ON minted.conversation_id = claimed.id
	`, args...)
	run, err := scanPgClaimedRun(row)
	if run != nil {
		run.ExecutorID = executorID
	}
	return run, wrapAdminPoolPermErr(err, "run_queue.ClaimNextRun")
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	// Guarded on 'running' — the one status a claimed run holds from claim
	// until terminal/park, whatever setup phase its claim is in — so a
	// stale requeue can't resurrect a terminal/parked/queued row. The
	// active claim releases as 'requeued' (the claim already counted this
	// try — Attempts is the claim count), so any owner-keyed reader (the
	// reaper, the fleet view) sees an unowned queued row. preferred_executor_id
	// is cleared for the same reason: a requeue's stamp likely points at the
	// executor that just failed the run; NULL means "unowned, claimable by anyone
	// now" — a live executor re-warms it with no aging delay, which is the correct
	// placement-is-advisory answer on a recovery path (affinity is re-earned on
	// the next enqueue, never carried stale).
	_, err := s.conn.ExecContext(ctx, `
		WITH req AS (
			UPDATE conversations SET status = 'queued', queued_at = now(), result_summary = $3,
				preferred_executor_id = NULL
			WHERE org_id = $1 AND id = $2 AND status = 'running'
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
const awaitingCredentialsCols = `r.id::text, r.org_id::text, COALESCE(r.team_id::text, ''), COALESCE(r.task_id::text, ''),
	COALESCE(cl.executor_id, ''), COALESCE(cl.boot_epoch, 0), COALESCE(cl.claimed_at, r.started_at), COALESCE(cl.cred_pubkey, '')`

func (s *runQueueStore) GetClaim(ctx context.Context, orgID, runID string) (db.AwaitingCredentialsRun, bool, error) {
	var r db.AwaitingCredentialsRun
	err := s.conn.QueryRowContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		LEFT JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, runID).Scan(&r.RunID, &r.OrgID, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AwaitingCredentialsRun{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsRun{}, false, err
	}
	return r, true, nil
}

// RequeueAwaitingCredentials releases a run whose active claim is parked in
// phase='awaiting_credentials' back to 'queued', releasing that claim — the
// executor-side timeout path: the brain never responded to this
// run's cred_request within the awaiting-credentials deadline, so the claim
// is released for the next dispatcher (this instance or a sibling) to
// re-claim and re-request. Guarded on that parked claim so a stale/
// duplicate timeout can't resurrect a row that already moved on (bundle
// arrived just after the deadline check, or the run was reaped).
func (s *runQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (bool, error) {
	var flipped int
	err := s.conn.QueryRowContext(ctx, `
		WITH req AS (
			UPDATE conversations SET status = 'queued', queued_at = now(),
				preferred_executor_id = NULL
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
	// The active-claim join replaces the former executor_id IS NOT NULL
	// filter (an active claim IS the ownership); the bundle age check joins
	// claim_credentials on the claim id. A claim parked awaiting
	// credentials is excluded — its bundle is being minted fresh, not
	// refreshed.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+awaitingCredentialsCols+`
		FROM conversations r
		JOIN claims cl ON cl.conversation_id = r.id AND cl.released_at IS NULL
		JOIN claim_credentials cc ON cc.claim_id = cl.id
		WHERE r.status = 'running'
		  AND cl.phase IS DISTINCT FROM 'awaiting_credentials'
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
		if err := rows.Scan(&r.RunID, &r.OrgID, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *runQueueStore) ResetProcessingRuns(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Ownership-scoped through claims: only ACTIVE claims this instance
	// itself holds (executor_id = $1) from a strictly earlier boot
	// (boot_epoch < $2) on 'running' conversations (claimed — mid-setup or
	// executing; setup progress is claim phase, never status) are released
	// ('reaped') and flipped back to 'queued' for re-claim. A
	// live sibling's claim carries a different executor_id and is never
	// touched — that's what makes a booting process safe alongside a
	// still-live one. Dormant conversations (open, pending_approval) are
	// not 'running' and so stay parked — they resume through their own
	// paths, not the queue.
	var count int
	err := s.conn.QueryRowContext(ctx, `
		WITH victims AS (
			SELECT cl.id AS claim_id, cl.conversation_id
			FROM claims cl
			JOIN conversations r ON r.id = cl.conversation_id
			WHERE cl.released_at IS NULL
			  AND cl.executor_id = $1
			  AND cl.boot_epoch < $2
			  AND r.status = 'running'
			  AND r.blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		),
		rel AS (
			UPDATE claims SET released_at = now(), outcome = 'reaped'
			FROM victims WHERE claims.id = victims.claim_id
			RETURNING claims.id
		),
		flip AS (
			UPDATE conversations SET status = 'queued', queued_at = now(),
				preferred_executor_id = NULL
			FROM victims WHERE conversations.id = victims.conversation_id
			RETURNING conversations.id
		)
		SELECT count(*) FROM flip
	`, executorID, bootEpoch).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *runQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// One pass over the live (active + queued) run set, grouped by org, then a
	// PK lookup on org_settings for the cap. FILTER splits the two counts in a
	// single scan; the status filter keeps it off the terminal-run history. An
	// operator-cadence read, not the claim hot path. Ordered most-pressure
	// first so the fleet view leads with the busiest tenants.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id::text, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id,
			       count(*) FILTER (WHERE status IN (`+activeRunStatusesSQL+`))::int AS active,
			       count(*) FILTER (WHERE status = 'queued')::int                   AS queued
			FROM conversations
			WHERE status IN (`+activeRunStatusesSQL+`, 'queued')
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
				WHERE status NOT IN (`+runTerminalStatusesSQL+`)
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

		// Claim-desync janitor arms (after the blueprint-terminal cancel, so
		// a row it just cancelled reads terminal here and is never requeued).
		released, requeued, err := healClaimDesyncs(ctx, q)
		if err != nil {
			return err
		}
		total = cancelled + released + requeued
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// healClaimDesyncs is the two-armed janitor for the states the app-pool
// (non-System) terminal writes can strand a delegation conversation in —
// their conversation flip and claim release commit independently (tf_app
// holds no claims UPDATE grant, so one atomic tx is structurally
// unavailable), and either commit can land without the other. Shared by the
// boot-time ReconcileOrphanedRuns and the leader reaper's periodic sweep;
// both arms are idempotent, and a normal in-between state (row still locked
// by the in-flight writer) resolves against the committed row under READ
// COMMITTED re-evaluation, so running this concurrently with healthy
// terminal writes is safe.
//
// Arm one: a terminal conversation with a still-active claim (the flip
// committed, the release never did) — release it, outcome mapped from the
// status the same way the terminal writes map it (task_unsolvable → failed).
//
// Arm two: an in-flight delegation conversation with no active claim (the
// release committed, the flip's tx rolled back) — back to 'queued' so the
// dispatcher re-claims it.
//
// Arm two cannot false-positive against a HEALTHY terminal write racing it:
// the app-pool tx stages its status UPDATE before the adjacent claim
// release, so this arm's UPDATE blocks on that row lock and Postgres
// re-evaluates the WHERE after the commit — a conversation that just went
// terminal no longer matches. Only a genuinely rolled-back flip (the crash
// window this arm exists for) still presents the claimless in-flight shape. Gated on a still-running blueprint parent,
// mirroring ResetProcessingRuns: a re-queued row is only claimable under a
// running parent, and rows under a terminal parent belong to the
// blueprint-terminal cancel arm. Curator conversations are excluded twice
// over (status NULL, type 'curator') — their claim recovery is the
// curator boot sweeps' and the curator reaper arm's job.
func healClaimDesyncs(ctx context.Context, q queryer) (released, requeued int, err error) {
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
		return 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	released = int(n)

	res, err = q.ExecContext(ctx, `
		UPDATE conversations SET status = 'queued', queued_at = now(),
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
		return 0, 0, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	requeued = int(n)
	return released, requeued, nil
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
	err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE status = 'queued'`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// runTimingClaimLateral derives the timing projection's claim fields — the
// LATEST claim's executor + claimed_at (the engagement that most recently
// drove the conversation, released or not) plus the SUM of the
// per-engagement duration telemetry.
const runTimingClaimLateral = `
	LEFT JOIN LATERAL (
		SELECT MAX(c2.claimed_at) AS claimed_at,
		       (ARRAY_AGG(c2.executor_id ORDER BY c2.claimed_at DESC))[1] AS executor_id,
		       SUM(c2.duration_ms)::bigint AS duration_ms
		FROM claims c2
		WHERE c2.conversation_id = r.id
	) cl ON true`

func (s *runQueueStore) RecentRunTimingsSystem(ctx context.Context, since time.Time, limit int) ([]domain.RunTiming, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT r.org_id::text, COALESCE(cl.executor_id, ''), r.status, COALESCE(r.failure_kind, ''),
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
	q := `SELECT org_id::text, started_at, COALESCE(preferred_executor_id, '')
	      FROM conversations WHERE status = 'queued'`
	args := []any{}
	if orgID != "" {
		q += ` AND org_id = $1`
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
	q := `SELECT r.org_id::text, COALESCE(cl.executor_id, ''), r.status, COALESCE(r.failure_kind, ''),
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
// claimable" as a non-error empty result.
func scanPgClaimedRun(row *sql.Row) (*domain.Conversation, error) {
	var (
		r         domain.Conversation
		stepIdx   sql.NullInt64
		claimedAt time.Time
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.TaskID, &r.PromptID, &r.Status, &r.Model, &r.Runtime,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.BlueprintRunID, &stepIdx, &claimedAt, &r.Attempts)
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
	r.ClaimedAt = &claimedAt
	return &r, nil
}
