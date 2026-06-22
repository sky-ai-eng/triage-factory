package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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

// runQueueClaimCols is the column list ClaimNextRun returns.
const runQueueClaimCols = `id::text, org_id::text, task_id::text, COALESCE(prompt_id, ''), status, COALESCE(model, ''),
	COALESCE(worktree_path, ''), COALESCE(session_id, ''), trigger_type, COALESCE(trigger_id::text, ''),
	COALESCE(creator_user_id::text, ''), COALESCE(blueprint_run_id::text, ''), blueprint_step_index, attempts`

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
			                  blueprint_run_id, blueprint_step_index)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'event', $8,
			        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
			        'team', NULL, $9, $10)
		`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
			nullIfEmpty(run.TriggerID), nullIfEmpty(run.BlueprintRunID), stepIdx)
		return err
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
		                  blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(NULLIF($9, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $2)),
		        $10, $11)
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), creatorBind, nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}

func (s *runQueueStore) ClaimNextRun(ctx context.Context) (*domain.AgentRun, error) {
	// FOR UPDATE SKIP LOCKED on the inner select so a future multi-worker
	// dispatcher never claims the same queued run. Claimable = the owning
	// blueprint_run is still 'running' and not cancel-requested. An empty queue
	// matches no row and the scan reports ErrNoRows -> (nil, nil).
	row := s.conn.QueryRowContext(ctx, `
		UPDATE runs
		SET status = 'running', claimed_at = now(), attempts = attempts + 1
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
		RETURNING `+runQueueClaimCols)
	return scanPgClaimedRun(row)
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	// attempts left as-is (the claim counted this try); claimed_at cleared.
	// Guarded on 'running' so a stale requeue can't resurrect a terminal row.
	_, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL, result_summary = $3
		WHERE org_id = $1 AND id = $2 AND status = 'running'
	`, orgID, runID, lastErr)
	return err
}

func (s *runQueueStore) ResetProcessingRuns(ctx context.Context) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL
		WHERE status NOT IN (
			'queued',
			'completed','failed','cancelled','task_unsolvable',
			'open','pending_approval'
		)
		AND blueprint_run_id IN (SELECT id FROM blueprint_runs WHERE status = 'running')
	`)
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
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, now()),
		    stop_reason = COALESCE(stop_reason, 'blueprint_terminal'),
		    result_summary = COALESCE(NULLIF(result_summary, ''), $1)
		WHERE status NOT IN ('completed','failed','cancelled','task_unsolvable')
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
		&r.CreatorUserID, &r.BlueprintRunID, &stepIdx, &r.Attempts)
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
