package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// agentRunStore is the SQLite impl of db.AgentRunStore. SQL bodies
// are ported from the pre-D2 internal/db/agent.go; the only
// behavioral change is the orgID assertion at each method entry
// (SQLite is single-tenant; any non-LocalDefaultOrg value is a
// confused caller).
//
// The constructor takes a single queryer (SQLite has one connection)
// rather than the (app, admin) pair the Postgres impl uses — the
// AgentRunStore was the first store to ship multi-pool before
// SKY-296, and the SQLite side never grew the second arg. The
// `...System` admin-pool variants are thin wrappers around their
// non-System counterparts on the SQLite side.
type agentRunStore struct{ q queryer }

func newAgentRunStore(q queryer) db.AgentRunStore { return &agentRunStore{q: q} }

var _ db.AgentRunStore = (*agentRunStore)(nil)

// --- Lifecycle ---

func (s *agentRunStore) Create(ctx context.Context, orgID string, run domain.AgentRun) error {
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
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	// team_id is the LocalDefaultTeamID sentinel — local mode has
	// exactly one team. SKY-262's schema requires a non-NULL team_id
	// for visibility='team' rows; pre-store callers passed the
	// sentinel directly, and there is no TeamID field on domain
	// AgentRun yet. Multi-mode requires a real team_id and will gain
	// it when the spawner is wired through that path (D9 / SKY-253).
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'team', ?, ?, ?, ?)
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		triggerType, nullIfEmpty(run.TriggerID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.CreatorUserID), nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}

// CreateIfNotFiredSystem is the event-path fenced insert (SKY-424). It
// mirrors Create's column binds but forces trigger_type='event' (so
// creator_user_id stays NULL per the schema CHECK), writes
// triggering_event_id, and adds ON CONFLICT … DO NOTHING against the
// runs_event_trigger_fence partial unique index. inserted=false means a
// run for this (triggering_event_id, trigger_id) already exists — a
// replayed event, skipped cleanly. SQLite has one connection so there is
// no admin/app pool split; the contract matches the Postgres impl.
func (s *agentRunStore) CreateIfNotFiredSystem(ctx context.Context, orgID string, run domain.AgentRun) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// Both halves of the fence key are required: an empty value binds NULL,
	// the partial unique index treats NULL as distinct, and the insert would
	// silently skip the fence. Fail loud — this is the fenced path, and an
	// unfenced run here would defeat its purpose (SKY-424).
	if run.TriggeringEventID == "" || run.TriggerID == "" {
		return false, db.ErrFenceRequiresEventAndTrigger
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	// trigger_type is hard-coded 'event': this method is the event path's
	// only insert, and creator_user_id is left NULL to satisfy the
	// runs_creator_matches_trigger_type CHECK.
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO runs (id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, triggering_event_id, team_id, visibility,
		                  creator_user_id, actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, ?, ?, 'event', ?, ?, ?, 'team', NULL, ?, ?, ?)
		ON CONFLICT (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL DO NOTHING
	`, run.ID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), nullIfEmpty(run.TriggeringEventID), runmode.LocalDefaultTeamID,
		nullIfEmpty(run.ActorAgentID), nullIfEmpty(run.BlueprintRunID), stepIdx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *agentRunStore) Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = ?,
		    completed_at = ?,
		    total_cost_usd = COALESCE(total_cost_usd, 0) + ?,
		    duration_ms = COALESCE(duration_ms, 0) + ?,
		    num_turns = COALESCE(num_turns, 0) + ?,
		    stop_reason = ?,
		    result_summary = ?,
		    outcome = ?,
		    outcome_reason = ?
		WHERE id = ?
	`, status, time.Now(), costUSD, durationMs, numTurns, stopReason, resultSummary,
		nullIfEmpty(outcome), nullIfEmpty(outcomeReason), runID)
	return err
}

func (s *agentRunStore) AddPartialTotals(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET total_cost_usd = COALESCE(total_cost_usd, 0) + ?,
		    duration_ms = COALESCE(duration_ms, 0) + ?,
		    num_turns = COALESCE(num_turns, 0) + ?
		WHERE id = ?
	`, costUSD, durationMs, numTurns, runID)
	return err
}

func (s *agentRunStore) MarkAwaitingInput(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'awaiting_input'
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over', 'awaiting_input')
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkResuming(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'running'
		WHERE id = ? AND status = 'awaiting_input'
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) SetSession(ctx context.Context, orgID, runID, sessionID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE runs SET session_id = ? WHERE id = ?
	`, sessionID, runID)
	return err
}

func (s *agentRunStore) SetStatus(ctx context.Context, orgID, runID, status string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, status, runID)
	return err
}

func (s *agentRunStore) SetWorktreePath(ctx context.Context, orgID, runID, path string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE runs SET worktree_path = ? WHERE id = ?`, path, runID)
	return err
}

func (s *agentRunStore) SetExecutorSystem(ctx context.Context, orgID, runID, executorID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE runs SET executor_id = ? WHERE id = ?`, nullIfEmpty(executorID), runID)
	return err
}

func (s *agentRunStore) MarkFailedIfActive(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', completed_at = COALESCE(completed_at, ?)
		WHERE id = ?
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over')
	`, time.Now().UTC(), runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkPendingApprovalIfCompleted(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'pending_approval' WHERE id = ? AND status = 'completed'
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkCompletedIfPendingApproval(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'completed' WHERE id = ? AND status = 'pending_approval'
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) HasOtherActiveRunForTask(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var exists bool
	err := s.q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM runs
			WHERE task_id = ? AND id != ?
			  AND status NOT IN ('completed','failed','cancelled','task_unsolvable','taken_over','pending_approval')
		)
	`, taskID, excludeRunID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *agentRunStore) MarkTakenOver(ctx context.Context, orgID, runID, takeoverPath, claimUserID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	rolled, err := s.runScoped(ctx, func(tx queryer) error {
		now := time.Now()
		res, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET status = 'taken_over',
			    completed_at = ?,
			    stop_reason = 'user_takeover',
			    result_summary = ?,
			    worktree_path = ?
			WHERE id = ?
			  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
			                     'pending_approval', 'taken_over')
		`, now, "Taken over by user → "+takeoverPath, takeoverPath, runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Race-lost on the run flip. errScopedRollback rolls
			// back the scope (savepoint when composed, tx when
			// standalone) and surfaces (false, nil) to the caller.
			return errScopedRollback
		}

		if claimUserID != "" {
			res, err := tx.ExecContext(ctx, `
				UPDATE tasks
				   SET claimed_by_user_id  = ?,
				       claimed_by_agent_id = NULL
				 WHERE id = (SELECT task_id FROM runs WHERE id = ?)
				   AND claimed_by_agent_id IS NOT NULL
				   AND claimed_by_user_id  IS NULL
			`, claimUserID, runID)
			if err != nil {
				return err
			}
			taskN, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if taskN == 0 {
				return errScopedRollback
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if rolled {
		return false, nil
	}
	return true, nil
}

// runScoped runs fn inside a rollback-safe scope. See
// internal/db/postgres/agentrun.go's runScoped for the full design;
// the SQLite shape is identical (savepoint syntax is portable). When
// s.q is a *sql.Tx (composed inside an outer WithTx), the scope is
// a SAVEPOINT so partial failure doesn't leak; when s.q is *sql.DB,
// runScoped opens a fresh tx.
func (s *agentRunStore) runScoped(ctx context.Context, fn func(queryer) error) (rolledBack bool, err error) {
	switch v := s.q.(type) {
	case *sql.Tx:
		sp := scopedSavepointName()
		if _, err := v.ExecContext(ctx, "SAVEPOINT "+sp); err != nil {
			return false, err
		}
		fnErr := fn(v)
		if fnErr == nil {
			if _, err := v.ExecContext(ctx, "RELEASE SAVEPOINT "+sp); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, rerr := v.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+sp); rerr != nil {
			return false, rerr
		}
		if _, rerr := v.ExecContext(ctx, "RELEASE SAVEPOINT "+sp); rerr != nil {
			return false, rerr
		}
		if errors.Is(fnErr, errScopedRollback) {
			return true, nil
		}
		return false, fnErr
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
		if fnErr := fn(tx); fnErr != nil {
			if errors.Is(fnErr, errScopedRollback) {
				return true, nil
			}
			return false, fnErr
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("sqlite agentrun: unexpected queryer type %T", v)
	}
}

// errScopedRollback is the sentinel fn returns to ask runScoped for
// a non-error rollback of the current scope.
var errScopedRollback = errors.New("agentrun: scoped rollback")

func scopedSavepointName() string {
	return fmt.Sprintf("agentrun_scope_%d", time.Now().UnixNano())
}

func (s *agentRunStore) MarkReleased(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET worktree_path = '',
		    result_summary = CASE
		        WHEN COALESCE(result_summary, '') = '' THEN 'released by user'
		        ELSE result_summary || '; released by user'
		    END
		WHERE id = ?
		  AND status = 'taken_over'
		  AND COALESCE(worktree_path, '') != ''
	`, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	now := time.Now()
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled', completed_at = ?, stop_reason = ?, result_summary = ?
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over')
	`, now, stopReason, summary, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkDiscarded(ctx context.Context, orgID, runID, stopReason string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	now := time.Now()
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, ?),
		    stop_reason = ?,
		    result_summary = COALESCE(NULLIF(result_summary, ''), ?)
		WHERE id = ? AND status = 'pending_approval'
	`, now, stopReason, "Review discarded by user.", runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- Queries ---

// sqliteRunColumns is the SELECT list scanned into a domain.AgentRun
// via scanAgentRun. Same shape as Postgres' pgRunColumns; the
// memory_missing derivation uses SQLite's TRIM(...) variant with the
// explicit whitespace charset (Postgres uses BTRIM with an E'...'
// escape string).
const sqliteRunColumns = `
	r.id, r.task_id, r.status, r.model, r.started_at, r.completed_at,
	r.total_cost_usd, r.duration_ms, r.num_turns, r.stop_reason, r.worktree_path,
	r.result_summary, r.outcome, r.outcome_reason, r.session_id, r.actor_agent_id,
	COALESCE(r.trigger_type, ''),
	r.creator_user_id,
	r.executor_id,
	r.blueprint_run_id, r.blueprint_step_index,
	(NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing
`

func (s *agentRunStore) Get(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		WHERE r.id = ?
	`, runID)

	var r domain.AgentRun
	if err := scanAgentRun(row, &r); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func (s *agentRunStore) ListForTask(ctx context.Context, orgID, taskID string) ([]domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		WHERE r.task_id = ?
		ORDER BY r.started_at DESC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []domain.AgentRun
	for rows.Next() {
		var r domain.AgentRun
		if err := scanAgentRunRows(rows, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *agentRunStore) PendingApprovalIDForTask(ctx context.Context, orgID, taskID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var id string
	err := s.q.QueryRowContext(ctx,
		`SELECT id FROM runs WHERE task_id = ? AND status = 'pending_approval' LIMIT 1`,
		taskID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *agentRunStore) HasActiveForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled',
		                                      'task_unsolvable', 'pending_approval', 'taken_over')
	`, taskID).Scan(&count)
	return count > 0, err
}

// HasActiveAutoRunForEntity is the per-entity sibling of
// HasActiveForTask: any non-terminal trigger_type='event' run on any
// task that belongs to the entity. Manual delegations are excluded.
// Used by the router's per-entity firing gate (SKY-189).
func (s *agentRunStore) HasActiveAutoRunForEntity(ctx context.Context, orgID, entityID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs r
		JOIN tasks t ON t.id = r.task_id
		WHERE t.entity_id = ?
		  AND r.trigger_type = 'event'
		  AND r.status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                       'pending_approval', 'taken_over')
	`, entityID).Scan(&count)
	return count > 0, err
}

func (s *agentRunStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE task_id = ? AND status NOT IN ('completed', 'failed', 'cancelled',
		                                      'task_unsolvable', 'pending_approval', 'taken_over')
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- Admin-pool variants ---
//
// All `...System` methods below delegate straight through to their
// non-System counterparts. SQLite has one connection so the pool
// distinction doesn't exist; the wrappers are kept for signature
// parity with Postgres. The delegate spawner consumes these from
// its goroutine paths that detach from the request context.

func (s *agentRunStore) ListTakenOverIDsSystem(ctx context.Context, orgID string) ([]string, error) {
	return s.ListTakenOverIDs(ctx, orgID)
}

func (s *agentRunStore) HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error) {
	return s.HasActiveAutoRunForEntity(ctx, orgID, entityID)
}

func (s *agentRunStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return s.ActiveIDsForTask(ctx, orgID, taskID)
}

func (s *agentRunStore) GetSystem(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	return s.Get(ctx, orgID, runID)
}

func (s *agentRunStore) LookupOrgForRunSystem(ctx context.Context, runID string) (string, error) {
	var orgID string
	err := s.q.QueryRowContext(ctx, `SELECT org_id FROM runs WHERE id = ?`, runID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return orgID, err
}

func (s *agentRunStore) CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error {
	return s.Complete(ctx, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason)
}

func (s *agentRunStore) AddPartialTotalsSystem(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	return s.AddPartialTotals(ctx, orgID, runID, costUSD, durationMs, numTurns)
}

func (s *agentRunStore) MarkAwaitingInputSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkAwaitingInput(ctx, orgID, runID)
}

func (s *agentRunStore) MarkResumingSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkResuming(ctx, orgID, runID)
}

func (s *agentRunStore) SetSessionSystem(ctx context.Context, orgID, runID, sessionID string) error {
	return s.SetSession(ctx, orgID, runID, sessionID)
}

func (s *agentRunStore) SetStatusSystem(ctx context.Context, orgID, runID, status string) error {
	return s.SetStatus(ctx, orgID, runID, status)
}

func (s *agentRunStore) SetWorktreePathSystem(ctx context.Context, orgID, runID, path string) error {
	return s.SetWorktreePath(ctx, orgID, runID, path)
}

func (s *agentRunStore) HasOtherActiveRunForTaskSystem(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error) {
	return s.HasOtherActiveRunForTask(ctx, orgID, taskID, excludeRunID)
}

func (s *agentRunStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkFailedIfActive(ctx, orgID, runID)
}

func (s *agentRunStore) MarkPendingApprovalIfCompletedSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkPendingApprovalIfCompleted(ctx, orgID, runID)
}

func (s *agentRunStore) MarkReleasedSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkReleased(ctx, orgID, runID)
}

func (s *agentRunStore) MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return s.MarkCancelledIfActive(ctx, orgID, runID, stopReason, summary)
}

func (s *agentRunStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error) {
	return s.InsertMessage(ctx, orgID, msg)
}

func (s *agentRunStore) InsertYieldRequestSystem(ctx context.Context, orgID, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
	return s.InsertYieldRequest(ctx, orgID, runID, req)
}

func (s *agentRunStore) ListTakenOverIDs(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT id FROM runs WHERE status = 'taken_over' AND COALESCE(worktree_path, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *agentRunStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
	return s.ListParkedWorktreePaths(ctx, orgID)
}

func (s *agentRunStore) ListParkedWorktreePaths(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT worktree_path FROM runs
		 WHERE status IN ('awaiting_input', 'pending_approval')
		   AND COALESCE(worktree_path, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

func (s *agentRunStore) ListTakenOverForResume(ctx context.Context, orgID string) ([]domain.TakenOverRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// completed_at and started_at returned as raw columns rather than
	// COALESCE'd into one — the SQLite driver can scan a column of
	// declared DATETIME type into sql.NullTime, but a COALESCE
	// expression strips the type metadata and the result comes back
	// as an unparseable string. ORDER BY uses COALESCE because string
	// sort over ISO-8601 happens to be correct ordering.
	rows, err := s.q.QueryContext(ctx, `
		SELECT r.id, COALESCE(r.session_id, ''), COALESCE(r.worktree_path, ''),
		       COALESCE(e.title, ''), COALESCE(e.source_id, ''),
		       r.completed_at, r.started_at
		FROM runs r
		LEFT JOIN tasks t ON t.id = r.task_id
		LEFT JOIN entities e ON e.id = t.entity_id
		WHERE r.status = 'taken_over'
		  AND COALESCE(r.worktree_path, '') != ''
		ORDER BY COALESCE(r.completed_at, r.started_at) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TakenOverRun
	for rows.Next() {
		var r domain.TakenOverRun
		var completedAt, startedAt sql.NullTime
		if err := rows.Scan(&r.RunID, &r.SessionID, &r.WorktreePath, &r.TaskTitle, &r.SourceID, &completedAt, &startedAt); err != nil {
			return nil, err
		}
		if r.SessionID == "" || r.WorktreePath == "" {
			continue
		}
		switch {
		case completedAt.Valid:
			r.CompletedAt = completedAt.Time
		case startedAt.Valid:
			r.CompletedAt = startedAt.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *agentRunStore) EntitiesWithAwaitingInput(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(entityIDs))
	args := make([]any, 0, len(entityIDs))
	for i, id := range entityIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `
		SELECT DISTINCT t.entity_id
		FROM runs r
		JOIN tasks t ON t.id = r.task_id
		WHERE r.status = 'awaiting_input'
		  AND t.entity_id IN (` + strings.Join(placeholders, ",") + `)
	`
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// --- Transcript / messages ---

func (s *agentRunStore) InsertMessage(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var toolCallsJSON, metadataJSON sql.NullString

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	// SQLite uses AUTOINCREMENT on run_messages.id, so LastInsertId
	// on the Result gives us the assigned row id. Postgres uses a
	// sequence + RETURNING — see postgres/agentrun.go.
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO run_messages (run_id, role, content, subtype, tool_calls, tool_call_id,
		                          is_error, metadata, model,
		                          input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.RunID, msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, sqliteNullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		sqliteNullStr(msg.Model), sqliteNullInt(msg.InputTokens), sqliteNullInt(msg.OutputTokens),
		sqliteNullInt(msg.CacheReadTokens), sqliteNullInt(msg.CacheCreationTokens),
		msg.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *agentRunStore) Messages(ctx context.Context, orgID, runID string) ([]domain.AgentMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
		       model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
		FROM run_messages WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []domain.AgentMessage
	for rows.Next() {
		var m domain.AgentMessage
		var content, subtype, toolCallsStr, toolCallID, metadataStr, model sql.NullString
		var inputTok, outputTok, cacheReadTok, cacheCreateTok sql.NullInt64

		if err := rows.Scan(
			&m.ID, &m.RunID, &m.Role, &content, &subtype, &toolCallsStr,
			&toolCallID, &m.IsError, &metadataStr, &model,
			&inputTok, &outputTok, &cacheReadTok, &cacheCreateTok, &m.CreatedAt,
		); err != nil {
			return nil, err
		}

		m.Content = content.String
		m.Subtype = subtype.String
		m.ToolCallID = toolCallID.String
		m.Model = model.String

		if toolCallsStr.Valid {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid {
			_ = json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
		}
		if inputTok.Valid {
			v := int(inputTok.Int64)
			m.InputTokens = &v
		}
		if outputTok.Valid {
			v := int(outputTok.Int64)
			m.OutputTokens = &v
		}
		if cacheReadTok.Valid {
			v := int(cacheReadTok.Int64)
			m.CacheReadTokens = &v
		}
		if cacheCreateTok.Valid {
			v := int(cacheCreateTok.Int64)
			m.CacheCreationTokens = &v
		}

		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (s *agentRunStore) TokenTotals(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM run_messages
		WHERE run_id = ? AND role = 'assistant'
	`, runID)

	var t domain.TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *agentRunStore) TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	return s.TokenTotals(ctx, orgID, runID)
}

// --- Yields ---

func (s *agentRunStore) InsertYieldRequest(ctx context.Context, orgID, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal yield request: %w", err)
	}
	msg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "assistant",
		Subtype: db.YieldRequestSubtype,
		Content: string(payload),
	}
	id, err := s.InsertMessage(ctx, orgID, msg)
	if err != nil {
		return nil, err
	}
	msg.ID = int(id)
	return msg, nil
}

func (s *agentRunStore) InsertYieldResponse(ctx context.Context, orgID, runID string, resp *domain.YieldResponse, displayContent string) (*domain.AgentMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal yield response: %w", err)
	}
	msg := &domain.AgentMessage{
		RunID:   runID,
		Role:    "user",
		Subtype: db.YieldResponseSubtype,
		Content: displayContent,
		Metadata: map[string]any{
			"yield_response": json.RawMessage(payload),
		},
	}
	id, err := s.InsertMessage(ctx, orgID, msg)
	if err != nil {
		return nil, err
	}
	msg.ID = int(id)
	return msg, nil
}

func (s *agentRunStore) LatestYieldRequest(ctx context.Context, orgID, runID string) (*domain.YieldRequest, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT content FROM run_messages
		WHERE run_id = ? AND subtype = ?
		ORDER BY id DESC LIMIT 1
	`, runID, db.YieldRequestSubtype)
	var content sql.NullString
	if err := row.Scan(&content); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !content.Valid || content.String == "" {
		return nil, nil
	}
	var req domain.YieldRequest
	if err := json.Unmarshal([]byte(content.String), &req); err != nil {
		return nil, fmt.Errorf("unmarshal yield request: %w", err)
	}
	return &req, nil
}

// --- Helpers ---

func scanAgentRun(row *sql.Row, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, outcome, outcomeReason, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &executorID, &blueprintRunID, &blueprintStep,
		&r.MemoryMissing,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
	return nil
}

func scanAgentRunRows(rows *sql.Rows, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, outcome, outcomeReason, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &executorID, &blueprintRunID, &blueprintStep,
		&r.MemoryMissing,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
	return nil
}

func finalizeAgentRun(r *domain.AgentRun, completedAt sql.NullTime, costUSD sql.NullFloat64,
	durationMs, numTurns, blueprintStep sql.NullInt64,
	model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID sql.NullString) {
	r.Model = model.String
	r.StopReason = stopReason.String
	r.WorktreePath = worktreePath.String
	r.ResultSummary = resultSummary.String
	r.Outcome = outcome.String
	r.OutcomeReason = outcomeReason.String
	r.SessionID = sessionID.String
	r.ActorAgentID = actorAgentID.String
	r.CreatorUserID = creatorUserID.String
	r.ExecutorID = executorID.String
	if blueprintRunID.Valid {
		r.BlueprintRunID = blueprintRunID.String
	}
	if blueprintStep.Valid {
		v := int(blueprintStep.Int64)
		r.BlueprintStepIndex = &v
	}
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if costUSD.Valid {
		r.TotalCostUSD = &costUSD.Float64
	}
	if durationMs.Valid {
		v := int(durationMs.Int64)
		r.DurationMs = &v
	}
	if numTurns.Valid {
		v := int(numTurns.Int64)
		r.NumTurns = &v
	}
}

// nullIfEmpty maps "" to a SQL NULL bind. Local mirror of the
// helper that lives in postgres/agentrun.go; the two impls are
// independent so neither imports the other's private helpers.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// sqliteNullStr / sqliteNullInt produce typed NullX values for the
// run_messages INSERT — pre-D2 these lived as nullStr / nullInt on
// internal/db/agent.go. Renamed with the sqlite prefix here to make
// it obvious this is the SQLite path's flavor (the *sql.NullX
// concrete types are SQLite-idiomatic; pgx accepts the same types
// but the Postgres impl uses raw any for clarity).
func sqliteNullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func sqliteNullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
