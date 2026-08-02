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

func (s *factoryReadStore) EventCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error) {
	// Scoped to the viewer's teams by the same tracked-set semi-join the
	// entity belt uses (factoryEventTrackedExists). The station header's
	// other counters — Triggered24h (tasks) and ActiveRuns (runs) — are
	// team-scoped because tasks/runs RLS is team-bound; events RLS is
	// org-wide (events_all keys on org_id only), so without this an event
	// on a PR outside the viewer's tracked set would inflate this team's
	// "items at station" count even though that PR never appears on the
	// belt. The semi-join keeps the counters consistent with the entity
	// set the belt renders.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(*)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.created_at > $2
		  AND `+factoryEventTrackedExists+`
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
	// Team-scoped via the tracked-set semi-join, same as EventCountsSince
	// and the entity belt — a lifetime distinct-entity count must not
	// include entities outside the viewer's tracked set, or the station's
	// lifetime readout reports PRs absent from the belt. The semi-join
	// inherently drops system events too (a NULL entity_id joins to no
	// entity), so the explicit entity_id IS NOT NULL guard is redundant
	// but kept for clarity / plan stability.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(DISTINCT ev.entity_id)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.entity_id IS NOT NULL
		  AND `+factoryEventTrackedExists+`
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
	// Re-scoped to the tracked set (factoryTaskTrackedExists), matching
	// the belt and the event counters. Tasks RLS already bounds the count
	// to the viewer's teams, but a task minted while a repo was tracked
	// survives an untrack (tracking changes are forward-only), so without
	// the semi-join the "Triggered24h" header would count a task whose
	// entity no longer rides the belt. The entity join also drops the rare
	// task with no surviving entity.
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.event_type, COUNT(*)
		FROM tasks t
		WHERE t.org_id = $1 AND t.created_at > $2
		  AND `+factoryTaskTrackedExists+`
		GROUP BY t.event_type
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

// ActiveRuns lists the conversations the factory view treats as in flight:
// exactly those an engagement is actually driving (an unreleased claim — the
// setup sub-states ride that claim's phase). Mirrors the X-button window in
// AgentCard. Duplicated in sqlite/factory.go; intentional per-backend copy.
func (s *factoryReadStore) ActiveRuns(ctx context.Context, orgID string) ([]domain.FactoryActiveRun, error) {
	// memory_missing derivation: the agent has not produced
	// its memory file iff no conversation_memory row exists, OR the row's
	// agent_content is NULL/whitespace. BTRIM with the whitespace set
	// collapses tabs, newlines, and carriage returns onto the same
	// NULL signal so legacy empty/whitespace rows match the canonical
	// "missing" condition.
	query := `
		SELECT
			r.id, r.task_id, r.prompt_id,
			` + pgDisplayStatusSQL + `,
			COALESCE(r.model, ''), r.started_at, r.completed_at,
			(SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = r.id AND m.org_id = r.org_id),
			(SELECT SUM(cl.duration_ms)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
			(SELECT SUM(cl.num_turns)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
			COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.sdk_session_id, ''),
			(NULLIF(BTRIM(rm.agent_content, E' \t\n\r'), '') IS NULL) AS memory_missing,
			r.trigger_type, COALESCE(r.trigger_id::text, ''),
			COALESCE(r.actor_agent_id::text, ''),
			COALESCE(a.display_name, ''),
			COALESCE(r.failure_kind, ''),
			` + pgTaskColumnsWithEntity + `
		FROM conversations r
		LEFT JOIN conversation_memory rm ON rm.conversation_id = r.id AND rm.org_id = r.org_id
		LEFT JOIN agents a ON a.id = r.actor_agent_id AND a.org_id = r.org_id
		JOIN tasks t ON r.task_id = t.id AND t.org_id = r.org_id
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE r.org_id = $1 AND ` + activeClaimExistsSQL + `
		ORDER BY r.started_at DESC
	`

	rows, err := s.q.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryActiveRun
	for rows.Next() {
		var r domain.Conversation
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64
		var failureKind string

		runTargets := []any{
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&r.StopReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
			&r.ActorAgentID, &r.ActorAgentName,
			&failureKind,
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
		r.FailureKind = domain.RunFailureKind(failureKind)
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

// --- Tracked-set membership (TFAC-516) --------------------------------
//
// The multi-mode factory belt sources entity visibility from each team's
// *tracked set*, not from task existence. An entity belongs on a team's
// factory iff it sits in that team's tracked set — a GitHub entity whose
// owner/name is one of the team's tracked repos (team_github_repos), or a
// Jira entity whose project key is attached to the team
// (jira_project_status_rules). This decouples the belt from task
// creation: an untasked PR on a tracked repo now appears, where the prior
// task-existence semi-join hid it until a rule minted a task. SQLite/
// local is N=1 and stays unscoped (sqlite/factory.go).
//
// The team scoping is free under the app pool (tf_app, RLS active): the
// team_github_repos_select and jira_rules_select policies already
// constrain visible rows to the viewer's team memberships, so each EXISTS
// auto-scopes to the viewer's teams with no explicit team_id — the same
// RLS-does-the-scoping pattern ListActiveJiraTeamScoped uses
// for the Jira discovery deck. The teams join binds org (via e.org_id) as
// defense-in-depth so the filter still holds on the admin pool where RLS
// is bypassed: team_github_repos and jira_project_status_rules carry no
// org_id column, so org scope rides the teams FK.
//
// GitHub source_id is "owner/repo#N" (tracker.ghSourceID): split_part on
// '/' yields the owner, split_part on '/' then '#' yields the repo.
// Matching is case-insensitive on both axes, mirroring TracksRepoSystem
// (GitHub identifiers are case-insensitive). Jira keys are hyphen-free,
// so split_part(source_id, '-', 1) is the project key (e.g. "PROJ" in
// "PROJ-123"), matching jiraTeamProjectMembershipExists.

// factoryGitHubRepoTrackedExists scopes a github entities row (alias e)
// to the tracked repos of the viewer's teams.
const factoryGitHubRepoTrackedExists = `EXISTS (
	SELECT 1 FROM team_github_repos g
	JOIN teams tm ON tm.id = g.team_id
	WHERE tm.org_id = e.org_id
	  AND lower(g.owner) = lower(split_part(e.source_id, '/', 1))
	  AND lower(g.repo) = lower(split_part(split_part(e.source_id, '/', 2), '#', 1))
)`

// factoryJiraProjectTrackedExists scopes a jira entities row (alias e) to
// the projects attached to the viewer's teams. Mirrors
// jiraTeamProjectMembershipExists (the Jira deck's semi-join).
const factoryJiraProjectTrackedExists = `EXISTS (
	SELECT 1 FROM jira_project_status_rules jr
	JOIN teams tm ON tm.id = jr.team_id
	WHERE tm.org_id = e.org_id
	  AND jr.project_key = split_part(e.source_id, '-', 1)
)`

// factoryEntityTrackedExists is the combined tracked-set membership
// predicate correlated against an entities row (alias e): a github entity
// in a tracked repo, or a jira entity in a tracked project. Entities of
// any other source (slack, linear) have no tracked-set notion and never
// match — they are off the factory belt by construction.
const factoryEntityTrackedExists = `(
	(e.source = 'github' AND ` + factoryGitHubRepoTrackedExists + `)
	OR (e.source = 'jira' AND ` + factoryJiraProjectTrackedExists + `)
)`

// factoryGitHubRepoTrackedForTeams / factoryJiraProjectTrackedForTeams
// are the team-narrowed variants used by the per-page read filter: the
// correlated tracked-set row must additionally belong to one of the teams
// in placeholders (the comma-joined "$3, $4" list bound to team ids).
// Each branch stays RLS-scoped to the viewer under tf_app (a forged team
// id the caller isn't on is filtered by the SELECT policy) and org-bound
// via the teams join for the admin pool — the same shape as
// jiraTeamProjectMembershipForTeam, widened to an IN list.
func factoryGitHubRepoTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM team_github_repos g
		JOIN teams tm ON tm.id = g.team_id
		WHERE tm.org_id = e.org_id
		  AND g.team_id IN (` + placeholders + `)
		  AND lower(g.owner) = lower(split_part(e.source_id, '/', 1))
		  AND lower(g.repo) = lower(split_part(split_part(e.source_id, '/', 2), '#', 1))
	)`
}

func factoryJiraProjectTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM jira_project_status_rules jr
		JOIN teams tm ON tm.id = jr.team_id
		WHERE tm.org_id = e.org_id
		  AND jr.team_id IN (` + placeholders + `)
		  AND jr.project_key = split_part(e.source_id, '-', 1)
	)`
}

func factoryEntityTrackedForTeams(placeholders string) string {
	return `(
		(e.source = 'github' AND ` + factoryGitHubRepoTrackedForTeams(placeholders) + `)
		OR (e.source = 'jira' AND ` + factoryJiraProjectTrackedForTeams(placeholders) + `)
	)`
}

// factoryEventTrackedExists / factoryTaskTrackedExists lift the
// entity-correlated predicate onto an events row (alias ev) / tasks row
// (alias t) for the station-throughput aggregates, so the header counters
// report the same tracked-set population the belt renders. A system event
// (NULL entity_id) or a task whose entity falls outside the tracked set
// joins to no qualifying entity and is excluded.
const factoryEventTrackedExists = `EXISTS (
	SELECT 1 FROM entities e
	WHERE e.id = ev.entity_id AND e.org_id = ev.org_id
	  AND ` + factoryEntityTrackedExists + `
)`

const factoryTaskTrackedExists = `EXISTS (
	SELECT 1 FROM entities e
	WHERE e.id = t.entity_id AND e.org_id = t.org_id
	  AND ` + factoryEntityTrackedExists + `
)`

func (s *factoryReadStore) Entities(ctx context.Context, orgID string, limit int, teamIDs []string) ([]domain.FactoryEntityRow, error) {
	// The tracked-set semi-join scopes the belt to the viewer's teams (via
	// team_github_repos / jira_project_status_rules RLS). The optional
	// per-page filter narrows it further to a set of teams by tightening
	// that same semi-join's correlated tracked-set row to one any selected
	// team tracks. The team placeholders appear before LIMIT in the text
	// but bind by number, which Postgres resolves regardless of order —
	// active args start the teams at $3 (after orgID, limit), closed at $4
	// (after orgID, cutoff, graceLimit).
	activeMembership := factoryEntityTrackedExists
	closedMembership := factoryEntityTrackedExists
	activeArgs := []any{orgID, limit}
	closedArgs := []any{orgID, time.Now().Add(-db.FactoryClosedGracePeriod), db.FactoryClosedGraceLimit}
	if len(teamIDs) > 0 {
		activePH := make([]string, len(teamIDs))
		for i, id := range teamIDs {
			activePH[i] = fmt.Sprintf("$%d", 3+i)
			activeArgs = append(activeArgs, id)
		}
		activeMembership = factoryEntityTrackedForTeams(strings.Join(activePH, ", "))
		closedPH := make([]string, len(teamIDs))
		for i, id := range teamIDs {
			closedPH[i] = fmt.Sprintf("$%d", 4+i)
			closedArgs = append(closedArgs, id)
		}
		closedMembership = factoryEntityTrackedForTeams(strings.Join(closedPH, ", "))
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
