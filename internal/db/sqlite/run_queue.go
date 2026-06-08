package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// runQueueStore is the SQLite impl of db.RunQueueStore — the durable run
// queue the delegation dispatcher drains. SQLite/local is single-worker (N=1),
// so ClaimNextRun doesn't need the FOR UPDATE SKIP LOCKED the Postgres impl
// uses; the UPDATE ... WHERE id = (oldest claimable) RETURNING form is atomic
// enough with one dispatcher.
type runQueueStore struct {
	conn *sql.DB
}

func newRunQueueStore(conn *sql.DB) db.RunQueueStore {
	return &runQueueStore{conn: conn}
}

var _ db.RunQueueStore = (*runQueueStore)(nil)

// runQueueClaimCols is the column list ClaimNextRun returns, shared with the
// scan helper. team_id/visibility are left at their row defaults on enqueue.
const runQueueClaimCols = `id, org_id, task_id, COALESCE(prompt_id, ''), status, COALESCE(model, ''),
	COALESCE(worktree_path, ''), COALESCE(session_id, ''), trigger_type, COALESCE(trigger_id, ''),
	COALESCE(creator_user_id, ''), COALESCE(blueprint_run_id, ''), blueprint_step_index, attempts`

func (s *runQueueStore) EnqueueRun(ctx context.Context, orgID string, run domain.AgentRun) error {
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
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.CreatorUserID), nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}

func (s *runQueueStore) ClaimNextRun(ctx context.Context) (*domain.AgentRun, error) {
	// Flip the oldest claimable queued run to 'running', stamp claimed_at, bump
	// attempts. Claimable = its owning blueprint_run is still 'running' and not
	// cancel-requested (a sequence-cancelled blueprint's queued step is never
	// claimed). A NULL subquery (nothing claimable) matches no row, RETURNING
	// yields nothing, and the scan reports ErrNoRows -> (nil, nil).
	row := s.conn.QueryRowContext(ctx, `
		UPDATE runs
		SET status = 'running', claimed_at = ?, attempts = attempts + 1
		WHERE id = (
			SELECT r.id FROM runs r
			JOIN blueprint_runs br ON br.id = r.blueprint_run_id
			WHERE r.status = 'queued'
			  AND br.cancel_requested = 0
			  AND br.status = 'running'
			ORDER BY r.started_at, r.id
			LIMIT 1
		)
		RETURNING `+runQueueClaimCols, time.Now())
	return scanSqliteClaimedRun(row)
}

func (s *runQueueStore) RequeueRun(ctx context.Context, orgID, runID, lastErr string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// attempts is left as-is (the claim counted this try); claimed_at cleared so
	// the row reads clean for the next claim. Guarded on 'running' so a stale
	// requeue can't resurrect a terminal/dormant row.
	_, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL, result_summary = ?
		WHERE id = ? AND status = 'running'
	`, lastErr, runID)
	return err
}

func (s *runQueueStore) ResetProcessingRuns(ctx context.Context) (int, error) {
	// Mid-flight runs left by a crash (claimed/running/setup statuses) go back
	// to 'queued' for re-claim. Terminal + dormant + already-queued rows are
	// left alone. attempts retained so a poison run still fails out.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', claimed_at = NULL
		WHERE status NOT IN (
			'queued',
			'completed','failed','cancelled','task_unsolvable','taken_over',
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

// scanSqliteClaimedRun scans a claimed runs row into *domain.AgentRun.
// (nil, nil) on sql.ErrNoRows so callers treat "nothing claimable" as a
// non-error empty result.
func scanSqliteClaimedRun(row *sql.Row) (*domain.AgentRun, error) {
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
