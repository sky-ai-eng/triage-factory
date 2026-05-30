package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// taskStore is the Postgres impl of db.TaskStore. SQL is written fresh
// against D3's schema: org_id in every WHERE clause as defense in depth
// alongside RLS, $N placeholders, JSONB extraction for snapshot_json.
//
// Holds two pools:
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer (server tasks handler, swipe handlers) runs here.
//     The scorer reads tasks via the admin-pooled ScoreStore — not
//     this store.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). Background
//     goroutines (router eventbus subscriber, tracker stale-review
//     reconciliation) route through admin because they have no
//     JWT-claims context. org_id stays in the WHERE clause as
//     defense in depth.
//
// Slice binds pass `[]string` directly into ANY($N) — pgx's
// database/sql adapter handles slice-to-array conversion natively,
// same as scoreStore.MarkScoring's taskIDs binding.
type taskStore struct {
	q     queryer
	admin queryer
}

func newTaskStore(q, admin queryer) db.TaskStore {
	return &taskStore{q: q, admin: admin}
}

var _ db.TaskStore = (*taskStore)(nil)

// --- Lookup ---

func (s *taskStore) Get(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	return getTask(ctx, s.q, orgID, taskID)
}

func (s *taskStore) GetSystem(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
	return getTask(ctx, s.admin, orgID, taskID)
}

func getTask(ctx context.Context, q queryer, orgID, taskID string) (*domain.Task, error) {
	var t domain.Task
	err := scanTaskFromRow(q.QueryRowContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.id = $2
	`, orgID, taskID), &t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// pgTaskTeamFilter returns the SQL fragment that narrows a tasks query
// (alias t) to a *set* of teams, and the args extended with the team ids.
// Empty teamIDs is a no-op (the union-of-the-viewer's-teams default).
//
// The predicate mirrors the tasks_select RLS policy's own team test so
// the filter narrows to *exactly* what membership would admit, never
// wider:
//
//   - The selected ids are first intersected with tf.user_in_team — a
//     forged ?team_id= for a team the caller isn't in contributes
//     nothing, even when RLS admits the row via a *different* team the
//     caller does belong to. Without this the filter would leak: an
//     unclaimed task visible to both the caller's team A and an
//     outside team B would still match a ?team_id=B narrow. (Codex P2)
//
//   - task_teams (visibility) is consulted only while the task is
//     UNCLAIMED. RLS documents — and HandoffAgentClaim enforces — that
//     a claim consolidates the task to its owning team_id; the stale
//     task_teams rows are not drained, so matching them post-claim would
//     show a team-A-owned claimed task under a ?team_id=B narrow. Gating
//     the task_teams branch on "both claim cols NULL" keeps claimed rows
//     matchable only by their owning team_id. (Codex P2)
//
// Both the owning-team branch and the visibility branch are themselves
// tf.user_in_team-gated, so the whole predicate can only ever shrink the
// RLS-admitted set, never widen it.
func pgTaskTeamFilter(teamIDs []string, args []any) (string, []any) {
	if len(teamIDs) == 0 {
		return "", args
	}
	ph := make([]string, len(teamIDs))
	for i, id := range teamIDs {
		ph[i] = fmt.Sprintf("$%d", len(args)+1+i)
		args = append(args, id)
	}
	list := strings.Join(ph, ", ")
	return fmt.Sprintf(
		" AND ("+
			"(t.team_id IN (%s) AND tf.user_in_team(t.team_id))"+
			" OR (t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL"+
			" AND EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id"+
			" AND tt.team_id IN (%s) AND tf.user_in_team(tt.team_id)))"+
			")",
		list, list,
	), args
}

func (s *taskStore) Queued(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error) {
	// SKY-261 B+ derived filter mirrors SQLite. The event_handlers
	// derived table is org-scoped so rules in another org can't
	// influence ordering — load-bearing in multi mode.
	args := []any{orgID}
	teamClause, args := pgTaskTeamFilter(teamIDs, args)
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = true AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.org_id = $1
			AND t.status = 'queued'
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL
			AND (t.snooze_until IS NULL OR t.snooze_until <= NOW())`+teamClause+`
		ORDER BY COALESCE(tr.sort_order, 999) ASC, COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) QueuedIncludingSnoozed(ctx context.Context, orgID string, teamIDs []string) ([]domain.Task, error) {
	// SKY-330: snoozed rows render at the tail regardless of
	// priority so deferred entries don't jump above live queued
	// work. Postgres sorts false before true on the boolean
	// expression (matches the SQLite mirror's 0/1 ordering).
	args := []any{orgID}
	teamClause, args := pgTaskTeamFilter(teamIDs, args)
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		LEFT JOIN (
			SELECT org_id, event_type, MIN(sort_order) AS sort_order
			FROM event_handlers
			WHERE enabled = true AND kind = 'rule'
			GROUP BY org_id, event_type
		) tr ON t.event_type = tr.event_type AND t.org_id = tr.org_id
		WHERE t.org_id = $1
			AND t.status IN ('queued', 'snoozed')
			AND t.claimed_by_agent_id IS NULL
			AND t.claimed_by_user_id  IS NULL`+teamClause+`
		ORDER BY (t.status = 'snoozed') ASC,
		         COALESCE(tr.sort_order, 999) ASC,
		         COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) ByStatus(ctx context.Context, orgID, status string, teamIDs []string) ([]domain.Task, error) {
	switch status {
	case "claimed":
		// SKY-330: "claimed" = any claim (user or bot) at
		// status='queued'. Mirrors the SQLite branch — see that
		// file's comment for the full rationale around the
		// status='queued' filter and including bot claims to
		// surface the delegate-spawn-failure retry case.
		args := []any{orgID}
		teamClause, args := pgTaskTeamFilter(teamIDs, args)
		return queryTasksCtx(ctx, s.q, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1
				AND (t.claimed_by_user_id IS NOT NULL OR t.claimed_by_agent_id IS NOT NULL)
				AND t.status = 'queued'`+teamClause+`
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	case "delegated":
		args := []any{orgID}
		teamClause, args := pgTaskTeamFilter(teamIDs, args)
		return queryTasksCtx(ctx, s.q, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1
				AND t.claimed_by_agent_id IS NOT NULL
				AND t.status NOT IN ('done', 'dismissed')`+teamClause+`
			ORDER BY COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	case "done", "dismissed":
		// SKY-330: cap the Done column at the last 7 days. Mirrors
		// the SQLite branch — every close path now populates
		// closed_at, so the NOT NULL guard turns missing values into
		// surfacable bugs rather than letting them accumulate.
		args := []any{orgID, status}
		teamClause, args := pgTaskTeamFilter(teamIDs, args)
		return queryTasksCtx(ctx, s.q, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1
				AND t.status = $2
				AND t.closed_at IS NOT NULL
				AND t.closed_at >= NOW() - INTERVAL '7 days'`+teamClause+`
			ORDER BY t.closed_at DESC, COALESCE(t.priority_score, 0.5) DESC
		`, args...)
	}
	args := []any{orgID, status}
	teamClause, args := pgTaskTeamFilter(teamIDs, args)
	return queryTasksCtx(ctx, s.q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.status = $2`+teamClause+`
		ORDER BY COALESCE(t.priority_score, 0.5) DESC
	`, args...)
}

func (s *taskStore) FindActiveByEntityAndType(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	return findActiveTasksByEntityAndType(ctx, s.q, orgID, entityID, eventType)
}

func (s *taskStore) FindActiveByEntityAndTypeSystem(ctx context.Context, orgID, entityID, eventType string) ([]domain.Task, error) {
	return findActiveTasksByEntityAndType(ctx, s.admin, orgID, entityID, eventType)
}

func findActiveTasksByEntityAndType(ctx context.Context, q queryer, orgID, entityID, eventType string) ([]domain.Task, error) {
	return queryTasksCtx(ctx, q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3
			AND t.status NOT IN ('done', 'dismissed')
	`, orgID, entityID, eventType)
}

func (s *taskStore) FindActiveByEntity(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	return findActiveTasksByEntity(ctx, s.q, orgID, entityID)
}

func (s *taskStore) FindActiveByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Task, error) {
	return findActiveTasksByEntity(ctx, s.admin, orgID, entityID)
}

func findActiveTasksByEntity(ctx context.Context, q queryer, orgID, entityID string) ([]domain.Task, error) {
	return queryTasksCtx(ctx, q, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2
			AND t.status NOT IN ('done', 'dismissed')
	`, orgID, entityID)
}

func (s *taskStore) ListActiveRefsForEntities(ctx context.Context, orgID string, entityIDs []string, teamIDs []string) ([]domain.PendingTaskRef, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	// Postgres has no comparable variable-bind cap to SQLite's 999/500;
	// the array bind via lib/pq keeps the whole list in a single
	// placeholder regardless of size, so no chunking is needed. The
	// table is aliased t so the shared team-filter fragment (same
	// RLS-mirroring predicate as Queued) applies — narrowing the refs to
	// the selected teams keeps the station drawer in sync with a
	// team-filtered entity belt.
	args := []any{orgID, entityIDs}
	teamClause, args := pgTaskTeamFilter(teamIDs, args)
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.id, t.entity_id, t.event_type, t.dedup_key
		FROM tasks t
		WHERE t.org_id = $1
			AND t.entity_id = ANY($2)
			AND t.status NOT IN ('done', 'dismissed')`+teamClause+`
		ORDER BY t.entity_id, t.event_type, t.created_at DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.PendingTaskRef, 0, len(entityIDs))
	for rows.Next() {
		var ref domain.PendingTaskRef
		if err := rows.Scan(&ref.ID, &ref.EntityID, &ref.EventType, &ref.DedupKey); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *taskStore) EntityIDsWithActiveTasks(ctx context.Context, orgID, source string) (map[string]struct{}, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT t.entity_id
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND e.source = $2 AND t.status NOT IN ('done', 'dismissed')
	`, orgID, source)
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
	return findOrCreateTaskAt(ctx, s.q, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
}

func (s *taskStore) FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	return findOrCreateTaskAt(ctx, s.admin, orgID, teamID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt)
}

func (s *taskStore) SetVisibilityTeams(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	return setVisibilityTeams(ctx, s.q, orgID, taskID, teamIDs)
}

func (s *taskStore) SetVisibilityTeamsSystem(ctx context.Context, orgID, taskID string, teamIDs []string) error {
	return setVisibilityTeams(ctx, s.admin, orgID, taskID, teamIDs)
}

func setVisibilityTeams(ctx context.Context, q queryer, orgID, taskID string, teamIDs []string) error {
	for _, teamID := range teamIDs {
		if teamID == "" {
			continue
		}
		// Org-visible handlers route the LocalDefaultTeamID sentinel
		// through here; resolve it to the org's canonical team so the
		// task_teams FK to teams(id) holds, mirroring findOrCreateTaskAt.
		teamBind, err := resolveTeamBind(ctx, q, orgID, teamID)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO task_teams (task_id, team_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING
		`, taskID, teamBind); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskStore) VisibilityTeams(ctx context.Context, orgID, taskID string) ([]string, error) {
	return visibilityTeams(ctx, s.q, taskID)
}

func (s *taskStore) VisibilityTeamsSystem(ctx context.Context, orgID, taskID string) ([]string, error) {
	return visibilityTeams(ctx, s.admin, taskID)
}

func (s *taskStore) SetOwnerTeam(ctx context.Context, orgID, taskID, teamID string) error {
	return setOwnerTeam(ctx, s.q, orgID, taskID, teamID)
}

func (s *taskStore) SetOwnerTeamSystem(ctx context.Context, orgID, taskID, teamID string) error {
	return setOwnerTeam(ctx, s.admin, orgID, taskID, teamID)
}

func setOwnerTeam(ctx context.Context, q queryer, orgID, taskID, teamID string) error {
	if teamID == "" {
		return nil
	}
	// Resolve the LocalDefaultTeamID sentinel to the canonical team so
	// the teams(id) FK holds, mirroring FindOrCreate/StampAgentClaim.
	teamBind, err := resolveTeamBind(ctx, q, orgID, teamID)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
		UPDATE tasks SET team_id = $1::uuid WHERE org_id = $2 AND id = $3
	`, teamBind, orgID, taskID)
	return err
}

func visibilityTeams(ctx context.Context, q queryer, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT team_id FROM task_teams WHERE task_id = $1::uuid`, taskID)
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

// resolveTeamBind maps the LocalDefaultTeamID sentinel (carried by
// org-visible handlers via handlerTeamID) to the org's canonical team —
// the oldest by created_at. Real team UUIDs pass through unchanged. An
// empty teamID stays empty so callers can apply their own required-team
// guard.
func resolveTeamBind(ctx context.Context, q queryer, orgID, teamID string) (string, error) {
	if teamID != runmode.LocalDefaultTeamID {
		return teamID, nil
	}
	var canonical string
	if err := q.QueryRowContext(ctx,
		`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`,
		orgID,
	).Scan(&canonical); err != nil {
		return "", fmt.Errorf("task store: resolve canonical team for org %s: %w", orgID, err)
	}
	return canonical, nil
}

func findOrCreateTaskAt(ctx context.Context, q queryer, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	// team_id is the owning/attributed team stamped on a newly created
	// row — caller-supplied. User-source handlers carry a real team
	// UUID. Org-visible handlers (visibility='org', team_id NULL)
	// collapse to runmode.LocalDefaultTeamID in handlerTeamID(); resolve
	// that sentinel to the org's canonical team (oldest by created_at) so
	// shipped system rules create tasks in Postgres mode. A truly-empty
	// teamBind (caller passed "") still trips the guard below.
	teamBind, err := resolveTeamBind(ctx, q, orgID, teamID)
	if err != nil {
		return nil, false, err
	}
	if teamBind == "" {
		return nil, false, fmt.Errorf("task store: team_id required for Postgres FindOrCreate (router must thread the user-selected team from the matched event_handler)")
	}

	// SELECT first so the common path (task already exists) stays a
	// single round-trip. The partial unique index on
	// (org_id, entity_id, event_type, dedup_key) WHERE status NOT IN
	// ('done', 'dismissed') is the race backstop on INSERT — one task
	// per real situation, independent of team, so a situation already
	// tasked by another team's rule is returned here.
	var existing domain.Task
	err = scanTaskFromRow(q.QueryRowContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3 AND t.dedup_key = $4
			AND t.status NOT IN ('done', 'dismissed')
		LIMIT 1
	`, orgID, entityID, eventType, dedupKey), &existing)
	if err == nil {
		return &existing, false, nil
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}

	// ON CONFLICT keys off the partial unique index — on a lost race
	// the INSERT no-ops and we re-read.
	taskID := uuid.New().String()
	res, err := q.ExecContext(ctx, `
		INSERT INTO tasks (id, org_id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility,
		                   creator_user_id)
		VALUES ($1, $2, $3, $4, $5, $6,
		        'queued', $7, 'pending', $8,
		        $9::uuid,
		        'team',
		        COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)))
		ON CONFLICT DO NOTHING
	`, taskID, orgID, entityID, eventType, dedupKey, primaryEventID, defaultPriority, createdAt, teamBind)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost the race — another goroutine inserted between our
		// SELECT and INSERT. Re-read to return the winner's row.
		var raced domain.Task
		err2 := scanTaskFromRow(q.QueryRowContext(ctx, `
			SELECT `+pgTaskColumnsWithEntity+`
			FROM tasks t
			JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
			WHERE t.org_id = $1 AND t.entity_id = $2 AND t.event_type = $3 AND t.dedup_key = $4
				AND t.status NOT IN ('done', 'dismissed')
			LIMIT 1
		`, orgID, entityID, eventType, dedupKey), &raced)
		if err2 != nil {
			return nil, false, fmt.Errorf("findorcreate: race reread: %w", err2)
		}
		return &raced, false, nil
	}

	task, err := getTask(ctx, q, orgID, taskID)
	if err != nil {
		return nil, false, err
	}
	return task, true, nil
}

func (s *taskStore) Bump(ctx context.Context, orgID, taskID, eventID string) error {
	return bumpTask(ctx, s.q, orgID, taskID)
}

func (s *taskStore) BumpSystem(ctx context.Context, orgID, taskID, eventID string) error {
	return bumpTask(ctx, s.admin, orgID, taskID)
}

func bumpTask(ctx context.Context, q queryer, orgID, taskID string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE tasks
		SET status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END,
		    snooze_until = CASE WHEN status = 'snoozed' THEN NULL ELSE snooze_until END
		WHERE org_id = $1 AND id = $2
	`, orgID, taskID)
	return err
}

func (s *taskStore) Close(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error {
	return closeTask(ctx, s.q, orgID, taskID, closeReason, closeEventType)
}

func (s *taskStore) CloseSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType string) error {
	return closeTask(ctx, s.admin, orgID, taskID, closeReason, closeEventType)
}

func closeTask(ctx context.Context, q queryer, orgID, taskID, closeReason, closeEventType string) error {
	var cet any
	if closeEventType != "" {
		cet = closeEventType
	}
	_, err := q.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = $1, close_event_type = $2,
		                 closed_at = NOW()
		WHERE org_id = $3 AND id = $4 AND status NOT IN ('done', 'dismissed')
	`, closeReason, cet, orgID, taskID)
	return err
}

func (s *taskStore) CloseAllForEntity(ctx context.Context, orgID, entityID, closeReason string) (int, error) {
	return closeAllTasksForEntity(ctx, s.q, orgID, entityID, closeReason)
}

func (s *taskStore) CloseAllForEntitySystem(ctx context.Context, orgID, entityID, closeReason string) (int, error) {
	return closeAllTasksForEntity(ctx, s.admin, orgID, entityID, closeReason)
}

func closeAllTasksForEntity(ctx context.Context, q queryer, orgID, entityID, closeReason string) (int, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', close_reason = $1, closed_at = NOW()
		WHERE org_id = $2 AND entity_id = $3 AND status NOT IN ('done', 'dismissed')
	`, closeReason, orgID, entityID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *taskStore) SetStatus(ctx context.Context, orgID, taskID, status string) error {
	return setTaskStatus(ctx, s.q, orgID, taskID, status)
}

func (s *taskStore) SetStatusSystem(ctx context.Context, orgID, taskID, status string) error {
	return setTaskStatus(ctx, s.admin, orgID, taskID, status)
}

func setTaskStatus(ctx context.Context, q queryer, orgID, taskID, status string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE tasks SET status = $1 WHERE org_id = $2 AND id = $3
	`, status, orgID, taskID)
	return err
}

func (s *taskStore) AdvanceStatusForUser(ctx context.Context, orgID, taskID, userID, newStatus string) (bool, error) {
	if newStatus != "in_progress" && newStatus != "in_review" {
		return false, nil
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET status = $1
		 WHERE org_id = $2
		   AND id = $3
		   AND claimed_by_user_id = $4
		   AND status IN ('queued', 'in_progress', 'in_review')
	`, newStatus, orgID, taskID, userID)
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
	return recordTaskEvent(ctx, s.q, orgID, taskID, eventID, kind)
}

func (s *taskStore) RecordEventSystem(ctx context.Context, orgID, taskID, eventID, kind string) error {
	return recordTaskEvent(ctx, s.admin, orgID, taskID, eventID, kind)
}

func recordTaskEvent(ctx context.Context, q queryer, orgID, taskID, eventID, kind string) error {
	// task_events has org_id NOT NULL in D3 schema; populate it so the
	// composite FK to (org_id, task_id) resolves.
	_, err := q.ExecContext(ctx, `
		INSERT INTO task_events (org_id, task_id, event_id, kind, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT DO NOTHING
	`, orgID, taskID, eventID, kind)
	return err
}

// --- Claim mutations ---

func (s *taskStore) SetClaimedByAgent(ctx context.Context, orgID, taskID, agentID string) error {
	var a any
	if agentID != "" {
		a = agentID
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       claimed_by_user_id  = NULL
		 WHERE org_id = $2 AND id = $3
	`, a, orgID, taskID)
	return err
}

func (s *taskStore) SetClaimedByUser(ctx context.Context, orgID, taskID, userID string) error {
	var u any
	if userID != "" {
		u = userID
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = $1,
		       claimed_by_agent_id = NULL
		 WHERE org_id = $2 AND id = $3
	`, u, orgID, taskID)
	return err
}

func (s *taskStore) StampAgentClaimIfUnclaimed(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	return stampAgentClaimIfUnclaimed(ctx, s.q, orgID, taskID, agentID, actingTeamID)
}

func (s *taskStore) StampAgentClaimIfUnclaimedSystem(ctx context.Context, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	return stampAgentClaimIfUnclaimed(ctx, s.admin, orgID, taskID, agentID, actingTeamID)
}

func stampAgentClaimIfUnclaimed(ctx context.Context, q queryer, orgID, taskID, agentID, actingTeamID string) (bool, error) {
	if agentID == "" {
		return false, errors.New("StampAgentClaimIfUnclaimed: empty agentID")
	}
	// actingTeamID is the firing trigger's team; on a successful claim
	// it consolidates the card to that owning team. Empty leaves
	// team_id unchanged. Resolve the LocalDefaultTeamID sentinel
	// (carried by org-visible handlers via handlerTeamID) to the org's
	// canonical team first, mirroring FindOrCreate/SetVisibilityTeams —
	// otherwise the sentinel uuid would be written straight into
	// tasks.team_id and trip the teams(id) FK in multi-mode.
	if actingTeamID != "" {
		var err error
		if actingTeamID, err = resolveTeamBind(ctx, q, orgID, actingTeamID); err != nil {
			return false, err
		}
	}
	res, err := q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       team_id = COALESCE(NULLIF($4, '')::uuid, team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND claimed_by_user_id IS NULL
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != $1)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, orgID, taskID, actingTeamID)
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
	if agentID == "" {
		return db.HandoffRefused, errors.New("HandoffAgentClaim: empty agentID")
	}
	if userID == "" {
		return db.HandoffRefused, errors.New("HandoffAgentClaim: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_agent_id = $1,
		       claimed_by_user_id  = NULL,
		       team_id = COALESCE(
		           (SELECT tt.team_id FROM task_teams tt
		              JOIN memberships m ON m.team_id = tt.team_id
		             WHERE tt.task_id = $3 AND m.user_id = $4
		             ORDER BY (tt.team_id = tasks.team_id) DESC, tt.team_id ASC LIMIT 1),
		           team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND (claimed_by_user_id  IS NULL OR claimed_by_user_id  = $4)
		   AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id != $1)
		   AND status NOT IN ('done', 'dismissed')
	`, agentID, orgID, taskID, userID)
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
	// 0 rows — re-read to distinguish refused vs no-op. Terminal-status
	// check takes precedence over same-agent so a sticky-claim on a
	// closed task doesn't fall through to HandoffNoOp.
	var curUser, curAgent sql.NullString
	var curStatus string
	err = s.q.QueryRowContext(ctx,
		`SELECT claimed_by_user_id, claimed_by_agent_id, status FROM tasks WHERE org_id = $1 AND id = $2`,
		orgID, taskID,
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
	// Mirrors HandoffAgentClaim's team_id COALESCE: the caller's team in
	// the task's visibility set (preferring the current owner on a tie),
	// else the current owner. Read-only; under the app pool tasks_select
	// RLS scopes the row to the viewer.
	var team string
	err := s.q.QueryRowContext(ctx, `
		SELECT COALESCE(
		         (SELECT tt.team_id::text FROM task_teams tt
		            JOIN memberships m ON m.team_id = tt.team_id
		           WHERE tt.task_id = $2 AND m.user_id = $3
		           ORDER BY (tt.team_id = t.team_id) DESC, tt.team_id ASC LIMIT 1),
		         t.team_id::text)
		  FROM tasks t
		 WHERE t.org_id = $1 AND t.id = $2
	`, orgID, taskID, userID).Scan(&team)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve claim team: %w", err)
	}
	return team, nil
}

func (s *taskStore) TakeoverClaimFromAgent(ctx context.Context, orgID, taskID, userID string) (bool, error) {
	if userID == "" {
		return false, errors.New("TakeoverClaimFromAgent: empty userID")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id  = $1,
		       claimed_by_agent_id = NULL,
		       team_id = COALESCE(
		           (SELECT tt.team_id FROM task_teams tt
		              JOIN memberships m ON m.team_id = tt.team_id
		             WHERE tt.task_id = $3 AND m.user_id = $1
		             ORDER BY (tt.team_id = tasks.team_id) DESC, tt.team_id ASC LIMIT 1),
		           team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND claimed_by_agent_id IS NOT NULL
		   AND claimed_by_user_id  IS NULL
		   AND status NOT IN ('done', 'dismissed')
	`, userID, orgID, taskID)
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
	if userID == "" {
		return false, errors.New("ClaimQueuedForUser: empty userID is not a valid claimant")
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE tasks
		   SET claimed_by_user_id = $1,
		       team_id = COALESCE(
		           (SELECT tt.team_id FROM task_teams tt
		              JOIN memberships m ON m.team_id = tt.team_id
		             WHERE tt.task_id = $3 AND m.user_id = $1
		             ORDER BY (tt.team_id = tasks.team_id) DESC, tt.team_id ASC LIMIT 1),
		           team_id),
		       snooze_until = NULL,
		       status = CASE WHEN status = 'snoozed' THEN 'queued' ELSE status END
		 WHERE org_id = $2 AND id = $3
		   AND status IN ('queued', 'snoozed')
		   AND claimed_by_user_id  IS NULL
		   AND claimed_by_agent_id IS NULL
	`, userID, orgID, taskID)
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
	return countConsecutiveFailedRuns(ctx, s.q, orgID, entityID, promptID)
}

func (s *taskStore) CountConsecutiveFailedRunsSystem(ctx context.Context, orgID, entityID, promptID string) (int, error) {
	return countConsecutiveFailedRuns(ctx, s.admin, orgID, entityID, promptID)
}

func countConsecutiveFailedRuns(ctx context.Context, q queryer, orgID, entityID, promptID string) (int, error) {
	// Same shape as SQLite. Postgres' ROW_NUMBER / OVER / CTEs are
	// identical syntax-wise; the only difference is the started_at
	// fallback literal (Postgres requires a typed cast on the
	// '1970-01-01' default for the comparison).
	var count int
	err := q.QueryRowContext(ctx, `
		WITH recent AS (
			SELECT
				CASE
					WHEN r.chain_run_id IS NULL THEN 'leaf'
					ELSE 'chain'
				END AS kind,
				r.chain_run_id,
				COALESCE(cr.status, r.status) AS status,
				COALESCE(cr.started_at, r.started_at) AS started_at,
				ROW_NUMBER() OVER (
					PARTITION BY COALESCE(r.chain_run_id, r.id)
					ORDER BY r.started_at ASC
				) AS step_rank
			FROM runs r
			JOIN tasks t ON r.task_id = t.id AND r.org_id = t.org_id
			LEFT JOIN chain_runs cr ON cr.id = r.chain_run_id AND cr.org_id = r.org_id
			WHERE r.org_id = $1
				AND t.entity_id = $2
				AND (
					(r.chain_run_id IS NULL AND r.prompt_id = $3)
					OR (cr.chain_prompt_id = $3)
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
				SELECT COALESCE(MAX(started_at), TIMESTAMPTZ '1970-01-01')
				FROM dedup WHERE status = 'completed'
			)
	`, orgID, entityID, promptID).Scan(&count)
	return count, err
}

// --- Internal helpers ---

// pgTaskColumnsWithEntity is the canonical column list for every task
// query that feeds scanTaskFields. Owned here on TaskStore;
// ScoreStore.UnscoredTasks references it via the same-package import.
//
// Two notable differences from SQLite:
//   - snapshot_json is JSONB (always-valid by type), so the json_valid
//     guard is unnecessary; ->> '...' returns NULL for a missing key
//     and COALESCE picks the 0 default.
//   - Time columns are TIMESTAMPTZ; sql.NullTime scans them directly.
const pgTaskColumnsWithEntity = `
	t.id, t.entity_id, t.event_type, t.dedup_key, t.primary_event_id,
	t.team_id,
	t.status, t.priority_score, t.ai_summary, t.autonomy_suitability,
	t.priority_reasoning, t.scoring_status, t.severity, t.relevance_reason,
	t.source_status, t.snooze_until, t.close_reason, t.close_event_type,
	t.closed_at, t.created_at,
	t.claimed_by_agent_id, t.claimed_by_user_id,
	COALESCE(e.title, ''), COALESCE(e.url, ''), e.source_id, e.source, e.kind,
	COALESCE((e.snapshot_json->>'open_subtask_count')::int, 0)`

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
		t.TeamID = s.teamID.String
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
