package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// agentRunStore is the Postgres impl of db.AgentRunStore. Holds two
// pools (see postgres.New): `q` is the app pool (or a *sql.Tx
// composed from it via WithTx) — every request-equivalent path
// runs here, RLS-active under tf_app. `admin` is the supabase_admin
// BYPASSRLS pool, used for the one write path that can't satisfy
// the app-pool RLS predicate: event-triggered run inserts. Those
// rows have creator_user_id NULL by the runs_creator_matches_trigger_type
// CHECK, but the runs_insert RLS policy requires
// `creator_user_id = tf.current_user_id()` — mutually exclusive, so
// the app pool can't insert them at all. Routing event-triggered
// Create through admin is the same shape PromptStore.SeedOrUpdate
// uses for its system-write path.
//
// SQL is written fresh against D3's schema: org_id in every WHERE
// clause as defense in depth alongside RLS, $N placeholders, JSONB
// extraction for tool_calls / metadata, RETURNING id for the
// run_messages auto-increment (Postgres has a sequence, not
// AUTOINCREMENT).
type agentRunStore struct {
	q     queryer
	admin queryer
}

func newAgentRunStore(q, admin queryer) db.AgentRunStore {
	return &agentRunStore{q: q, admin: admin}
}

var _ db.AgentRunStore = (*agentRunStore)(nil)

// --- Lifecycle ---

func (s *agentRunStore) Create(ctx context.Context, orgID string, run domain.AgentRun) error {
	triggerType := run.TriggerType
	if triggerType == "" {
		triggerType = "manual"
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	// team_id resolves from the parent task — runs inherit team
	// scope from their task so team-scoped queue / Board filters
	// attribute the run consistently. Pre-fix this read the org's
	// oldest team, which misattributed runs whose task belonged to
	// a different team. SKY-285 review.
	if triggerType == "event" {
		return s.createEventTriggered(ctx, orgID, run, stepIdx)
	}
	return s.createManual(ctx, orgID, run, stepIdx)
}

// createEventTriggered routes through the admin pool (BYPASSRLS).
// Two reasons:
//
//   - The runs_creator_matches_trigger_type CHECK requires
//     trigger_type='event' rows to have creator_user_id IS NULL,
//     but the runs_insert RLS policy requires
//     creator_user_id = tf.current_user_id() for every insert under
//     tf_app — the two conditions are mutually exclusive, so the
//     app pool can't insert event-triggered runs at all.
//   - The boundary is also a security guarantee: event runs are
//     system-emitted (eventbus → spawner). Pool routing enforces
//     "only server-side code with admin pool access can create
//     event-triggered runs" rather than relying on application
//     layer guards. Same pattern as PromptStore.SeedOrUpdate.
//
// **Nuance**: the admin pool is a separate connection, so this
// insert commits autonomously from any outer WithTx the caller
// might be composed inside. If a future caller wraps an
// event-triggered Create + another write under WithTx, the run
// insert will land even if the outer tx rolls back. No caller
// composes this path today; document the edge for whoever
// eventually needs atomic event-create + adjacent writes.
//
// The SQL is split from the manual path because Postgres needs
// USAGE on schema `tf` to plan a reference to `tf.current_user_id()`,
// and the admin role (supabase_admin) running this insert
// shouldn't need to touch `tf` at all.
func (s *agentRunStore) createEventTriggered(ctx context.Context, orgID string, run domain.AgentRun, stepIdx any) error {
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO runs (id, org_id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'event', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team', NULL,
		        $9, $10, $11)
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID),
		nullIfEmpty(run.ActorAgentID), nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}

// createManual runs on the app pool (RLS-active under tf_app).
// CreatorUserID resolution: the SQLite-shaped LocalDefaultUserID
// sentinel has no FK target in a multi-mode `users` table, so any
// caller that still passes it (the pre-store-migration spawner does
// for manual delegations) would fail runs_creator_user_id_fkey.
// Treat the sentinel as empty here so the COALESCE walks to
// tf.current_user_id() (the JWT-claimed user, set by WithTx) or
// the org owner. The schema CHECK requires non-NULL creator for
// trigger_type='manual'; org-owner is the only universally
// available non-null in production multi-mode.
//
// The sentinel filter is transitional: D9 / SKY-253 will rewire
// the spawner to pass the request user from auth context, after
// which this filter becomes dead code.
func (s *agentRunStore) createManual(ctx context.Context, orgID string, run domain.AgentRun, stepIdx any) error {
	creatorBind := run.CreatorUserID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO runs (id, org_id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual', $8,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team',
		        COALESCE(
		            NULLIF($9, '')::uuid,
		            tf.current_user_id(),
		            (SELECT owner_user_id FROM orgs WHERE id = $2)
		        ),
		        $10, $11, $12)
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID),
		creatorBind, nullIfEmpty(run.ActorAgentID),
		nullIfEmpty(run.BlueprintRunID), stepIdx)
	return err
}

// CreateIfNotFiredSystem is the event-path fenced insert (SKY-424). Same
// admin-pool routing and team_id derivation as createEventTriggered (the
// runs_creator_matches_trigger_type CHECK + runs_insert RLS make the app
// pool unable to write trigger_type='event' rows), plus triggering_event_id
// and ON CONFLICT … DO NOTHING against the runs_event_trigger_fence partial
// unique index. inserted=false means a run for this (triggering_event_id,
// trigger_id) already committed — a replayed event, skipped cleanly. The
// run insert is the crash-consistent commit point, so the fence is exact:
// fence row exists iff run exists.
func (s *agentRunStore) CreateIfNotFiredSystem(ctx context.Context, orgID string, run domain.AgentRun) (bool, error) {
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
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO runs (id, org_id, task_id, prompt_id, status, model, worktree_path,
		                  trigger_type, trigger_id, triggering_event_id, team_id, visibility, creator_user_id,
		                  actor_agent_id, blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'event', $8, $9,
		        (SELECT team_id FROM tasks WHERE id = $3 AND org_id = $2),
		        'team', NULL,
		        $10, $11, $12)
		ON CONFLICT (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL DO NOTHING
	`, run.ID, orgID, run.TaskID, nullIfEmpty(run.PromptID), run.Status, run.Model, run.WorktreePath,
		nullIfEmpty(run.TriggerID), nullIfEmpty(run.TriggeringEventID),
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
	return completeRun(ctx, s.q, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason)
}

func (s *agentRunStore) CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error {
	return completeRun(ctx, s.admin, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason)
}

func completeRun(ctx context.Context, q queryer, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs
		SET status = $1,
		    completed_at = $2,
		    total_cost_usd = COALESCE(total_cost_usd, 0) + $3,
		    duration_ms = COALESCE(duration_ms, 0) + $4,
		    num_turns = COALESCE(num_turns, 0) + $5,
		    stop_reason = $6,
		    result_summary = $7,
		    outcome = NULLIF($8, ''),
		    outcome_reason = NULLIF($9, '')
		WHERE org_id = $10 AND id = $11
	`, status, time.Now(), costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason, orgID, runID)
	return err
}

func (s *agentRunStore) AddPartialTotals(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	return addPartialTotals(ctx, s.q, orgID, runID, costUSD, durationMs, numTurns)
}

func (s *agentRunStore) AddPartialTotalsSystem(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	return addPartialTotals(ctx, s.admin, orgID, runID, costUSD, durationMs, numTurns)
}

func addPartialTotals(ctx context.Context, q queryer, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs
		SET total_cost_usd = COALESCE(total_cost_usd, 0) + $1,
		    duration_ms = COALESCE(duration_ms, 0) + $2,
		    num_turns = COALESCE(num_turns, 0) + $3
		WHERE org_id = $4 AND id = $5
	`, costUSD, durationMs, numTurns, orgID, runID)
	return err
}

func (s *agentRunStore) MarkAwaitingInput(ctx context.Context, orgID, runID string) (bool, error) {
	return markAwaitingInput(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkAwaitingInputSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markAwaitingInput(ctx, s.admin, orgID, runID)
}

func markAwaitingInput(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'awaiting_input'
		WHERE org_id = $1 AND id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over', 'awaiting_input')
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkResuming(ctx context.Context, orgID, runID string) (bool, error) {
	return markResuming(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkResumingSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markResuming(ctx, s.admin, orgID, runID)
}

func markResuming(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE runs SET status = 'running'
		WHERE org_id = $1 AND id = $2 AND status = 'awaiting_input'
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) SetSession(ctx context.Context, orgID, runID, sessionID string) error {
	return setRunSession(ctx, s.q, orgID, runID, sessionID)
}

func (s *agentRunStore) SetSessionSystem(ctx context.Context, orgID, runID, sessionID string) error {
	return setRunSession(ctx, s.admin, orgID, runID, sessionID)
}

func setRunSession(ctx context.Context, q queryer, orgID, runID, sessionID string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs SET session_id = $1 WHERE org_id = $2 AND id = $3
	`, sessionID, orgID, runID)
	return err
}

func (s *agentRunStore) SetStatus(ctx context.Context, orgID, runID, status string) error {
	return setRunStatus(ctx, s.q, orgID, runID, status)
}

func (s *agentRunStore) SetStatusSystem(ctx context.Context, orgID, runID, status string) error {
	return setRunStatus(ctx, s.admin, orgID, runID, status)
}

func setRunStatus(ctx context.Context, q queryer, orgID, runID, status string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs SET status = $1 WHERE org_id = $2 AND id = $3
	`, status, orgID, runID)
	return err
}

func (s *agentRunStore) SetWorktreePath(ctx context.Context, orgID, runID, path string) error {
	return setRunWorktreePath(ctx, s.q, orgID, runID, path)
}

func (s *agentRunStore) SetWorktreePathSystem(ctx context.Context, orgID, runID, path string) error {
	return setRunWorktreePath(ctx, s.admin, orgID, runID, path)
}

func setRunWorktreePath(ctx context.Context, q queryer, orgID, runID, path string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE runs SET worktree_path = $1 WHERE org_id = $2 AND id = $3
	`, path, orgID, runID)
	return err
}

func (s *agentRunStore) HasOtherActiveRunForTask(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error) {
	return hasOtherActiveRunForTask(ctx, s.q, orgID, taskID, excludeRunID)
}

func (s *agentRunStore) HasOtherActiveRunForTaskSystem(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error) {
	return hasOtherActiveRunForTask(ctx, s.admin, orgID, taskID, excludeRunID)
}

func (s *agentRunStore) MarkFailedIfActive(ctx context.Context, orgID, runID string) (bool, error) {
	return markFailedIfActive(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markFailedIfActive(ctx, s.admin, orgID, runID)
}

func markFailedIfActive(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', completed_at = COALESCE(completed_at, $1)
		WHERE org_id = $2 AND id = $3
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over')
	`, time.Now().UTC(), orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkPendingApprovalIfCompleted(ctx context.Context, orgID, runID string) (bool, error) {
	return markPendingApprovalIfCompleted(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkPendingApprovalIfCompletedSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markPendingApprovalIfCompleted(ctx, s.admin, orgID, runID)
}

func markPendingApprovalIfCompleted(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE runs SET status = 'pending_approval'
		WHERE org_id = $1 AND id = $2 AND status = 'completed'
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkCompletedIfPendingApproval(ctx context.Context, orgID, runID string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'completed'
		WHERE org_id = $1 AND id = $2 AND status = 'pending_approval'
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func hasOtherActiveRunForTask(ctx context.Context, q queryer, orgID, taskID, excludeRunID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM runs
			WHERE org_id = $1 AND task_id = $2 AND id != $3
			  AND status NOT IN ('completed','failed','cancelled','task_unsolvable','taken_over','pending_approval')
		)
	`, orgID, taskID, excludeRunID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *agentRunStore) MarkTakenOver(ctx context.Context, orgID, runID, takeoverPath, claimUserID string) (bool, error) {
	rolled, err := s.runScoped(ctx, func(tx queryer) error {
		now := time.Now()
		res, err := tx.ExecContext(ctx, `
			UPDATE runs
			SET status = 'taken_over',
			    completed_at = $1,
			    stop_reason = 'user_takeover',
			    result_summary = $2,
			    worktree_path = $3
			WHERE org_id = $4 AND id = $5
			  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
			                     'pending_approval', 'taken_over')
		`, now, "Taken over by user → "+takeoverPath, takeoverPath, orgID, runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Race-lost on the run flip — run is already terminal.
			// errScopedRollback tells runScoped to roll back the
			// scope (savepoint when composed, tx when standalone)
			// and surface (false, nil) to the caller.
			return errScopedRollback
		}

		if claimUserID != "" {
			// Race-safety guards on the task UPDATE — only fire when
			// the bot still holds the claim AND no user has stepped
			// in. Postgres READ COMMITTED can let another tx commit
			// changes to the task between our run UPDATE and this
			// one; without the guards the takeover would silently
			// overwrite a concurrent swipe-claim takeover or a
			// /requeue's clear.
			res, err := tx.ExecContext(ctx, `
				UPDATE tasks
				   SET claimed_by_user_id  = $1,
				       claimed_by_agent_id = NULL
				 WHERE org_id = $2
				   AND id = (SELECT task_id FROM runs WHERE org_id = $2 AND id = $3)
				   AND claimed_by_agent_id IS NOT NULL
				   AND claimed_by_user_id  IS NULL
			`, claimUserID, orgID, runID)
			if err != nil {
				return err
			}
			taskN, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if taskN == 0 {
				// Race-lost on the task claim axis. Rolling back
				// the scope unwinds the run UPDATE too — both
				// statements are atomic with respect to outer
				// state.
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

func (s *agentRunStore) MarkReleased(ctx context.Context, orgID, runID string) (bool, error) {
	return markReleased(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkReleasedSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markReleased(ctx, s.admin, orgID, runID)
}

func markReleased(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET worktree_path = '',
		    result_summary = CASE
		        WHEN COALESCE(result_summary, '') = '' THEN 'released by user'
		        ELSE result_summary || '; released by user'
		    END
		WHERE org_id = $1
		  AND id = $2
		  AND status = 'taken_over'
		  AND COALESCE(worktree_path, '') != ''
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return markCancelledIfActive(ctx, s.q, orgID, runID, stopReason, summary)
}

func (s *agentRunStore) MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return markCancelledIfActive(ctx, s.admin, orgID, runID, stopReason, summary)
}

func markCancelledIfActive(ctx context.Context, q queryer, orgID, runID, stopReason, summary string) (bool, error) {
	now := time.Now()
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled', completed_at = $1, stop_reason = $2, result_summary = $3
		WHERE org_id = $4 AND id = $5
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over')
	`, now, stopReason, summary, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkDiscarded(ctx context.Context, orgID, runID, stopReason string) (bool, error) {
	now := time.Now()
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled',
		    completed_at = COALESCE(completed_at, $1),
		    stop_reason = $2,
		    result_summary = COALESCE(NULLIF(result_summary, ''), $3)
		WHERE org_id = $4 AND id = $5 AND status = 'pending_approval'
	`, now, stopReason, "Review discarded by user.", orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- Queries ---

// pgRunColumns is the SELECT list scanned into a domain.AgentRun
// via scanAgentRun. Owned here on AgentRunStore; sibling Postgres
// stores that need to project a run (e.g. factoryReadStore.ActiveRuns)
// already use their own copy because they also project task+entity
// JOINs. Keeping this here keeps the simple "just the run" projection
// uncoupled from those.
const pgRunColumns = `
	r.id, r.task_id, r.status, COALESCE(r.model, ''), r.started_at, r.completed_at,
	r.total_cost_usd, r.duration_ms, r.num_turns,
	COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
	COALESCE(r.result_summary, ''),
	COALESCE(r.outcome, ''), COALESCE(r.outcome_reason, ''),
	COALESCE(r.session_id, ''),
	COALESCE(r.actor_agent_id::text, ''),
	COALESCE(r.trigger_type, ''),
	COALESCE(r.creator_user_id::text, ''),
	r.blueprint_run_id, r.blueprint_step_index,
	(NULLIF(BTRIM(rm.agent_content, E' \t\n\r'), '') IS NULL) AS memory_missing
`

func (s *agentRunStore) Get(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	return getRun(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) GetSystem(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	return getRun(ctx, s.admin, orgID, runID)
}

func (s *agentRunStore) LookupOrgForRunSystem(ctx context.Context, runID string) (string, error) {
	var orgID string
	err := s.admin.QueryRowContext(ctx, `SELECT org_id::text FROM runs WHERE id = $1`, runID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return orgID, err
}

func getRun(ctx context.Context, q queryer, orgID, runID string) (*domain.AgentRun, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id AND rm.org_id = r.org_id
		WHERE r.org_id = $1 AND r.id = $2
	`, orgID, runID)

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
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id AND rm.org_id = r.org_id
		WHERE r.org_id = $1 AND r.task_id = $2
		ORDER BY r.started_at DESC
	`, orgID, taskID)
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
	var id string
	err := s.q.QueryRowContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND task_id = $2 AND status = 'pending_approval'
		LIMIT 1
	`, orgID, taskID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *agentRunStore) HasActiveForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE org_id = $1 AND task_id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over')
	`, orgID, taskID).Scan(&count)
	return count > 0, err
}

// HasActiveAutoRunForEntity is the per-entity sibling of
// HasActiveForTask: any non-terminal trigger_type='event' run on any
// task that belongs to the entity. Manual delegations are excluded.
// Used by the router's per-entity firing gate (SKY-189).
func (s *agentRunStore) HasActiveAutoRunForEntity(ctx context.Context, orgID, entityID string) (bool, error) {
	return hasActiveAutoRunForEntity(ctx, s.q, orgID, entityID)
}

func (s *agentRunStore) HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error) {
	return hasActiveAutoRunForEntity(ctx, s.admin, orgID, entityID)
}

func hasActiveAutoRunForEntity(ctx context.Context, q queryer, orgID, entityID string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND t.entity_id = $2
		  AND r.trigger_type = 'event'
		  AND r.status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                       'pending_approval', 'taken_over')
	`, orgID, entityID).Scan(&count)
	return count > 0, err
}

func (s *agentRunStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.q, orgID, taskID)
}

func (s *agentRunStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.admin, orgID, taskID)
}

func activeRunIDsForTask(ctx context.Context, q queryer, orgID, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND task_id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'taken_over')
	`, orgID, taskID)
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

func (s *agentRunStore) ListTakenOverIDs(ctx context.Context, orgID string) ([]string, error) {
	return listTakenOverRunIDs(ctx, s.q, orgID)
}

func (s *agentRunStore) ListTakenOverIDsSystem(ctx context.Context, orgID string) ([]string, error) {
	return listTakenOverRunIDs(ctx, s.admin, orgID)
}

func listTakenOverRunIDs(ctx context.Context, q queryer, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND status = 'taken_over' AND COALESCE(worktree_path, '') != ''
	`, orgID)
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

func (s *agentRunStore) ListTakenOverForResume(ctx context.Context, orgID string) ([]domain.TakenOverRun, error) {
	// Postgres' pgx round-trips timestamps cleanly even through
	// COALESCE, so the SQLite-side dance around stripped type
	// metadata isn't needed here. ORDER BY uses COALESCE directly.
	rows, err := s.q.QueryContext(ctx, `
		SELECT r.id, COALESCE(r.session_id, ''), COALESCE(r.worktree_path, ''),
		       COALESCE(e.title, ''), COALESCE(e.source_id, ''),
		       r.completed_at, r.started_at
		FROM runs r
		LEFT JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		LEFT JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE r.org_id = $1
		  AND r.status = 'taken_over'
		  AND COALESCE(r.worktree_path, '') != ''
		ORDER BY COALESCE(r.completed_at, r.started_at) DESC
	`, orgID)
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
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM runs r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND r.status = 'awaiting_input'
		  AND t.entity_id = ANY($2)
	`, orgID, entityIDs)
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
	return insertRunMessage(ctx, s.q, orgID, msg)
}

func (s *agentRunStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error) {
	return insertRunMessage(ctx, s.admin, orgID, msg)
}

func insertRunMessage(ctx context.Context, q queryer, orgID string, msg *domain.AgentMessage) (int64, error) {
	var toolCallsJSON, metadataJSON []byte

	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = b
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return 0, fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = b
	}

	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	// Postgres uses a sequence on run_messages.id, so we get the
	// auto-assigned id back via RETURNING rather than the
	// LastInsertId Result method (which pgx doesn't implement).
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO run_messages (org_id, run_id, role, content, subtype, tool_calls,
		                          tool_call_id, is_error, metadata, model,
		                          input_tokens, output_tokens,
		                          cache_read_tokens, cache_creation_tokens, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id
	`,
		orgID, msg.RunID, msg.Role, msg.Content, msg.Subtype,
		nullableJSONB(toolCallsJSON), nullIfEmpty(msg.ToolCallID), msg.IsError,
		nullableJSONB(metadataJSON), nullIfEmpty(msg.Model),
		nullIntPtr(msg.InputTokens), nullIntPtr(msg.OutputTokens),
		nullIntPtr(msg.CacheReadTokens), nullIntPtr(msg.CacheCreationTokens),
		msg.CreatedAt,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// nullableJSONB returns NULL for empty input so the JSONB column
// stays unset (matching SQLite's behavior where the TEXT column is
// NULL when no data). pgx accepts []byte for JSONB binding.
func nullableJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *agentRunStore) Messages(ctx context.Context, orgID, runID string) ([]domain.AgentMessage, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, run_id, role, content, subtype, tool_calls::text, tool_call_id, is_error, metadata::text,
		       model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at
		FROM run_messages
		WHERE org_id = $1 AND run_id = $2
		ORDER BY id ASC
	`, orgID, runID)
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

		if toolCallsStr.Valid && toolCallsStr.String != "" {
			_ = json.Unmarshal([]byte(toolCallsStr.String), &m.ToolCalls)
		}
		if metadataStr.Valid && metadataStr.String != "" {
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
	return tokenTotals(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	return tokenTotals(ctx, s.admin, orgID, runID)
}

func tokenTotals(ctx context.Context, q queryer, orgID, runID string) (*domain.TokenTotals, error) {
	row := q.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(model), ''),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0),
		       COUNT(*)
		FROM run_messages
		WHERE org_id = $1 AND run_id = $2 AND role = 'assistant'
	`, orgID, runID)

	var t domain.TokenTotals
	if err := row.Scan(&t.Model, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheCreationTokens, &t.NumTurns); err != nil {
		return nil, err
	}
	return &t, nil
}

// --- Yields ---

func (s *agentRunStore) InsertYieldRequest(ctx context.Context, orgID, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
	return insertYieldRequest(ctx, s.q, orgID, runID, req)
}

func (s *agentRunStore) InsertYieldRequestSystem(ctx context.Context, orgID, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
	return insertYieldRequest(ctx, s.admin, orgID, runID, req)
}

func insertYieldRequest(ctx context.Context, q queryer, orgID, runID string, req *domain.YieldRequest) (*domain.AgentMessage, error) {
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
	id, err := insertRunMessage(ctx, q, orgID, msg)
	if err != nil {
		return nil, err
	}
	msg.ID = int(id)
	return msg, nil
}

func (s *agentRunStore) InsertYieldResponse(ctx context.Context, orgID, runID string, resp *domain.YieldResponse, displayContent string) (*domain.AgentMessage, error) {
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
	var content sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT content FROM run_messages
		WHERE org_id = $1 AND run_id = $2 AND subtype = $3
		ORDER BY id DESC LIMIT 1
	`, orgID, runID, db.YieldRequestSubtype).Scan(&content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
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

// scanAgentRun fills r from a single-row QueryRow result. Sibling
// scanAgentRunRows handles the rows.Scan case. Both unpack the
// nullable columns through the same set of intermediates.
func scanAgentRun(row *sql.Row, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Status, &r.Model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &blueprintRunID, &blueprintStep,
		&r.MemoryMissing,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, completedAt, costUSD, durationMs, numTurns, blueprintStep, blueprintRunID)
	return nil
}

func scanAgentRunRows(rows *sql.Rows, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Status, &r.Model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &blueprintRunID, &blueprintStep,
		&r.MemoryMissing,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, completedAt, costUSD, durationMs, numTurns, blueprintStep, blueprintRunID)
	return nil
}

func finalizeAgentRun(r *domain.AgentRun, completedAt sql.NullTime, costUSD sql.NullFloat64, durationMs, numTurns, blueprintStep sql.NullInt64, blueprintRunID sql.NullString) {
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

// runScoped runs fn inside a rollback-safe scope:
//
//   - If s.q is *sql.DB (the standalone path — every store method
//     called outside Stores.Tx.WithTx), runScoped opens a fresh tx,
//     hands fn the *sql.Tx as a queryer, and commits on success or
//     rolls back via defer on any error including the sentinel.
//
//   - If s.q is *sql.Tx (composed inside an outer WithTx), runScoped
//     declares a SAVEPOINT, runs fn against the same tx, and either
//     RELEASEs on success or ROLLBACK-TO-SAVEPOINTs on errScopedRollback
//     — leaving the surrounding tx's other work intact.
//
// fn signals "roll back this scope but don't surface an error to the
// caller" by returning errScopedRollback. runScoped translates that
// into (rolledBack=true, nil); MarkTakenOver uses it to convert
// takeover-race-lost into (false, nil) without poisoning an outer tx.
//
// Other errors bubble up unchanged — runScoped rolls back the scope
// (savepoint or tx) and returns (false, err).
//
// Savepoint names are unique per call to avoid collisions if a
// future caller composes two AgentRunStore methods inside one
// outer tx.
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
		// Always roll back the savepoint on any error so partial work
		// doesn't leak into the outer tx. The RELEASE after ROLLBACK
		// TO is necessary in Postgres — the savepoint stays declared
		// otherwise (SQLite's parser tolerates either; uniform shape
		// keeps the helper simple).
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
				return true, nil // deferred Rollback unwinds the tx
			}
			return false, fnErr
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, errors.New("postgres agentrun: unexpected queryer type")
	}
}

// errScopedRollback is the sentinel fn returns when it wants
// runScoped to roll back the current scope (savepoint or tx) and
// surface (rolledBack=true, nil) to the caller. Used by
// MarkTakenOver to model race-loss as a non-error rollback.
var errScopedRollback = errors.New("agentrun: scoped rollback")

// scopedSavepointName generates a unique savepoint identifier per
// call. UnixNano + a process-local counter would be marginally safer
// against same-nanosecond collisions but the helper is only called
// inside a transaction, where SAVEPOINT names form a stack and
// declaring two with the same name shadows the outer — the unique
// suffix is defensive against logical collisions across nested
// composed calls within one tx, not against time-resolution
// collisions.
func scopedSavepointName() string {
	return fmt.Sprintf("agentrun_scope_%d", time.Now().UnixNano())
}

// nullIfEmpty is the small reusable helper many Postgres stores want
// — empty string → SQL NULL bind, non-empty passes through. Defined
// once per package; sibling stores that also need it import the same
// symbol. Currently agentrun.go is the first store to declare it on
// the Postgres side; if another store grows the same need we can
// lift this to a shared util file then.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
