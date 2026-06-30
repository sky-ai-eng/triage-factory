package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// taskStore is the SQLite impl of db.TaskStore. SQL bodies are moved
// verbatim from the pre-D2 internal/db/tasks.go; the only behavioral
// change is the orgID assertion at each method entry. Local mode is
// single-tenant by design — the orgID column on the SQLite tasks row
// defaults to LocalDefaultOrgID so we don't need to thread it through
// every UPDATE/INSERT, but rejecting an unexpected orgID at the entry
// point is the safety net for a caller that's confused about which
// mode it's in.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl (SKY-297), but SQLite has one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants are thin wrappers around the non-System methods.
type taskStore struct{ q queryer }

func newTaskStore(q, _ queryer) db.TaskStore { return &taskStore{q: q} }

var _ db.TaskStore = (*taskStore)(nil)

// --- Lookup ---

func (s *taskStore) Get(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var t domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.id = ?
	`, taskID), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// sqliteTaskTeamFilter mirrors pgTaskTeamFilter: it narrows a tasks
// query (alias t) to a *set* of teams — owning team_id OR a task_teams
// visibility row — appending the ids twice (SQLite binds ? positionally,
// so the two IN-lists each need their own copy). Empty teamIDs is a
// no-op. The fragment is always emitted last in the WHERE (before ORDER
// BY), so appending the args keeps them in placeholder order. Local mode
// is N=1 (one team) so this is effectively inert, but it keeps the SQLite
// store conformant with the interface contract.
func sqliteTaskTeamFilter(teamIDs []string, args []any) (string, []any) {
	if len(teamIDs) == 0 {
		return "", args
	}
	ph := strings.TrimRight(strings.Repeat("?, ", len(teamIDs)), ", ")
	for _, id := range teamIDs { // first IN (t.team_id)
		args = append(args, id)
	}
	for _, id := range teamIDs { // second IN (task_teams)
		args = append(args, id)
	}
	return fmt.Sprintf(" AND (t.team_id IN (%s) OR EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tt.team_id IN (%s)))", ph, ph), args
}

func (s *taskStore) Queued(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// SKY-261 B+ derived filter: queue = status='queued' + both claim
	// cols NULL + not future-snoozed.
	teamClause, args := sqliteTaskTeamFilter(teamIDs, nil)
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = 1 AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.status = 'queued'
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL
			AND (t.snooze_until IS NULL OR t.snooze_until <= datetime('now'))`+teamClause+`
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) QueuedIncludingSnoozed(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// SKY-330: drops the snooze-window filter so the Board's "show
	// snoozed" toggle surfaces deferred entries. Status='queued' +
	// status='snoozed' both qualify; SKY-261 enforces snoozed↔unclaimed
	// so the claim guards are still safe to apply.
	//
	// Ordering puts snoozed rows at the tail (the "wakes Mar 5"
	// badge cluster) regardless of priority so a high-priority but
	// deferred entry doesn't jump above live queued work. SQLite
	// treats the boolean expression as 0/1 — false (live) sorts
	// before true (snoozed).
	teamClause, args := sqliteTaskTeamFilter(teamIDs, nil)
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = 1 AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.status IN ('queued', 'snoozed')
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL`+teamClause+`
		ORDER BY (t.status = 'snoozed') ASC,
		         COALESCE(tr.sort_order, 999) ASC,
		         COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) ByStatus(ctx context.Context, orgID, status string, teamIDs []string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// SKY-261 B+: 'claimed' and 'delegated' aren't real lifecycle
	// values — they're derived filters on the claim columns.
	// SKY-330 added in_progress + in_review as first-class statuses,
	// so the "claimed" derivation now means specifically "any claim
	// (user or bot) at status='queued'." Status='queued' avoids
	// double-rendering once a task advances to in_progress / in_review
	// (those have their own columns). Including bot claims here
	// surfaces the brief window between delegate-stamp and the run's
	// first non-initializing status — plus delegate-spawn-failure
	// rows that stay claimed-queued indefinitely until retry.
	switch status {
	case "claimed":
		teamClause, args := sqliteTaskTeamFilter(teamIDs, nil)
		return queryTasksCtx(ctx, s.q, `
			SELECT `+sqliteTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE (t.claimed_by_user_id IS NOT NULL OR t.claimed_by_agent_id IS NOT NULL)
				AND t.status = 'queued'`+teamClause+`
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	case "delegated":
		teamClause, args := sqliteTaskTeamFilter(teamIDs, nil)
		return queryTasksCtx(ctx, s.q, `
			SELECT `+sqliteTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.claimed_by_agent_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')`+teamClause+`
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	case "done", "dismissed":
		// SKY-330: cap the Done column at the last 7 days so the
		// board doesn't accumulate an unbounded history. closed_at
		// is now populated by every close path — Close(),
		// RecordSwipe (complete/dismiss), spawner auto-close — so
		// requiring it here means rows that bypass those paths
		// surface as bugs (column empty) rather than accumulating
		// silently. Legacy pre-330 rows with NULL closed_at fall
		// out of the cap; they're old enough to be excluded anyway.
		teamClause, args := sqliteTaskTeamFilter(teamIDs, []any{status})
		return queryTasksCtx(ctx, s.q, `
			SELECT `+sqliteTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.status = ?
				AND t.closed_at IS NOT NULL
				AND t.closed_at >= datetime('now', '-7 days')`+teamClause+`
			ORDER BY t.closed_at DESC, COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	}
	teamClause, args := sqliteTaskTeamFilter(teamIDs, []any{status})
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = ?`+teamClause+`
		ORDER BY COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) FindActiveByEntityAndType(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID, eventType)
}

// FindActiveByEntityAndTypeSystem mirrors FindActiveByEntityAndType.
// SKY-297: the tracker consumes this through the admin pool in
// Postgres; SQLite has one connection, so this delegates straight
// through with the same assertLocalOrg gate.
func (s *taskStore) FindActiveByEntityAndTypeSystem(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	return s.FindActiveByEntityAndType(ctx, orgID, entityID, eventType)
}

// --- Admin-pool variants ---
//
// All `...System` methods below delegate straight through to their
// non-System counterparts. SQLite has one connection so the pool
// distinction doesn't exist; the wrappers are kept for signature
// parity with Postgres. The router consumes these from its eventbus
// subscriber goroutine.

func (s *taskStore) GetSystem(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	return s.Get(ctx, orgID, taskID)
}

func (s *taskStore) FindActiveByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	return s.FindActiveByEntity(ctx, orgID, entityID)
}

func (s *taskStore) FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	return s.FindOrCreateAt(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
}

func (s *taskStore) SetVisibilityTeamsSystem(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	return s.SetVisibilityTeams(ctx, orgID, taskID, teamIDs)
}

func (s *taskStore) VisibilityTeamsSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return s.VisibilityTeams(ctx, orgID, taskID)
}

func (s *taskStore) SetOwnerTeamSystem(ctx context.Context, orgID, taskID, teamID string) error {
	return s.SetOwnerTeam(ctx, orgID, taskID, teamID)
}

func (s *taskStore) BumpSystem(ctx context.Context, orgID, taskID, eventID string) error {
	return s.Bump(ctx, orgID, taskID, eventID)
}

func (s *taskStore) CloseSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error {
	return s.Close(ctx, orgID, taskID, closeReason, closeEventType)
}

func (s *taskStore) SetStatusSystem(ctx context.Context, orgID, taskID, status string) error {
	return s.SetStatus(ctx, orgID, taskID, status)
}

func (s *taskStore) RecordEventSystem(ctx context.Context, orgID, taskID, eventID, kind string) error {
	return s.RecordEvent(ctx, orgID, taskID, eventID, kind)
}

func (s *taskStore) CountConsecutiveFailedRunsSystem(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	return s.CountConsecutiveFailedRuns(ctx, orgID, entityID, promptID)
}

func (s *taskStore) StampAgentClaimIfUnclaimedSystem(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	return s.StampAgentClaimIfUnclaimed(ctx, orgID, taskID, agentID, actingTeamID)
}

func (s *taskStore) OwnerTeamForLatestTaskInTypesSystem(ctx context.Context, orgID, entityID string, eventTypes []string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if len(eventTypes) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(eventTypes))
	args := make([]any, 0, len(eventTypes)+1)
	args = append(args, entityID)
	for i, et := range eventTypes {
		placeholders[i] = "?"
		args = append(args, et)
	}
	var teamID string
	err := s.q.QueryRowContext(ctx, `
		SELECT team_id
		FROM tasks
		WHERE entity_id = ?
		  AND team_id IS NOT NULL
		  AND event_type IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY created_at DESC
		LIMIT 1
	`, args...).Scan(&teamID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return teamID, nil
}

func (s *taskStore) FindActiveByEntity(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return queryTasksCtx(ctx, s.q, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.status NOT IN ('done', 'dismissed')
	`, entityID)
}

// listActiveRefsChunkSize is the chunk applied to the IN clause when
// fanning out across many entities, kept conservatively below SQLite's
// historical bound-variable limit. Same shape ListRecentEventsByEntity
// uses.
const listActiveRefsChunkSize = 500

func (s *taskStore) ListActiveRefsForEntities(ctx context.Context, orgID string, entityIDs []string, _ []string) ([]domain.PendingTaskRef, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// teamIDs ignored: N=1 local mode has one team, so the factory's team
	// filter has nothing to narrow here (the frontend never renders it
	// below 2 teams). Mirrors the Queued/Entities asymmetry.
	if len(entityIDs) == 0 {
		return nil, nil
	}
	out := make([]domain.PendingTaskRef, 0, len(entityIDs))
	for start := 0; start < len(entityIDs); start += listActiveRefsChunkSize {
		end := start + listActiveRefsChunkSize
		if end > len(entityIDs) {
			end = len(entityIDs)
		}
		chunk := entityIDs[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT id, entity_id, event_type, dedup_key
			FROM tasks
			WHERE entity_id IN (`+strings.Join(placeholders, ",")+`)
				AND status NOT IN ('done', 'dismissed')
			ORDER BY entity_id, event_type, created_at DESC, rowid DESC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref domain.PendingTaskRef
			if err := rows.Scan(&ref.ID, &ref.EntityID, &ref.EventType, &ref.DedupKey); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *taskStore) EntityIDsWithActiveTasks(ctx context.Context, orgID, source string) (map[string]struct{}, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE e.source = ? AND t.status NOT IN ('done', 'dismissed')
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// --- Lifecycle ---

func (s *taskStore) FindOrCreate(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64) (*domain.Task, bool, error) {
	return s.FindOrCreateAt(ctx, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, time.Now())
}

func (s *taskStore) FindOrCreateAt(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, false, err
	}
	// teamID is the owning/attributed team stamped on a new row. Empty is
	// allowed and means "unresolved owner" — author-centric routing couldn't
	// pick a single team — and is inserted as NULL. A NULL-team task is still
	// visible via task_teams and gates auto-delegation off; it consolidates
	// to one team on the first human claim.
	var teamBind any
	if teamID != "" {
		teamBind = teamID
	}
	// Try to find an existing active task first. Identity is
	// (entity, event_type, dedup_key) — team is not part of it, so a
	// situation already tasked by another team's rule returns that
	// task here rather than spawning a duplicate.
	var existing domain.Task
	err := scanTaskFromRow(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, entityID, eventType, dedupKey), &existing)
	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// Create new task. The partial unique index on
	// (entity_id, event_type, dedup_key) WHERE status NOT IN
	// ('done', 'dismissed') is the race backstop: a concurrent
	// goroutine that races past the SELECT will get rejected on
	// INSERT, and we re-read to return the winner's row.
	id := uuid.New().String()
	// team_id is the owning/attributed team; 'team' is the canonical
	// visibility. The broader visibility set is written separately via
	// SetVisibilityTeams.
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility)
		VALUES (?, ?, ?, ?, ?, 'queued', ?, 'pending', ?, ?, 'team')
	`, id, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt, teamBind)
	if err != nil {
		var raced domain.Task
		err2 := scanTaskFromRow(s.q.QueryRowContext(ctx, `
			SELECT `+sqliteTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id
			WHERE t.entity_id = ? AND t.event_type = ? AND t.dedup_key = ?
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, entityID, eventType, dedupKey), &raced)
		if err2 == nil {
			return &raced, false, nil
		}
		return nil, false, err
	}

	task, err := s.Get(ctx, orgID, id)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

func (s *taskStore) Bump(ctx context.Context, orgID, taskID, eventID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE id = ?
	`, taskID)
	return err
}

func (s *taskStore) Close(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	now := time.Now()
	var cet *string
	if closeEventType != "" {
		cet = &closeEventType
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = ?, close_event_type = ?,
		                 closed_at = ?
		WHERE id = ? AND status NOT IN ('done', 'dismissed')
	`, closeReason, cet, now, taskID)
	return err
}

func (s *taskStore) SetStatus(ctx context.Context, orgID, taskID, status string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, status, taskID)
	return err
}

func (s *taskStore) AdvanceStatusForUser(ctx context.Context, orgID, taskID, userID, newStatus string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if newStatus != "in_progress" && newStatus != "in_review" {
		return false, nil
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET status = ?
		 WHERE id = ?
		   AND claimed_by_user_id = ?
		   AND status IN ('queued', 'in_progress', 'in_review')
	`, newStatus, taskID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) RecordEvent(ctx context.Context, orgID, taskID, eventID, kind string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO task_events (task_id, event_id, kind, created_at)
		VALUES (?, ?, ?, ?)
	`, taskID, eventID, kind, time.Now())
	return err
}

func (s *taskStore) SetVisibilityTeams(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, teamID := range teamIDs {
		if teamID == "" {
			continue
		}
		if _, err := s.q.ExecContext(ctx, `
			INSERT OR IGNORE INTO task_teams (task_id, team_id) VALUES (?, ?)
		`, taskID, teamID); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskStore) VisibilityTeams(ctx context.Context, orgID, taskID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT team_id FROM task_teams WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// sqliteActingTeamExpr derives the owning team for a user-initiated
// claim: a team in the task's visibility set (task_teams) that the
// claimer belongs to. It prefers the current owner team when the
// claimer is a member of it — honoring "keep the owner when the choice
// is ambiguous" — and otherwise picks the lowest-id qualifying team so
// the result is deterministic. The existing owner team_id is kept only
// when the intersection is empty. The result is always a team the
// claimer belongs to, which the RLS update check requires. Bind order
// is (taskID, userID).
const sqliteActingTeamExpr = `COALESCE(
		(SELECT tt.team_id
		   FROM task_teams tt
		   JOIN memberships m ON m.team_id = tt.team_id
		  WHERE tt.task_id = ? AND m.user_id = ?
		  ORDER BY (tt.team_id = tasks.team_id) DESC, tt.team_id ASC
		  LIMIT 1),
		team_id)`

func (s *taskStore) SetOwnerTeam(ctx context.Context, orgID, taskID, teamID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if teamID == "" {
		return nil
	}
	_, err := s.q.ExecContext(ctx, `UPDATE tasks SET team_id = ? WHERE id = ?`, teamID, taskID)
	return err
}

// --- Claim mutations ---

func (s *taskStore) SetClaimedByAgent(ctx context.Context, orgID, taskID, agentID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var claimedByAgentID any = agentID
	if agentID == "" {
		claimedByAgentID = nil
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL
		 WHERE id = ?
	`, claimedByAgentID, taskID)
	return err
}

func (s *taskStore) SetClaimedByUser(ctx context.Context, orgID, taskID, userID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	var claimedByUserID any = userID
	if userID == "" {
		// Empty string is the domain's NULL convention. Passing it raw
		// would violate the users(id) FK on the next read.
		claimedByUserID = nil
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL
		 WHERE id = ?
	`, claimedByUserID, taskID)
	return err
}

func (s *taskStore) StampAgentClaimIfUnclaimed(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if agentID == "" {
		return false, fmt.Errorf("StampAgentClaimIfUnclaimed: empty agentID")
	}
	// actingTeamID is the firing trigger's team; on a successful claim
	// it consolidates the card to that owning team. Empty leaves
	// team_id unchanged.
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       team_id = COALESCE(NULLIF(?, ''), team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND claimed_by_user_id IS NULL
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, actingTeamID, taskID, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) HandoffAgentClaim(ctx context.Context, orgID, taskID, agentID, userID string) (db.HandoffResult, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.HandoffRefused, err
	}
	if agentID == "" {
		return db.HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty agentID")
	}
	if userID == "" {
		return db.HandoffRefused, fmt.Errorf("HandoffAgentClaim: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = ?,
		       claimed_by_user_id  = NULL,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND (claimed_by_user_id  IS NULL OR claimed_by_user_id  = ?)
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != ?)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, taskID, userID, taskID, userID, agentID)
	if err != nil {
		return db.HandoffRefused, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return db.HandoffRefused, err
	}
	if n > 0 {
		return db.HandoffChanged, nil
	}
	// 0 rows — re-read to figure out which guard tripped. Terminal-status
	// check takes precedence over the no-op check so a sticky-bot-claim
	// on a closed task doesn't fall through to HandoffNoOp and let the
	// caller proceed past a row that mustn't be reopened.
	var curUser, curAgent sql.NullString
	var curStatus string
	err = s.q.QueryRowContext(ctx,
		`SELECT claimed_by_user_id, claimed_by_agent_id, status FROM tasks WHERE id = ?`,
		taskID,
	).Scan(&curUser, &curAgent, &curStatus)
	if err == sql.ErrNoRows {
		return db.HandoffRefused, nil
	}
	if err != nil {
		return db.HandoffRefused, err
	}
	if curStatus == "done" || curStatus == "dismissed" {
		return db.HandoffRefused, nil
	}
	if curAgent.Valid && curAgent.String == agentID {
		return db.HandoffNoOp, nil
	}
	return db.HandoffRefused, nil
}

func (s *taskStore) ResolveClaimTeam(ctx context.Context, orgID, taskID, userID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// N=1: the local user isn't enrolled via memberships, so the
	// visibility-set subquery is empty and this collapses to the task's
	// current team_id — the sole local team in practice. Mirrors the
	// Postgres derivation shape for interface parity.
	var team string
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(
		         (SELECT tt.team_id
		            FROM task_teams tt
		            JOIN memberships m ON m.team_id = tt.team_id
		           WHERE tt.task_id = ? AND m.user_id = ?
		           ORDER BY (tt.team_id = t.team_id) DESC, tt.team_id ASC LIMIT 1),
		         t.team_id)
		  FROM tasks t
		 WHERE t.id = ?
	`, taskID, userID, taskID).Scan(&team)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve claim team: %w", err)
	}
	return team, nil
}

func (s *taskStore) TakeoverClaimFromAgent(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if userID == "" {
		return false, fmt.Errorf("TakeoverClaimFromAgent: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = ?,
		       claimed_by_agent_id = NULL,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND claimed_by_agent_id IS NOT NULL
		   AND claimed_by_user_id  IS NULL
		   AND status NOT IN ('done', 'dismissed')
	`, userID, taskID, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *taskStore) ClaimQueuedForUser(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if userID == "" {
		return false, fmt.Errorf("ClaimQueuedForUser: empty userID is not a valid claimant")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id = ?,
		       team_id = `+sqliteActingTeamExpr+`,
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE id = ?
		   AND status IN ('queued', 'snoozed')
		   AND claimed_by_user_id  IS NULL
		   AND claimed_by_agent_id IS NULL
	`, userID, taskID, userID, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- Breaker ---

func (s *taskStore) CountConsecutiveFailedRuns(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var count int
	err := s.q.QueryRowContext(ctx, `
		WITH recent AS (
			SELECT
				CASE
					WHEN r.blueprint_run_id IS NULL THEN 'leaf'
					ELSE 'blueprint'
				END AS kind,
				r.blueprint_run_id,
				COALESCE(cr.status, r.status) AS status,
				COALESCE(cr.started_at, r.started_at) AS started_at,
				ROW_NUMBER() OVER (
					PARTITION BY COALESCE(r.blueprint_run_id, r.id)
					ORDER BY r.started_at ASC
				) AS step_rank
			FROM runs r
			JOIN tasks t ON r.task_id = t.id
			LEFT JOIN blueprint_runs cr ON cr.id = r.blueprint_run_id
			WHERE t.entity_id = ?
				AND (
					(r.blueprint_run_id IS NULL AND r.prompt_id = ?)
					OR (cr.blueprint_id = ?)
				)
				AND r.trigger_type = 'event'
		),
		dedup AS (
			SELECT status, started_at
			FROM recent
			WHERE step_rank = 1
			ORDER BY started_at DESC
			LIMIT 20
		)
		SELECT COUNT(*)
		FROM dedup
		WHERE status IN ('failed', 'task_unsolvable', 'aborted')
			AND started_at > (
				SELECT COALESCE(MAX(started_at), '1970-01-01')
				FROM dedup WHERE status = 'completed'
			)
	`, entityID, promptID, promptID).Scan(&count)
	return count, err
}

// --- Internal helpers ---

// sqliteTaskColumnsWithEntity is the canonical column list for every
// task query that feeds scanTask. Lives on TaskStore because it owns
// the surface; ScoreStore's UnscoredTasks references this constant via
// the same-package import.
const sqliteTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.team_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	t.claimed_by_agent_id, t.claimed_by_user_id,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	-- Guard json_extract so malformed or empty legacy snapshots do not fail
	-- the entire task query.
	COALESCE(
		CASE
			WHEN json_valid(NULLIF(e.snapshot_json, ''))
				THEN json_extract(NULLIF(e.snapshot_json, ''), '$.open_subtask_count')
			ELSE NULL
		END,
		0
	)`

// taskScanState holds the NullX intermediates for one row of
// sqliteTaskColumnsWithEntity. Keeping the helper here means
// TaskStore's scan path is right next to the column list — the
// dependency runs in one direction (queries → scan) and lives in one
// file.
type taskScanState struct {
	teamID                             sql.NullString
	priorityScore, autonomySuitability sql.NullFloat64
	aiSummary, priorityReasoning       sql.NullString
	severity, relevanceReason          sql.NullString
	sourceStatus, scoringStatus        sql.NullString
	closeReason, closeEventType        sql.NullString
	snoozeUntil, closedAt              sql.NullTime
	claimedByAgentID, claimedByUserID  sql.NullString
}

func (s *taskScanState) targets(t *domain.Task) []any {
	return []any{
		&t.ID, &t.EntityID, &t.EventType, &t.DedupKey, &t.PrimaryEventID,
		&s.teamID,
		&t.Status, &s.priorityScore, &s.aiSummary, &s.autonomySuitability,
		&s.priorityReasoning, &s.scoringStatus, &s.severity, &s.relevanceReason,
		&s.sourceStatus, &s.snoozeUntil, &s.closeReason, &s.closeEventType,
		&s.closedAt, &t.CreatedAt,
		&s.claimedByAgentID, &s.claimedByUserID,
		&t.Title, &t.SourceURL, &t.EntitySourceID, &t.EntitySource, &t.EntityKind,
		&t.OpenSubtaskCount,
	}
}

func (s *taskScanState) finalize(t *domain.Task) {
	if s.teamID.Valid {
		v := s.teamID.String
		t.TeamID = &v
	}
	if s.priorityScore.Valid {
		t.PriorityScore = &s.priorityScore.Float64
	}
	if s.autonomySuitability.Valid {
		t.AutonomySuitability = &s.autonomySuitability.Float64
	}
	t.AISummary = s.aiSummary.String
	t.PriorityReasoning = s.priorityReasoning.String
	t.Severity = s.severity.String
	t.RelevanceReason = s.relevanceReason.String
	t.SourceStatus = s.sourceStatus.String
	t.ScoringStatus = s.scoringStatus.String
	t.CloseReason = s.closeReason.String
	t.CloseEventType = s.closeEventType.String
	if s.snoozeUntil.Valid {
		t.SnoozeUntil = &s.snoozeUntil.Time
	}
	if s.closedAt.Valid {
		t.ClosedAt = &s.closedAt.Time
	}
	t.ClaimedByAgentID = s.claimedByAgentID.String
	t.ClaimedByUserID = s.claimedByUserID.String
}

func scanTaskFields(rows *sql.Rows, t *domain.Task) error {
	var s taskScanState
	if err := rows.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}

func scanTaskFromRow(row *sql.Row, t *domain.Task) error {
	var s taskScanState
	if err := row.Scan(s.targets(t)...); err != nil {
		return err
	}
	s.finalize(t)
	return nil
}

func queryTasksCtx(ctx context.Context, q queryer, query string, args ...any) ([]domain.Task, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		if err := scanTaskFields(rows, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}
