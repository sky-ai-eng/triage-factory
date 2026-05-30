package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// factoryReadStore is the Postgres impl of db.FactoryReadStore. The
// non-tx Stores.Factory wiring routes through the admin pool (see
// postgres.New) as defense-in-depth for any future caller that
// happens to read outside a claims-set tx; production usage flows
// through TxStores.Factory inside WithTx, which is tx-bound and runs
// as tf_app with RLS active. Either way every method takes an
// explicit orgID and binds it in every WHERE clause.
//
// SQL is written fresh against D3's schema: org_id in every WHERE
// clause as defense in depth, $N placeholders, JSONB extraction for
// snapshot_json.
type factoryReadStore struct{ q queryer }

func newFactoryReadStore(q queryer) db.FactoryReadStore { return &factoryReadStore{q: q} }

var _ db.FactoryReadStore = (*factoryReadStore)(nil)

// pgFactoryActiveRunStatuses is the set of run.status values treated
// as "in flight" for the factory view. Matches the X-button window in
// AgentCard — every state before a terminal transition. Duplicated in
// sqlite/factory.go; intentional per-backend copy.
var pgFactoryActiveRunStatuses = []string{
	"initializing",
	"cloning",
	"fetching",
	"worktree_created",
	"agent_starting",
	"running",
	"pending_approval",
}

func (s *factoryReadStore) EventCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error) {
	// Scoped to the viewer's teams by the same membership semi-join the
	// entity belt uses (factoryEventMembershipExists). The station
	// header's other counters — Triggered24h (tasks) and ActiveRuns
	// (runs) — are already team-scoped because tasks/runs RLS is
	// team-bound; events RLS is org-wide (events_all keys on org_id
	// only), so without this an event on another team's untasked PR
	// would inflate this team's "items at station" count even though
	// that PR never appears on the belt. The semi-join keeps the four
	// counters consistent with one another and with the entity set.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(*)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.created_at > $2
		  AND `+factoryEventMembershipExists+`
		GROUP BY ev.event_type
	`, orgID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) LifetimeDistinctByEventType(ctx context.Context, orgID string) (map[string]int, error) {
	// Team-scoped via the membership semi-join, same as EventCountsSince
	// and the entity belt — a lifetime distinct-entity count must not
	// include entities no team of the viewer's ever tasked, or the
	// station's lifetime readout reports cross-team PRs absent from the
	// belt. The semi-join inherently drops system events too (NULL
	// entity_id can't match a task), so the explicit entity_id IS NOT
	// NULL guard is redundant but kept for clarity / plan stability.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(DISTINCT ev.entity_id)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.entity_id IS NOT NULL
		  AND `+factoryEventMembershipExists+`
		GROUP BY ev.event_type
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) TaskCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT event_type, COUNT(*)
		FROM tasks
		WHERE org_id = $1 AND created_at > $2
		GROUP BY event_type
	`, orgID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) ActiveRuns(ctx context.Context, orgID string) ([]domain.FactoryActiveRun, error) {
	// memory_missing derivation: SKY-204. The agent has not produced
	// its memory file iff no run_memory row exists, OR the row's
	// agent_content is NULL/whitespace. BTRIM with the whitespace set
	// collapses tabs, newlines, and carriage returns onto the same
	// NULL signal so legacy empty/whitespace rows match the canonical
	// "missing" condition.
	query := `
		SELECT
			r.id, r.task_id, r.prompt_id,
			r.status, COALESCE(r.model, ''), r.started_at, r.completed_at,
			r.total_cost_usd, r.duration_ms, r.num_turns,
			COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.session_id, ''),
			(NULLIF(BTRIM(rm.agent_content, E' \t\n\r'), '') IS NULL) AS memory_missing,
			r.trigger_type, COALESCE(r.trigger_id::text, ''),
			COALESCE(r.actor_agent_id::text, ''),
			` + pgTaskColumnsWithEntity + `
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id AND rm.org_id = r.org_id
		JOIN tasks t ON r.task_id = t.id AND t.org_id = r.org_id
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE r.org_id = $1 AND r.status = ANY($2)
		ORDER BY r.started_at DESC
	`

	rows, err := s.q.QueryContext(ctx, query, orgID, pgFactoryActiveRunStatuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryActiveRun
	for rows.Next() {
		var r domain.AgentRun
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64

		runTargets := []any{
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&r.StopReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
			&r.ActorAgentID,
		}
		var ts taskScanState
		if err := rows.Scan(append(runTargets, ts.targets(&t)...)...); err != nil {
			return nil, err
		}
		ts.finalize(&t)
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
		out = append(out, domain.FactoryActiveRun{Run: r, Task: t, EntityEventTyp: t.EventType})
	}
	return out, rows.Err()
}

func (s *factoryReadStore) RecentEventsByEntity(ctx context.Context, orgID string, entityIDs []string, perEntity int) (map[string][]domain.FactoryRecentEvent, error) {
	out := map[string][]domain.FactoryRecentEvent{}
	if len(entityIDs) == 0 || perEntity <= 0 {
		return out, nil
	}
	// Single window-partitioned query — Postgres' param limit is
	// 65535 (uint16), so the entire entityIDs slice fits in one
	// round-trip even at factoryEntityLimit=500 without chunking.
	// Tie-break on event id (uuid) to keep ordering stable when
	// two events share an event_at to the microsecond.
	query := `
		SELECT entity_id, event_type, event_at, detected_at
		FROM (
			SELECT entity_id, event_type,
				COALESCE(occurred_at, created_at) AS event_at,
				created_at AS detected_at,
				id AS event_id,
				ROW_NUMBER() OVER (
					PARTITION BY entity_id
					ORDER BY COALESCE(occurred_at, created_at) DESC, id DESC
				) AS rn
			FROM events
			WHERE org_id = $1 AND entity_id = ANY($2)
		) ranked
		WHERE rn <= $3
		ORDER BY entity_id, event_at ASC, event_id ASC
	`
	rows, err := s.q.QueryContext(ctx, query, orgID, entityIDs, perEntity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var entityID, eventType string
		var eventAt, detectedAt time.Time
		if err := rows.Scan(&entityID, &eventType, &eventAt, &detectedAt); err != nil {
			return nil, err
		}
		out[entityID] = append(out[entityID], domain.FactoryRecentEvent{
			EventType:  eventType,
			CreatedAt:  eventAt,
			DetectedAt: detectedAt,
		})
	}
	return out, rows.Err()
}

// pgFactoryEntitySelectColumns is the SELECT list shared by the
// active and closed-grace queries in Entities. The two correlated
// subqueries pull the latest event_type + created_at; Postgres can
// scan these directly into time.Time / sql.NullTime without the
// COALESCE-loses-type-hint dance SQLite needs (no parseDBDatetime
// helper required on this side).
const pgFactoryEntitySelectColumns = `
	e.id, e.source, e.source_id, e.kind,
	COALESCE(e.title, ''), COALESCE(e.url, ''),
	COALESCE(e.snapshot_json::text, ''), COALESCE(e.description, ''),
	e.state, e.created_at, e.last_polled_at, e.closed_at,
	(SELECT event_type FROM events WHERE org_id = e.org_id AND entity_id = e.id ORDER BY created_at DESC LIMIT 1),
	(SELECT created_at FROM events WHERE org_id = e.org_id AND entity_id = e.id ORDER BY created_at DESC LIMIT 1)
`

// factoryEntityMembershipExists is the semi-join that scopes the
// factory to the viewer's teams. An entity belongs in a team's factory
// iff a task for that team has *ever* existed on it (over all task
// statuses — a long-closed task still counts). Repos are configured
// org-wide, so polling produces org-wide entities + events; rather than
// fork the shared entity per team (which would break the snapshot-diff
// re-emit invariant and the append-only event log), the entity↔team
// relationship is derived through tasks, which carry team_id.
//
// The team scoping itself is free: the factory snapshot runs tx-bound
// as tf_app with RLS active (TxStores.Factory), and tasks_select RLS
// already constrains rows to the viewer's org + teams. So the EXISTS
// auto-scopes to the viewer's teams with no explicit team_id in the
// query — RLS does it. org_id is bound here too as defense-in-depth,
// matching the convention every method in this file follows. Membership
// is monotonic since tasks terminate to done/dismissed, never delete.
const factoryEntityMembershipExists = `EXISTS (SELECT 1 FROM tasks t WHERE t.entity_id = e.id AND t.org_id = e.org_id)`

// factoryEntityMembershipForTeams is the team-narrowed variant of
// factoryEntityMembershipExists used by the per-page read filter: the
// correlated task must additionally be owned by one of the teams
// (t.team_id) or make the entity visible to one while unclaimed (a
// task_teams row). placeholders is the comma-joined placeholder list
// (e.g. "$3, $4") bound to the team ids and referenced twice.
//
// The team test mirrors the tasks_select RLS policy (see
// pgTaskTeamFilter for the full rationale): each branch is
// tf.user_in_team-gated so a forged team id can't pull an entity in via
// a team the caller isn't on, and the task_teams (visibility) branch is
// limited to UNCLAIMED tasks because a claim consolidates to team_id
// without draining the stale visibility rows. The correlated task t is
// itself RLS-scoped to the viewer.
func factoryEntityMembershipForTeams(placeholders string) string {
	return `EXISTS (SELECT 1 FROM tasks t WHERE t.entity_id = e.id AND t.org_id = e.org_id` +
		` AND ((t.team_id IN (` + placeholders + `) AND tf.user_in_team(t.team_id))` +
		` OR (t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL` +
		` AND EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id` +
		` AND tt.team_id IN (` + placeholders + `) AND tf.user_in_team(tt.team_id)))))`
}

// factoryEventMembershipExists is the membership semi-join correlated
// against an events row (alias ev) rather than an entities row. Same
// shape and same RLS auto-scoping as factoryEntityMembershipExists —
// used to scope the station-throughput aggregates (EventCountsSince,
// LifetimeDistinctByEventType) to the viewer's teams so the header
// counters match the team-scoped entity belt.
const factoryEventMembershipExists = `EXISTS (SELECT 1 FROM tasks t WHERE t.entity_id = ev.entity_id AND t.org_id = ev.org_id)`

func (s *factoryReadStore) Entities(ctx context.Context, orgID string, limit int, teamIDs []string) ([]domain.FactoryEntityRow, error) {
	// The membership semi-join scopes the belt to the viewer's teams
	// (via tasks RLS). The optional per-page filter narrows it further to
	// a set of teams by tightening that same semi-join's correlated task
	// to one any selected team owns or can see. The team placeholders
	// appear before LIMIT in the text but bind by number, which Postgres
	// resolves regardless of order — active args start the teams at $3
	// (after orgID, limit), closed at $4 (after orgID, cutoff, limit).
	activeMembership := factoryEntityMembershipExists
	closedMembership := factoryEntityMembershipExists
	activeArgs := []any{orgID, limit}
	closedArgs := []any{orgID, time.Now().Add(-db.FactoryClosedGracePeriod), db.FactoryClosedGraceLimit}
	if len(teamIDs) > 0 {
		activePH := make([]string, len(teamIDs))
		for i, id := range teamIDs {
			activePH[i] = fmt.Sprintf("$%d", 3+i)
			activeArgs = append(activeArgs, id)
		}
		activeMembership = factoryEntityMembershipForTeams(strings.Join(activePH, ", "))
		closedPH := make([]string, len(teamIDs))
		for i, id := range teamIDs {
			closedPH[i] = fmt.Sprintf("$%d", 4+i)
			closedArgs = append(closedArgs, id)
		}
		closedMembership = factoryEntityMembershipForTeams(strings.Join(closedPH, ", "))
	}

	active, err := queryFactoryEntities(ctx, s.q, `
		SELECT `+pgFactoryEntitySelectColumns+`
		FROM entities e
		WHERE e.org_id = $1 AND e.state = 'active'
		  AND `+activeMembership+`
		ORDER BY e.created_at DESC
		LIMIT $2
	`, activeArgs...)
	if err != nil {
		return nil, fmt.Errorf("factory entities active: %w", err)
	}

	closed, err := queryFactoryEntities(ctx, s.q, `
		SELECT `+pgFactoryEntitySelectColumns+`
		FROM entities e
		WHERE e.org_id = $1 AND e.closed_at IS NOT NULL AND e.closed_at > $2
		  AND `+closedMembership+`
		ORDER BY e.closed_at DESC
		LIMIT $3
	`, closedArgs...)
	if err != nil {
		return nil, fmt.Errorf("factory entities closed-grace: %w", err)
	}

	if len(closed) == 0 {
		return active, nil
	}
	return append(active, closed...), nil
}

func queryFactoryEntities(ctx context.Context, q queryer, query string, args ...any) ([]domain.FactoryEntityRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryEntityRow
	for rows.Next() {
		var row domain.FactoryEntityRow
		var lastPolledAt, closedAt sql.NullTime
		var latestType sql.NullString
		var latestAt sql.NullTime
		if err := rows.Scan(
			&row.Entity.ID, &row.Entity.Source, &row.Entity.SourceID, &row.Entity.Kind,
			&row.Entity.Title, &row.Entity.URL,
			&row.Entity.SnapshotJSON, &row.Entity.Description,
			&row.Entity.State, &row.Entity.CreatedAt, &lastPolledAt, &closedAt,
			&latestType, &latestAt,
		); err != nil {
			return nil, err
		}
		if lastPolledAt.Valid {
			row.Entity.LastPolledAt = &lastPolledAt.Time
		}
		if closedAt.Valid {
			row.Entity.ClosedAt = &closedAt.Time
		}
		if latestType.Valid {
			row.LatestEventType = latestType.String
		}
		if latestAt.Valid {
			row.LatestEventAt = &latestAt.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
