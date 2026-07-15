package postgres

import (
	"context"
	"database/sql"
	"errors"
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
// The runs_* RLS policies are defense-in-depth (admin bypasses them) and org_id
// is bound where it is known.
type runQueueStore struct {
	conn *sql.DB
}

func newRunQueueStore(conn *sql.DB) db.RunQueueStore {
	return &runQueueStore{conn: conn}
}

var _ db.RunQueueStore = (*runQueueStore)(nil)

// runTerminalStatusesSQL is the canonical set of terminal runs.status values as
// a SQL IN-list body. The reconcile sweep and the orphaned-child cancel
// (blueprints.go) interpolate it instead of re-spelling the literal, so the
// "non-terminal child" predicate has one definition. Other queries that add
// dormant statuses (open/pending_approval) to this set keep their own list.
const runTerminalStatusesSQL = `'completed','failed','cancelled','task_unsolvable'`

// runQueueClaimCols is the column list ClaimNextRun returns. team_id is
// surfaced (TFAC-458) so the construction-path RunInfo built off a claimed run
// carries the owning team for the capture writers; it is NOT NULL on the row
// (EnqueueRun derives it from the parent task).
const runQueueClaimCols = `id::text, org_id::text, task_id::text, COALESCE(prompt_id, ''), status, COALESCE(model, ''),
	COALESCE(worktree_path, ''), COALESCE(session_id, ''), trigger_type, COALESCE(trigger_id::text, ''),
	COALESCE(creator_user_id::text, ''), team_id::text, COALESCE(blueprint_run_id::text, ''), blueprint_step_index, attempts`

func (s *runQueueStore) EnqueueRun(ctx context.Context, orgID string, run domain.AgentRun) error {
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
			INSERT INTO runs (id, org_id, task_id, prompt_id, status, model, worktree_path,
			                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
			                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'event', $8,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $9, $10, $11, NULLIF($12, ''))
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
		INSERT INTO runs (id, org_id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index, preferred_executor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($9, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $10, $11, $12, NULLIF($13, ''))
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), creatorBind, nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx, run.PreferredExecutorID)
	if err != nil {
		return err
	}
	s.notifyWake(ctx, orgID)
	return nil
}

// notifyWake fires the tf_wake doorbell (TFAC-586) after a row lands
// claimable, so an idle executor claims within milliseconds instead of
// waiting out its scan interval. Best-effort by contract (wakebus's "never
// the only path" rule) — a notify failure is swallowed, never surfaced to
// the caller, since the write it announces already committed successfully
// and the scan-interval backstop covers a dropped doorbell.
func (s *runQueueStore) notifyWake(ctx context.Context, orgID string) {
	_ = wakebus.Publish(ctx, s.conn, wakebus.KindRun, orgID)
}

// activeRunStatusesSQL is the set of runs.status values that occupy a live
// executor slot — claimed and executing, but not yet terminal and not
// hibernated (open/pending_approval park off the fleet). It is what the
// per-org fairness/cap claim counts as an org's "active" runs. Keep this
// status set in sync with idx_runs_active_by_org's partial predicate: Postgres
// serves the count from that partial index when the query's WHERE provably
// implies the index predicate (predicate implication — an equivalent IN-list
// matches regardless of spelling, not textual identity), and that implication
// is what keeps the count an index scan over the small live set rather than a
// seq scan of the whole runs history.
const activeRunStatusesSQL = `'running', 'awaiting_credentials'`

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64, placement db.ClaimPlacement) (*domain.AgentRun, error) {
	// FOR UPDATE SKIP LOCKED on the inner select so a multi-worker fleet never
	// claims the same queued run. Claimable = the owning blueprint_run is still
	// 'running' and not cancel-requested. An empty queue matches no row and the
	// scan reports ErrNoRows -> (nil, nil).
	//
	// executor_id + boot_epoch are stamped in this same statement (TFAC-578)
	// so the row's ownership is never ambiguous between claim and the process
	// actually going live — see ResetProcessingRuns.
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
	// placement changes (TFAC-587). Placement composes with fairness by
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
			FROM runs
			WHERE status IN (`+activeRunStatusesSQL+`)
			GROUP BY org_id
		)
		UPDATE runs
		SET status = 'running', claimed_at = now(), attempts = attempts + 1,
		    executor_id = NULLIF($1, ''),
		    boot_epoch  = CASE WHEN $1 = '' THEN NULL ELSE $2::bigint END
		WHERE id = (
			SELECT r.id FROM runs r
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
		RETURNING `+runQueueClaimCols, args...)
	run, err := scanPgClaimedRun(row)
	return run, wrapAdminPoolPermErr(err, "run_queue.ClaimNextRun")
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	// attempts left as-is (the claim counted this try); claimed_at cleared.
	// Guarded on 'running' so a stale requeue can't resurrect a terminal row.
	// The ownership stamp is cleared too: a queued row has no owner, and a
	// stale pair here would misdirect any owner-keyed reader (the reaper,
	// the fleet view) toward an instance that stopped touching the row.
	// preferred_executor_id is cleared for the same reason (TFAC-587): a
	// requeue's stamp likely points at the executor that just failed the run;
	// NULL means "unowned, claimable by anyone now" — a live executor re-warms
	// it with no aging delay, which is the correct placement-is-advisory
	// answer on a recovery path (affinity is re-earned on the next enqueue,
	// never carried stale).
	_, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL, result_summary = $3,
			executor_id = NULL, boot_epoch = NULL, cred_pubkey = NULL, preferred_executor_id = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'running'
	`, orgID, runID, lastErr)
	return err
}

func (s *runQueueStore) MarkAwaitingCredentials(ctx context.Context, orgID, runID, credPubKey string) (bool, error) {
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'awaiting_credentials', cred_pubkey = NULLIF($3, '')
		WHERE org_id = $1 AND id = $2 AND status = 'running'
	`, orgID, runID, credPubKey)
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

func (s *runQueueStore) GetClaim(ctx context.Context, orgID, runID string) (db.AwaitingCredentialsRun, bool, error) {
	var r db.AwaitingCredentialsRun
	err := s.conn.QueryRowContext(ctx, `
		SELECT id::text, org_id::text, team_id::text, task_id::text, COALESCE(executor_id, ''),
		       COALESCE(boot_epoch, 0), COALESCE(claimed_at, started_at), COALESCE(cred_pubkey, '')
		FROM runs WHERE org_id = $1 AND id = $2
	`, orgID, runID).Scan(&r.RunID, &r.OrgID, &r.TeamID, &r.TaskID, &r.ExecutorID, &r.BootEpoch, &r.ClaimedAt, &r.CredPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AwaitingCredentialsRun{}, false, nil
	}
	if err != nil {
		return db.AwaitingCredentialsRun{}, false, err
	}
	return r, true, nil
}

// RequeueAwaitingCredentials releases a run parked in status=
// 'awaiting_credentials' back to 'queued', clearing ownership — the
// executor-side timeout path (TFAC-614): the brain never responded to this
// run's cred_request within the awaiting-credentials deadline, so the claim
// is released for the next dispatcher (this instance or a sibling) to
// re-claim and re-request. Guarded on 'awaiting_credentials' so a stale/
// duplicate timeout can't resurrect a row that already moved on (bundle
// arrived just after the deadline check, or the run was reaped).
func (s *runQueueStore) RequeueAwaitingCredentials(ctx context.Context, orgID, runID string) (bool, error) {
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL, cred_pubkey = NULL, preferred_executor_id = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'awaiting_credentials'
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *runQueueStore) ListAwaitingCredentials(ctx context.Context) ([]db.AwaitingCredentialsRun, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id::text, org_id::text, team_id::text, task_id::text, COALESCE(executor_id, ''),
		       COALESCE(boot_epoch, 0), COALESCE(claimed_at, started_at), COALESCE(cred_pubkey, '')
		FROM runs WHERE status = 'awaiting_credentials'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAwaitingCredentialsRuns(rows)
}

func (s *runQueueStore) ListActiveNeedingCredentialRefresh(ctx context.Context, olderThan time.Time) ([]db.AwaitingCredentialsRun, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT r.id::text, r.org_id::text, r.team_id::text, r.task_id::text, COALESCE(r.executor_id, ''),
		       COALESCE(r.boot_epoch, 0), COALESCE(r.claimed_at, r.started_at), COALESCE(r.cred_pubkey, '')
		FROM runs r
		JOIN run_credentials rc ON rc.run_id = r.id
		WHERE r.status NOT IN ('queued', 'awaiting_credentials', `+runTerminalStatusesSQL+`, 'open')
		  AND r.executor_id IS NOT NULL
		  AND rc.created_at < $1
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
	// Ownership-scoped (TFAC-578): only rows this instance itself claimed
	// (executor_id = $1) during a strictly earlier boot (boot_epoch < $2) are
	// reset. A live sibling's claimed/running row carries a different
	// executor_id and is never touched — that's what makes a booting process
	// safe alongside a still-live one. The `boot_epoch IS NULL` disjunct is
	// a narrow defensive catch: it matches only rows stamped with THIS
	// instance's persistent id but no epoch (a state only pre-epoch
	// from-source builds could produce) — it does NOT cover pre-registry
	// rows, which carry a per-boot random uuid or NULL executor_id that no
	// persistent id ever matches. On Postgres that boundary is moot (the
	// multi-mode posture is fresh-installs-only, migrations.go
	// assertFreshOrCurrent); SQLite — which has released pre-registry
	// installs — normalizes it once via migration
	// 202607080003_pre_registry_orphan_normalization. The reset clears the
	// stamp: a queued row has no owner.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL, cred_pubkey = NULL, preferred_executor_id = NULL
		WHERE status NOT IN (
			'queued',
			'completed','failed','cancelled','task_unsolvable',
			'open','pending_approval'
		)
		AND blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
		AND executor_id = $1
		AND (boot_epoch IS NULL OR boot_epoch < $2)
	`, executorID, bootEpoch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *runQueueStore) FleetQueueShares(ctx context.Context) ([]db.OrgQueueShare, error) {
	// One pass over the live (active + queued) run set, grouped by org, then a
	// PK lookup on org_settings for the cap. FILTER splits the two counts in a
	// single scan; the status filter keeps it off the terminal-run history. An
	// operator-cadence read, not the claim hot path — idx_runs_org_status
	// (org_id, status) serves it. Ordered most-pressure first so the fleet view
	// leads with the busiest tenants.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT counts.org_id::text, counts.active, counts.queued, os.max_concurrent_runs
		FROM (
			SELECT org_id,
			       count(*) FILTER (WHERE status IN (`+activeRunStatusesSQL+`))::int AS active,
			       count(*) FILTER (WHERE status = 'queued')::int                   AS queued
			FROM runs
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
	// per-user identity, the same posture as ResetProcessingRuns.
	//
	// Roll up the token cache here too (correlated SUM per row) — a 'running'
	// child stranded under a terminal blueprint may have streamed run_messages,
	// so this terminal write must reflect them rather than leave the columns at
	// 0. Same every-terminal-write invariant as the cancel paths; correlates on
	// the unique run_id, so no org scoping is needed on this cross-tenant sweep
	// (TFAC-473).
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, now()),
		    stop_reason = COALESCE(stop_reason, 'blueprint_terminal'),
		    result_summary = COALESCE(NULLIF(result_summary, ''), $1),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE run_id = runs.id),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE run_id = runs.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE run_id = runs.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE run_id = runs.id)
		WHERE status NOT IN (`+runTerminalStatusesSQL+`)
		  AND blueprint_run_id IN (
		      SELECT id FROM blueprint_runs
		      WHERE status IN ('completed','aborted','failed','cancelled')
		  )
	`, "Cancelled: owning blueprint run reached a terminal state")
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
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

func scanPgClaimedRun(row *sql.Row) (*domain.AgentRun, error) {
	var (
		r       domain.AgentRun
		stepIdx sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.OrgID, &r.TaskID, &r.PromptID, &r.Status, &r.Model,
		&r.WorktreePath, &r.SessionID, &r.TriggerType, &r.TriggerID,
		&r.CreatorUserID, &r.TeamID, &r.BlueprintRunID, &stepIdx, &r.Attempts)
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
