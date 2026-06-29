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

// runTerminalStatusesSQL is the canonical set of terminal runs.status values as
// a SQL IN-list body. The reconcile sweep and the orphaned-child cancel
// (blueprints.go) interpolate it instead of re-spelling the literal, so the
// "non-terminal child" predicate has one definition. Other queries that add
// dormant statuses (open/pending_approval) to this set keep their own list.
const runTerminalStatusesSQL = `'completed','failed','cancelled','task_unsolvable'`

// runQueueClaimCols is the column list ClaimNextRun returns, shared with the
// scan helper. visibility is left at its row default on enqueue; team_id is
// surfaced (TFAC-458) so the construction-path RunInfo built off a claimed run
// carries the owning team for the capture writers.
const runQueueClaimCols = `id, org_id, task_id, COALESCE(prompt_id, ''), status, COALESCE(model, ''),
	COALESCE(worktree_path, ''), COALESCE(session_id, ''), trigger_type, COALESCE(trigger_id, ''),
	COALESCE(creator_user_id, ''), team_id, COALESCE(blueprint_run_id, ''), blueprint_step_index, attempts`

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
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), status, run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.CreatorUserID), nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx)
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
	// Boot self-heal: cancel child runs left non-terminal under a
	// blueprint_run that is already terminal. This is the mirror of
	// ResetProcessingRuns (which requeues active runs under a *running* parent):
	// a child alive under a terminal parent will never be claimed (ClaimNextRun
	// gates on a running parent) nor reset, so without this it sits 'running'
	// forever — the dispatcher treats it as live work and its worktree pins the
	// feature branch, requeuing any sibling fetch into a forever-failing loop.
	// Heals DBs broken before the atomic cancel in MarkRunStatus landed.
	//
	// Roll up the token cache here too (correlated SUM per row) — a 'running'
	// child stranded under a terminal blueprint may have streamed run_messages,
	// so this terminal write must reflect them rather than leave the columns at
	// 0. Same every-terminal-write invariant as the cancel paths (TFAC-473).
	res, err := s.conn.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, ?),
		    stop_reason = COALESCE(stop_reason, 'blueprint_terminal'),
		    result_summary = COALESCE(NULLIF(result_summary, ''), ?),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE run_id = runs.id),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE run_id = runs.id),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE run_id = runs.id),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE run_id = runs.id)
		WHERE status NOT IN (`+runTerminalStatusesSQL+`)
		  AND blueprint_run_id IN (
		      SELECT id FROM blueprint_runs
		      WHERE status IN ('completed','aborted','failed','cancelled')
		  )
	`, time.Now().UTC(), "Cancelled: owning blueprint run reached a terminal state")
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
