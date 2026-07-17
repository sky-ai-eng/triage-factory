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
// (SQLite is single-tenant; any non-LocalDefaultOrgID value is a
// confused caller).
//
// The constructor takes a single queryer (SQLite has one connection)
// rather than the (app, admin) pair the Postgres impl uses — the
// AgentRunStore was the first store to ship multi-pool, and the
// SQLite side never grew the second arg. The
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
	// exactly one team, so every run lands on it regardless of
	// run.TeamID (which the read paths surface back out, TFAC-458).
	// The schema requires a non-NULL team_id for visibility='team'
	// rows. Multi-mode derives the real team from the parent task in the
	// Postgres Create / EnqueueRun, never here.
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

// CreateIfNotFiredSystem is the event-path fenced insert. It
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
	// unfenced run here would defeat its purpose.
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

func (s *agentRunStore) Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Token columns are SET to the absolute SUM over run_messages (the
	// streaming sink wrote every session's messages before this terminal
	// write), NOT accumulated like total_cost_usd — the SUM is already the
	// full total, so re-running Complete on a resume re-sets the same
	// correct value rather than doubling. TFAC-473.
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
		    outcome_reason = ?,
		    failure_kind = ?,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE run_id = ?),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE run_id = ?),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE run_id = ?),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE run_id = ?)
		WHERE id = ?
	`, status, time.Now(), costUSD, durationMs, numTurns, stopReason, resultSummary,
		nullIfEmpty(outcome), nullIfEmpty(outcomeReason), nullIfEmpty(failureKind),
		runID, runID, runID, runID, runID)
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

func (s *agentRunStore) MarkOpen(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// pending_approval stays in the exclusion list as a backward-compat guard:
	// runs no longer park in it, but a legacy row must not be re-opened from it.
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'open', parked_at = ?
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval', 'open')
	`, time.Now().UTC(), runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *agentRunStore) MarkResuming(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// Wake any non-finish parked/terminal state: open, or an aborted run
	// (completed + outcome='abort'). Keyed on (status, outcome) so a finish run
	// (completed + outcome='finish') is excluded and a racing resume that already
	// moved the row loses this compare-and-swap. pending_approval is gone
	// Runs never park for approval.
	//
	// The ownership stamp rides the same statement — see the Postgres
	// twin's comment; at N=1 the practical effect is that a resumed row
	// always carries the CURRENT boot's epoch, so a same-boot reconcile
	// never mistakes it for a prior boot's orphan. Empty executorID →
	// NULL for both columns.
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'running', parked_at = NULL,
			executor_id = NULLIF(?, ''),
			boot_epoch  = CASE WHEN ? = '' THEN NULL ELSE ? END
		WHERE id = ?
		  AND (status = 'open'
		       OR (status = 'completed' AND outcome = 'abort'))
	`, executorID, executorID, bootEpoch, runID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkQueuedForResume is resume-by-enqueue's status flip (TFAC-585): see
// the interface doc comment / the Postgres twin.
func (s *agentRunStore) MarkQueuedForResume(ctx context.Context, orgID, runID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'queued', queued_at = CURRENT_TIMESTAMP,
		                parked_at = NULL, claimed_at = NULL,
		                executor_id = NULL, boot_epoch = NULL, cred_pubkey = NULL,
		                preferred_executor_id = NULL
		WHERE id = ?
		  AND (status = 'open'
		       OR (status = 'completed' AND outcome = 'abort'))
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

func (s *agentRunStore) SetExecutorSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// Clearing the pointer (executorID == "") clears boot_epoch with it —
	// the two columns are always either both set or both NULL.
	var epoch any
	if executorID != "" {
		epoch = bootEpoch
	}
	_, err := s.q.ExecContext(ctx, `UPDATE runs SET executor_id = ?, boot_epoch = ? WHERE id = ?`, nullIfEmpty(executorID), epoch, runID)
	return err
}

func (s *agentRunStore) MarkFailedIfActive(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// 'open' is deliberately failable here (unlike 'pending_approval') — see
	// AgentRunStore.MarkFailedIfActive: a warm 'open' run has no durable
	// snapshot yet, so an infra error reaching failRun must terminate it.
	//
	// Refresh the denormalized token columns from the run_messages SUM, same as
	// Complete — an infra-failed run still streamed (and paid for) messages, so
	// the cache must reflect them rather than strand at 0 (TFAC-473).
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs SET status = 'failed', completed_at = COALESCE(completed_at, ?),
		    failure_kind = ?,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE run_id = ?),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE run_id = ?),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE run_id = ?),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE run_id = ?)
		WHERE id = ?
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval')
	`, time.Now().UTC(), nullIfEmpty(failureKind), runID, runID, runID, runID, runID)
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
			  AND status NOT IN ('completed','failed','cancelled','task_unsolvable','pending_approval')
		)
	`, taskID, excludeRunID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *agentRunStore) MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	now := time.Now()
	// Refresh the denormalized token columns from the run_messages SUM, same as
	// Complete — a run cancelled while running/open still streamed (and paid
	// for) messages, so the cache must reflect them rather than strand at 0
	// (TFAC-473).
	res, err := s.q.ExecContext(ctx, `
		UPDATE runs
		SET status = 'cancelled', completed_at = ?, stop_reason = ?, result_summary = ?,
		    input_tokens          = (SELECT COALESCE(SUM(input_tokens), 0)          FROM run_messages WHERE run_id = ?),
		    output_tokens         = (SELECT COALESCE(SUM(output_tokens), 0)         FROM run_messages WHERE run_id = ?),
		    cache_read_tokens     = (SELECT COALESCE(SUM(cache_read_tokens), 0)     FROM run_messages WHERE run_id = ?),
		    cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM run_messages WHERE run_id = ?)
		WHERE id = ?
		  AND status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                     'pending_approval')
	`, now, stopReason, summary, runID, runID, runID, runID, runID)
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
	// No token roll-up here (unlike the other terminal writes): this acts only
	// on 'pending_approval' rows, which already rolled up at the
	// completed→pending_approval transition via Complete — run_messages can't
	// grow after that, so the cache is already current (TFAC-473).
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
	r.id, r.task_id, r.status, r.model, r.started_at, r.queued_at, r.claimed_at, r.completed_at,
	r.total_cost_usd, r.duration_ms, r.num_turns, r.stop_reason, r.worktree_path,
	r.result_summary, r.outcome, r.outcome_reason, r.failure_kind, r.session_id, r.actor_agent_id,
	COALESCE(r.trigger_type, ''),
	r.creator_user_id,
	r.team_id,
	r.executor_id,
	r.blueprint_run_id, r.blueprint_step_index,
	r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_creation_tokens,
	(NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing,
	COALESCE(a.display_name, '') AS actor_agent_name
`

func (s *agentRunStore) Get(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteRunColumns+`
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		LEFT JOIN agents a ON a.id = r.actor_agent_id
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
		LEFT JOIN agents a ON a.id = r.actor_agent_id
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

func (s *agentRunStore) ListForTasks(ctx context.Context, orgID string, taskIDs []string) ([]domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(taskIDs) == 0 {
		return nil, nil
	}
	// ?-placeholder IN list (SQLite has no array bind), mirroring
	// artifactStore.ListByRuns, chunked to stay inside SQLite's variable
	// limit (chunkIDs). Each task_id falls in exactly one chunk, so a task's
	// runs are returned contiguously and newest-first among themselves
	// (order ACROSS tasks is chunk order, not started_at) — which is all the
	// caller, grouping by run.TaskID, relies on. Same projection as ListForTask.
	var runs []domain.AgentRun
	for _, chunk := range chunkIDs(taskIDs) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT `+sqliteRunColumns+`
			FROM runs r
			LEFT JOIN run_memory rm ON rm.run_id = r.id
			LEFT JOIN agents a ON a.id = r.actor_agent_id
			WHERE r.task_id IN (`+strings.Join(placeholders, ", ")+`)
			ORDER BY r.started_at DESC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r domain.AgentRun
			if err := scanAgentRunRows(rows, &r); err != nil {
				rows.Close()
				return nil, err
			}
			runs = append(runs, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return runs, nil
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
		                                      'task_unsolvable', 'pending_approval')
	`, taskID).Scan(&count)
	return count > 0, err
}

// HasActiveAutoRunForEntity is the per-entity sibling of
// HasActiveForTask: any non-terminal trigger_type='event' run on any
// task that belongs to the entity. Manual delegations are excluded.
// Used by the router's per-entity firing gate.
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
		                       'pending_approval')
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
		                                      'task_unsolvable', 'pending_approval')
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

func (s *agentRunStore) HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error) {
	return s.HasActiveAutoRunForEntity(ctx, orgID, entityID)
}

func (s *agentRunStore) ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return s.ActiveIDsForTask(ctx, orgID, taskID)
}

func (s *agentRunStore) ActiveAutoRunIDForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var id string
	err := s.q.QueryRowContext(ctx, `
		SELECT r.id FROM runs r
		JOIN tasks t ON t.id = r.task_id
		WHERE t.entity_id = ?
		  AND r.trigger_type = 'event'
		  AND r.status NOT IN ('completed', 'failed', 'cancelled', 'task_unsolvable',
		                       'pending_approval')
		ORDER BY r.started_at DESC
		LIMIT 1
	`, entityID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *agentRunStore) ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE team_id = ? AND status NOT IN ('completed', 'failed', 'cancelled',
		                                     'task_unsolvable', 'pending_approval')
	`, teamID)
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

func (s *agentRunStore) CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error {
	return s.Complete(ctx, orgID, runID, status, costUSD, durationMs, numTurns, stopReason, resultSummary, outcome, outcomeReason, failureKind)
}

func (s *agentRunStore) AddPartialTotalsSystem(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error {
	return s.AddPartialTotals(ctx, orgID, runID, costUSD, durationMs, numTurns)
}

func (s *agentRunStore) MarkOpenSystem(ctx context.Context, orgID, runID string) (bool, error) {
	return s.MarkOpen(ctx, orgID, runID)
}

func (s *agentRunStore) MarkResumingSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) (bool, error) {
	return s.MarkResuming(ctx, orgID, runID, executorID, bootEpoch)
}

func (s *agentRunStore) ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error) {
	// Resumable-state runs (open / completed+abort) grouped by their shared
	// snapshot key (org, blueprint_run_id); a key is reapable once its newest
	// resumable run last parked before the cutoff. The park timestamp is
	// COALESCE(parked_at, completed_at, started_at): parked_at for an open run
	// (re-stamped each park, so resumes don't age it), completed_at for the
	// completed+abort terminal, started_at a legacy fallback. datetime()
	// normalizes the mixed on-disk timestamp formats (CURRENT_TIMESTAMP text vs
	// Go-bound values) so the MAX is consistent; the cutoff binds as a canonical
	// UTC string.
	rows, err := s.q.QueryContext(ctx, `
		SELECT org_id, blueprint_run_id
		FROM runs
		WHERE status = 'open'
		   OR (status = 'completed' AND outcome = 'abort')
		GROUP BY org_id, blueprint_run_id
		HAVING MAX(datetime(COALESCE(parked_at, completed_at, started_at))) < datetime(?)
	`, cutoff.UTC().Format("2006-01-02 15:04:05"))
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

func (s *agentRunStore) MarkFailedIfActiveSystem(ctx context.Context, orgID, runID, failureKind string) (bool, error) {
	return s.MarkFailedIfActive(ctx, orgID, runID, failureKind)
}

func (s *agentRunStore) MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error) {
	return s.MarkCancelledIfActive(ctx, orgID, runID, stopReason, summary)
}

func (s *agentRunStore) InsertMessageSystem(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error) {
	return s.InsertMessage(ctx, orgID, msg)
}

// LastAgentActivityAtSystem returns the created_at of the run's newest non-user
// message (the artifact-change ledger watermark, TFAC-493). Ordered by id DESC —
// the monotonic insertion order — rather than MAX(created_at), so the watermark
// is the genuinely last-inserted agent row and never trips over mixed timestamp
// text formats in a lexical MAX. ok=false when the run has no agent message yet.
//
// assertLocalOrg above is SQLite's hard single-tenant gate; org_id is also kept
// in the WHERE to mirror the Postgres twin (defense-in-depth, and resilience if a
// multi-org SQLite path ever lands).
func (s *agentRunStore) LastAgentActivityAtSystem(ctx context.Context, orgID, runID string) (time.Time, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return time.Time{}, false, err
	}
	var at time.Time
	err := s.q.QueryRowContext(ctx, `
		SELECT created_at FROM run_messages
		WHERE org_id = ? AND run_id = ? AND role <> 'user'
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

func (s *agentRunStore) ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error) {
	return s.ListParkedWorktreePaths(ctx, orgID)
}

// ListParkedWorktreePaths returns the worktree dirs the startup sweep must keep
// warm — parked `open` runs whose owning blueprint_run is still 'running'. A
// parked run under an already-terminal blueprint_run is NOT resumable (every
// resume path gates on cr.Status == running), so its worktree must NOT be
// preserved: preserving it would leave a checked-out branch on disk that the
// boot reconcile then orphans by cancelling the row, reviving the "refusing to
// fetch into a branch checked out in a worktree" loop.
func (s *agentRunStore) ListParkedWorktreePaths(ctx context.Context, orgID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT r.worktree_path FROM runs r
		 LEFT JOIN blueprint_runs br ON br.id = r.blueprint_run_id
		 WHERE r.status = 'open'
		   AND COALESCE(r.worktree_path, '') != ''
		   AND (br.id IS NULL OR br.status = 'running')`)
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
		WHERE r.status = 'open'
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

const sqliteMessageColumns = `id, run_id, role, content, subtype, tool_calls, tool_call_id, is_error, metadata,
	model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, created_at`

// scanAgentMessageRows drains a run_messages result set selecting
// sqliteMessageColumns into domain.AgentMessage values. Shared by the
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

func (s *agentRunStore) Messages(ctx context.Context, orgID, runID string) ([]domain.AgentMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteMessageColumns+`
		FROM run_messages WHERE run_id = ? ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgentMessageRows(rows)
}

func (s *agentRunStore) MessagesForRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.AgentMessage, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	// ?-placeholder IN list (SQLite has no array bind), mirroring
	// artifactStore.ListByRuns, chunked to stay inside SQLite's variable
	// limit (chunkIDs). A run_id falls in exactly one chunk, so ORDER BY
	// (run_id, id) keeps each run's messages contiguous and in insertion
	// order within the merged slice; the caller groups by RunID.
	var messages []domain.AgentMessage
	for _, chunk := range chunkIDs(runIDs) {
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT `+sqliteMessageColumns+`
			FROM run_messages
			WHERE run_id IN (`+strings.Join(placeholders, ", ")+`)
			ORDER BY run_id ASC, id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		batch, err := scanAgentMessageRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		messages = append(messages, batch...)
	}
	return messages, nil
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

func (s *agentRunStore) BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (float64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var cost sql.NullFloat64
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_cost_usd), 0)
		FROM runs
		WHERE blueprint_run_id = ? AND id <> ?
	`, blueprintRunID, excludeRunID).Scan(&cost)
	if err != nil {
		return 0, err
	}
	return cost.Float64, nil
}

func (s *agentRunStore) BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var ms sql.NullInt64
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(duration_ms), 0)
		FROM runs
		WHERE blueprint_run_id = ? AND id <> ?
	`, blueprintRunID, excludeRunID).Scan(&ms)
	if err != nil {
		return 0, err
	}
	return int(ms.Int64), nil
}

// --- Helpers ---

func scanAgentRun(row *sql.Row, r *domain.AgentRun) error {
	var queuedAt, claimedAt, completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := row.Scan(
		&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &failureKind, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &r.TeamID, &executorID, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
	return nil
}

func scanAgentRunRows(rows *sql.Rows, r *domain.AgentRun) error {
	var queuedAt, claimedAt, completedAt sql.NullTime
	var costUSD sql.NullFloat64
	var durationMs, numTurns, blueprintStep sql.NullInt64
	var stopReason, worktreePath, model, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, creatorUserID, executorID, blueprintRunID sql.NullString

	if err := rows.Scan(
		&r.ID, &r.TaskID, &r.Status, &model, &r.StartedAt, &queuedAt, &claimedAt, &completedAt,
		&costUSD, &durationMs, &numTurns, &stopReason, &worktreePath,
		&resultSummary, &outcome, &outcomeReason, &failureKind, &sessionID, &actorAgentID, &r.TriggerType, &creatorUserID, &r.TeamID, &executorID, &blueprintRunID, &blueprintStep,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
		&r.MemoryMissing, &r.ActorAgentName,
	); err != nil {
		return err
	}
	finalizeAgentRun(r, queuedAt, claimedAt, completedAt, costUSD, durationMs, numTurns, blueprintStep,
		model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID)
	return nil
}

func finalizeAgentRun(r *domain.AgentRun, queuedAt, claimedAt, completedAt sql.NullTime, costUSD sql.NullFloat64,
	durationMs, numTurns, blueprintStep sql.NullInt64,
	model, stopReason, worktreePath, resultSummary, outcome, outcomeReason, failureKind, sessionID, actorAgentID, blueprintRunID, creatorUserID, executorID sql.NullString) {
	r.Model = model.String
	r.StopReason = stopReason.String
	r.WorktreePath = worktreePath.String
	r.ResultSummary = resultSummary.String
	r.Outcome = outcome.String
	r.OutcomeReason = outcomeReason.String
	r.FailureKind = domain.RunFailureKind(failureKind.String)
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
	if queuedAt.Valid {
		r.QueuedAt = &queuedAt.Time
	}
	if claimedAt.Valid {
		r.ClaimedAt = &claimedAt.Time
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
