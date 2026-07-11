package postgres

import (
	"context"
	"database/sql"

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
			                  actor_agent_id, blueprint_run_id, blueprint_step_index)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'event', $8,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $9, $10, $11)
		`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
			nullIfEmpty(run.TriggerID), nullIfEmpty(run.ActorAgentID),
			nullIfEmpty(run.BlueprintRunID), stepIdx)
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
		                  actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($9, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $10, $11, $12)
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), creatorBind, nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx)
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

func (s *runQueueStore) ClaimNextRun(ctx context.Context, executorID string, bootEpoch int64) (*domain.AgentRun, error) {
	// FOR UPDATE SKIP LOCKED on the inner select so a future multi-worker
	// dispatcher never claims the same queued run. Claimable = the owning
	// blueprint_run is still 'running' and not cancel-requested. An empty queue
	// matches no row and the scan reports ErrNoRows -> (nil, nil).
	//
	// executor_id + boot_epoch are stamped in this same statement (TFAC-578)
	// so the row's ownership is never ambiguous between claim and the process
	// actually going live — see ResetProcessingRuns.
	row := s.conn.QueryRowContext(ctx, `
		UPDATE runs
		SET status = 'running', claimed_at = now(), attempts = attempts + 1,
		    executor_id = NULLIF($1, ''),
		    boot_epoch  = CASE WHEN $1 = '' THEN NULL ELSE $2::bigint END
		WHERE id = (
			SELECT r.id FROM runs r
			JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			WHERE r.status = 'queued'
			  AND br.cancel_requested = false
			  AND br.status = 'running'
			ORDER BY r.started_at, r.id
			FOR UPDATE OF r SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+runQueueClaimCols, executorID, bootEpoch)
	run, err := scanPgClaimedRun(row)
	return run, wrapAdminPoolPermErr(err, "run_queue.ClaimNextRun")
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	// attempts left as-is (the claim counted this try); claimed_at cleared.
	// Guarded on 'running' so a stale requeue can't resurrect a terminal row.
	// The ownership stamp is cleared too: a queued row has no owner, and a
	// stale pair here would misdirect any owner-keyed reader (the reaper,
	// the fleet view) toward an instance that stopped touching the row.
	_, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL, result_summary = $3,
			executor_id = NULL, boot_epoch = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'running'
	`, orgID, runID, lastErr)
	return err
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
			executor_id = NULL, boot_epoch = NULL
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
