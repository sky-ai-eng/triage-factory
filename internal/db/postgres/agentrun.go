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
	// a different team.
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
// The sentinel filter is transitional: D9 will rewire
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

// CreateIfNotFiredSystem is the event-path fenced insert. Same
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
	// unfenced run here would defeat its purpose.
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

func (s *agentRunStore) Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	return completeRun(ctx, s.q, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason, failureKind)
}

func (s *agentRunStore) CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	return completeRun(ctx, s.admin, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason, failureKind)
}

func completeRun(ctx context.Context, q queryer, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	// Token columns are SET to the absolute SUM over run_messages (the
	// streaming sink wrote every session's rows before this terminal
	// write), NOT accumulated like total_cost_usd — the SUM is already the
	// full total, so re-running Complete on a resume re-sets the same
	// correct value rather than doubling. The subqueries reuse the org/run
	// binds ($11/$12); org_id scopes run_messages for defense-in-depth
	// (and so the admin-pool BYPASSRLS path stays tenant-correct). TFAC-473.
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
		    outcome_reason = NULLIF($9, ''),
		    failure_kind = NULLIF($10, ''),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE org_id = $11 AND run_id = $12),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE org_id = $11 AND run_id = $12),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE org_id = $11 AND run_id = $12),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE org_id = $11 AND run_id = $12)
		WHERE org_id = $11 AND id = $12
	`, status, time.Now(), costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason, failureKind, orgID, runID)
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

func (s *agentRunStore) MarkOpen(ctx context.Context, orgID, runID string) (bool, error) {
	return markOpen(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) MarkOpenSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return markOpen(ctx, s.admin, orgID, runID)
}

func markOpen(ctx context.Context, q queryer, orgID, runID string) (bool, error) {
	// pending_approval stays in the exclusion list as a backward-compat guard:
	// runs no longer park in it, but a legacy row must not be re-opened from it.
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'open', parked_at = now()
		WHERE org_id = $1 AND id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'open')
	`, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkResuming(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) (bool, error) {
	return markResuming(ctx, s.q, orgID, runID, executorID, bootEpoch)
}

func (s *agentRunStore) MarkResumingSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) (bool, error) {
	return markResuming(ctx, s.admin, orgID, runID, executorID, bootEpoch)
}

func (s *agentRunStore) ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error) {
	// Resumable-state runs (open / completed+abort) grouped by their shared
	// snapshot key (org, blueprint_run_id); a key is reapable once its newest
	// resumable run last parked before the cutoff. The park timestamp is
	// COALESCE(parked_at, completed_at, started_at): parked_at for an open run
	// (re-stamped each park, so resumes don't age it), completed_at for the
	// completed+abort terminal, started_at a legacy fallback. Admin pool — the
	// retention sweep is a tenant-spanning system job with no JWT claims.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT org_id, blueprint_run_id
		FROM runs
		WHERE status = 'open'
		   OR (status = 'completed' AND outcome = 'abort')
		GROUP BY org_id, blueprint_run_id
		HAVING MAX(COALESCE(parked_at, completed_at, started_at)) < $1
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []domain.SnapshotReapKey
	for rows.Next() {
		var k domain.SnapshotReapKey
		if err := rows.Scan(&k.OrgID, &k.BlueprintRunID); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func markResuming(ctx context.Context, q queryer, orgID, runID, executorID string, bootEpoch int64) (bool, error) {
	// Wake any non-finish parked/terminal state: open, or an aborted run
	// (completed + outcome='abort'). Keyed on (status, outcome) so a finish run
	// (completed + outcome='finish') is excluded and a racing resume that already
	// moved the row loses this compare-and-swap. pending_approval is gone
	// Runs never park for approval.
	//
	// The ownership stamp rides the same statement: a parked row still
	// wears the identity of whichever instance last ran it, and flipping
	// it 'running' without re-stamping would leave the whole
	// rehydrate+spawn window attributed to that instance — whose next
	// boot (or the reaper) would sweep a run this process is live-resuming.
	// Empty executorID → NULL for both columns (un-wired test spawner).
	res, err := q.ExecContext(ctx, `
		UPDATE runs SET status = 'running', parked_at = NULL,
			executor_id = NULLIF($3, ''),
			boot_epoch  = CASE WHEN $3 = '' THEN NULL ELSE $4::bigint END
		WHERE org_id = $1 AND id = $2
		  AND (status = 'open'
		       OR (status = 'completed' AND outcome = 'abort'))
	`, orgID, runID, executorID, bootEpoch)
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

func (s *agentRunStore) SetExecutorSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) error {
	// Clearing the pointer (executorID == "") clears boot_epoch with it —
	// the two columns are always either both set or both NULL, never a
	// stale boot_epoch left behind for no owner.
	var epoch any
	if executorID != "" {
		epoch = bootEpoch
	}
	_, err := s.admin.ExecContext(ctx, `
		UPDATE runs SET executor_id = $1, boot_epoch = $2 WHERE org_id = $3 AND id = $4
	`, nullIfEmpty(executorID), epoch, orgID, runID)
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

func (s *agentRunStore) MarkFailedIfActive(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	return markFailedIfActive(ctx, s.q, orgID, runID, failureKind)
}

func (s *agentRunStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	return markFailedIfActive(ctx, s.admin, orgID, runID, failureKind)
}

func markFailedIfActive(ctx context.Context, q queryer, orgID, runID, failureKind string) (bool, error) {
	// 'open' is deliberately failable here (unlike 'pending_approval') — see
	// AgentRunStore.MarkFailedIfActive: a warm 'open' run has no durable
	// snapshot yet, so an infra error reaching failRun must terminate it.
	//
	// Refresh the denormalized token columns from the run_messages SUM, same as
	// completeRun — an infra-failed run still streamed (and paid for) messages,
	// so the cache must reflect them rather than strand at 0. The subqueries
	// reuse the org/run binds ($3/$4); org_id scopes run_messages for
	// defense-in-depth (and so the admin-pool BYPASSRLS path stays
	// tenant-correct). TFAC-473.
	res, err := q.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', completed_at = COALESCE(completed_at, $1),
		    failure_kind = NULLIF($2, ''),
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE org_id = $3 AND run_id = $4),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE org_id = $3 AND run_id = $4),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE org_id = $3 AND run_id = $4),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE org_id = $3 AND run_id = $4)
		WHERE org_id = $3 AND id = $4
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval')
	`, time.Now().UTC(), failureKind, orgID, runID)
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
			  AND status NOT IN ('completed','failed','cancelled','task_unsolvable','pending_approval')
		)
	`, orgID, taskID, excludeRunID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *agentRunStore) MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return markCancelledIfActive(ctx, s.q, orgID, runID, stopReason, summary)
}

func (s *agentRunStore) MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return markCancelledIfActive(ctx, s.admin, orgID, runID, stopReason, summary)
}

func markCancelledIfActive(ctx context.Context, q queryer, orgID, runID, stopReason, summary string) (bool, error) {
	now := time.Now()
	// Refresh the denormalized token columns from the run_messages SUM, same as
	// completeRun — a run cancelled while running/open still streamed (and paid
	// for) messages, so the cache must reflect them rather than strand at 0. The
	// subqueries reuse the org/run binds ($4/$5); org_id scopes run_messages for
	// defense-in-depth (and so the admin-pool BYPASSRLS path stays
	// tenant-correct). TFAC-473.
	res, err := q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled', completed_at = $1, stop_reason = $2, result_summary = $3,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE org_id = $4 AND run_id = $5),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE org_id = $4 AND run_id = $5),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE org_id = $4 AND run_id = $5),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE org_id = $4 AND run_id = $5)
		WHERE org_id = $4 AND id = $5
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval')
	`, now, stopReason, summary, orgID, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkDiscarded(ctx context.Context, orgID, runID, stopReason string) (bool, error) {
	now := time.Now()
	// No token roll-up here (unlike the other terminal writes): this acts only
	// on 'pending_approval' rows, which already rolled up at the
	// completed→pending_approval transition via completeRun — run_messages can't
	// grow after that, so the cache is already current (TFAC-473).
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
	COALESCE(r.failure_kind, ''),
	COALESCE(r.session_id, ''),
	COALESCE(r.actor_agent_id::text, ''),
	COALESCE(r.trigger_type, ''),
	COALESCE(r.creator_user_id::text, ''),
	r.team_id::text,
	COALESCE(r.executor_id, ''),
	r.blueprint_run_id, r.blueprint_step_index,
	r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_creation_tokens,
	(NULLIF(BTRIM(rm.agent_content, E' \t\n\r'), '') IS NULL) AS memory_missing,
	COALESCE(a.display_name, '') AS actor_agent_name
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
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
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
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
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

func (s *agentRunStore) ListForTasks(ctx context.Context, orgID string, taskIDs []string) ([]domain.AgentRun, error) {
	// task_id is a uuid column: a non-UUID id (these are client-supplied on the
	// batched run-list path) would fail Postgres parsing with 22P02 → 500
	// before the row filter runs, so drop invalid ids up front and treat them
	// as "no rows" — the read-method convention in uuid.go.
	taskIDs = filterValidUUIDs(taskIDs)
	if len(taskIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): rows are team-scoped exactly like ListForTask.
	// task_id is a uuid column, so the slice binds as a uuid[] literal
	// through one $N (pgUUIDArray), like artifactStore.ListByRuns — not a
	// raw []string. Same projection as ListForTask; the caller groups the
	// flat result by run.TaskID.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		WHERE r.org_id = $1 AND r.task_id = ANY($2)
		ORDER BY r.started_at DESC
	`, orgID, pgUUIDArray(taskIDs))
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
		                     'pending_approval')
	`, orgID, taskID).Scan(&count)
	return count > 0, err
}

// HasActiveAutoRunForEntity is the per-entity sibling of
// HasActiveForTask: any non-terminal trigger_type='event' run on any
// task that belongs to the entity. Manual delegations are excluded.
// Used by the router's per-entity firing gate.
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
		                       'pending_approval')
	`, orgID, entityID).Scan(&count)
	return count > 0, err
}

func (s *agentRunStore) ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.q, orgID, taskID)
}

func (s *agentRunStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return activeRunIDsForTask(ctx, s.admin, orgID, taskID)
}

func (s *agentRunStore) ActiveAutoRunIDForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	var id string
	err := s.admin.QueryRowContext(ctx, `
		SELECT r.id FROM runs r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND t.entity_id = $2
		  AND r.trigger_type = 'event'
		  AND r.status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                       'pending_approval')
		ORDER BY r.started_at DESC
		LIMIT 1
	`, orgID, entityID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func activeRunIDsForTask(ctx context.Context, q queryer, orgID, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND task_id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval')
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

func (s *agentRunStore) ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND team_id = $2
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval')
	`, orgID, teamID)
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

func (s *agentRunStore) ListParkedWorktreePaths(ctx context.Context, orgID string) ([]string, error) {
	return listParkedWorktreePaths(ctx, s.q, orgID)
}

func (s *agentRunStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
	return listParkedWorktreePaths(ctx, s.admin, orgID)
}

// listParkedWorktreePaths returns the worktree dirs the startup sweep must keep
// warm — parked `open` runs whose owning blueprint_run is still 'running'. A
// parked run under an already-terminal blueprint_run is NOT resumable (every
// resume path gates on cr.Status == running), so its worktree must NOT be
// preserved: preserving it would leave a checked-out branch on disk that the
// boot reconcile then orphans by cancelling the row, reviving the "refusing to
// fetch into a branch checked out in a worktree" loop.
func listParkedWorktreePaths(ctx context.Context, q queryer, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT r.worktree_path FROM runs r
		LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
		WHERE r.org_id = $1
		  AND r.status = 'open'
		  AND COALESCE(r.worktree_path, '') != ''
		  AND (br.id IS NULL OR br.status = 'running')
	`, orgID)
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

func (s *agentRunStore) EntitiesWithOpenRuns(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM runs r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		WHERE r.org_id = $1
		  AND r.status = 'open'
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

// LastAgentActivityAtSystem returns the created_at of the run's newest non-user
// message (the artifact-change ledger watermark, TFAC-493). Ordered by id DESC
// (the monotonic sequence) so the watermark is the genuinely last-inserted agent
// row. Admin pool: the resume path holds no JWT claims. ok=false when the run has
// no agent message yet.
func (s *agentRunStore) LastAgentActivityAtSystem(ctx context.Context, orgID, runID string) (time.Time, bool, error) {
	var at time.Time
	err := s.admin.QueryRowContext(ctx, `
		SELECT created_at FROM run_messages
		WHERE org_id = $1 AND run_id = $2 AND role <> 'user'
		ORDER BY id DESC LIMIT 1
	`, orgID, runID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
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

const pgMessageColumns = `id, run_id, role, content, subtype, tool_calls::text, tool_call_id, is_error, metadata::text,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at`

// scanAgentMessageRows drains a run_messages result set selecting
// pgMessageColumns into domain.AgentMessage values. Shared by the
// single-run Messages and the batched MessagesForRuns.
func scanAgentMessageRows(rows *sql.Rows) ([]domain.AgentMessage, error) {
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

func (s *agentRunStore) Messages(ctx context.Context, orgID, runID string) ([]domain.AgentMessage, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM run_messages
		WHERE org_id = $1 AND run_id = $2
		ORDER BY id ASC
	`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgentMessageRows(rows)
}

func (s *agentRunStore) MessagesForRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.AgentMessage, error) {
	// run_id is a uuid column; drop any non-UUID id (22P02 → 500 guard, same
	// read convention as ListForTasks). These ids are server-derived today, so
	// this is defense in depth against a future caller passing raw input.
	runIDs = filterValidUUIDs(runIDs)
	if len(runIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): run_id is a uuid column, so the slice binds
	// as a uuid[] literal through one $N (pgUUIDArray), mirroring
	// artifactStore.ListByRuns. ORDER BY (run_id, id) so the caller groups
	// by RunID with each run's messages in insertion order.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM run_messages
		WHERE org_id = $1 AND run_id = ANY($2)
		ORDER BY run_id ASC, id ASC
	`, orgID, pgUUIDArray(runIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgentMessageRows(rows)
}

func (s *agentRunStore) TokenTotals(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	return tokenTotals(ctx, s.q, orgID, runID)
}

func (s *agentRunStore) TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error) {
	return tokenTotals(ctx, s.admin, orgID, runID)
}

func (s *agentRunStore) BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (float64, error) {
	var cost sql.NullFloat64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_cost_usd), 0)
		FROM runs
		WHERE org_id = $1 AND blueprint_run_id = $2 AND id <> $3
	`, orgID, blueprintRunID, excludeRunID).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost.Float64, nil
}

func (s *agentRunStore) BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (int, error) {
	var ms sql.NullInt64
	err := s.admin.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(duration_ms), 0)
		FROM runs
		WHERE org_id = $1 AND blueprint_run_id = $2 AND id <> $3
	`, orgID, blueprintRunID, excludeRunID).Scan(&ms)
	if err != nil {
		return 0, err
	}
	return int(ms.Int64), nil
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

// --- Helpers ---

// scanAgentRun fills r from a single-row QueryRow result. Sibling
// scanAgentRunRows handles the rows.Scan case. Both unpack the
// nullable columns through the same set of intermediates.
func scanAgentRun(row *sql.Row, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString
	var failureKind string

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Status, &r.Model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &failureKind, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &r.TeamID, &r.ExecutorID, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	r.FailureKind = domain.RunFailureKind(failureKind)
	finalizeAgentRun(r, completedAt, costUSD, durationMs, numTurns, blueprintStep, blueprintRunID)
	return nil
}

func scanAgentRunRows(rows *sql.Rows, r *domain.AgentRun) error {
	var completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var blueprintRunID sql.NullString
	var failureKind string

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Status, &r.Model, &r.StartedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &r.StopReason, &r.WorktreePath,
		&r.ResultSummary, &r.Outcome, &r.OutcomeReason, &failureKind, &r.SessionID, &r.ActorAgentID, &r.TriggerType, &r.CreatorUserID, &r.TeamID, &r.ExecutorID, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	r.FailureKind = domain.RunFailureKind(failureKind)
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
