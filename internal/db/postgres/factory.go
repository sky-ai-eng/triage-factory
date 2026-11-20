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
	// Scoped to what the belt can carry (factoryEventOnBelt). The station
	// header's other counters — Triggered24h (tasks) and ActiveConversations
	// — are team-scoped because tasks/conversations RLS is team-bound;
	// events RLS is org-wide (events_all keys on org_id only), so without
	// this an event on a PR outside the viewer's tracked set would inflate
	// this team's "items at station" count even though that PR never
	// appears on the belt.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(*)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.created_at > $2
		  AND `+factoryEventOnBelt+`
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
	// Scoped to what the belt can carry, same as EventCountsSince — a
	// lifetime distinct-entity count must not include entities outside the
	// viewer's tracked set, or the station's lifetime readout reports PRs
	// absent from the belt. The semi-join inherently drops system events too
	// (a NULL entity_id joins to no entity), so the explicit entity_id IS NOT
	// NULL guard is redundant but kept for clarity / plan stability.
	rows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(DISTINCT ev.entity_id)
		FROM events ev
		WHERE ev.org_id = $1 AND ev.entity_id IS NOT NULL
		  AND `+factoryEventOnBelt+`
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
	// Scoped to what the belt can carry (factoryTaskOnBelt), matching the
	// event counters. Tasks RLS already bounds the count
	// to the viewer's teams, but a task minted while a repo was tracked
	// survives an untrack (tracking changes are forward-only), so without
	// the semi-join the "Triggered24h" header would count a task whose
	// entity no longer rides the belt. The entity join also drops the rare
	// task with no surviving entity.
	rows, err := s.q.QueryContext(ctx, `
		SELECT t.event_type, COUNT(*)
		FROM tasks t
		WHERE t.org_id = $1 AND t.created_at > $2
		  AND `+factoryTaskOnBelt+`
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

// ActiveConversations lists the conversations the factory view treats as in flight:
// exactly those an engagement is actually driving (an unreleased claim — the
// setup sub-states ride that claim's phase — that is not an idle warm one).
// Mirrors the X-button window in AgentCard. Duplicated in sqlite/factory.go;
// intentional per-backend copy.
func (s *factoryReadStore) ActiveConversations(ctx context.Context, orgID string) ([]domain.FactoryActiveConversation, error) {
	// memory_missing derivation: the agent has not produced its memory file
	// iff the conversation has no conversation_memory row whose source is
	// `agent`. Same predicate the conversation read uses (pgConversationColumns
	// carries the reasoning) — the belt and the run station answer the same
	// question about the same row, so they resolve it the same way.
	query := `
		SELECT
			r.id, r.task_id, r.prompt_id,
			` + pgDisplayStatusSQL + `,
			COALESCE(r.model, ''), r.started_at, r.completed_at,
			(SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = r.id AND m.org_id = r.org_id),
			(SELECT SUM(cl.duration_ms)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
			(SELECT SUM(cl.num_turns)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
			COALESCE(r.park_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.sdk_session_id, ''),
			(rm.source IS DISTINCT FROM 'agent') AS memory_missing,
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
		WHERE r.org_id = $1 AND ` + workingClaimExistsSQL + `
		ORDER BY r.started_at DESC
	`

	rows, err := s.q.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryActiveConversation
	for rows.Next() {
		var r domain.Conversation
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64
		var failureKind, parkReason string

		convTargets := []any{
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&parkReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
			&r.ActorAgentID, &r.ActorAgentName,
			&failureKind,
		}
		var ts taskScanState
		if err := rows.Scan(append(convTargets, ts.targets(&t)...)...); err != nil {
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
		r.FailureKind = domain.ConversationFailureKind(failureKind)
		r.ParkReason = domain.ParkReason(parkReason)
		out = append(out, domain.FactoryActiveConversation{Conversation: r, Task: t, EntityEventTyp: t.EventType})
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
	// Tie-break on seq, insertion order: events often share an event_at,
	// since GitHub reports source times to the second.
	query := `
		SELECT entity_id, event_type, event_at, detected_at
		FROM (
			SELECT entity_id, event_type,
				COALESCE(occurred_at, created_at) AS event_at,
				created_at AS detected_at,
				seq,
				ROW_NUMBER() OVER (
					PARTITION BY entity_id
					ORDER BY COALESCE(occurred_at, created_at) DESC, seq DESC
				) AS rn
			FROM events
			WHERE org_id = $1 AND entity_id = ANY($2)
		) ranked
		WHERE rn <= $3
		ORDER BY entity_id, event_at ASC, seq ASC
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
	COALESCE(e.repository_id::text, ''),
	(SELECT event_type FROM events WHERE org_id = e.org_id AND entity_id = e.id ORDER BY created_at DESC LIMIT 1),
	(SELECT created_at FROM events WHERE org_id = e.org_id AND entity_id = e.id ORDER BY created_at DESC LIMIT 1)
`

// --- Tracked-set membership ------------------------------------------
//
// In multi mode an entity belongs to a team's flow iff it sits in that
// team's tracked set: a GitHub entity whose repository is one of the team's
// tracked repos (team_github_repos), a Jira entity whose project is attached
// to the team (jira_project_status_rules), a Linear entity whose Linear team
// the team tracks (linear_team_rules), or a Slack thread in a channel the
// team watches (team_slack_channels). Whether a task was ever minted on the
// entity does not enter it. The belt, the station counters and the team
// activity node all read it. SQLite/local is N=1 and stays unscoped
// (sqlite/factory.go).
//
// The team scoping is free under the app pool (tf_app, RLS active): the
// team_github_repos_select, jira_rules_select, linear_rules_select and
// team_slack_channels_select policies already constrain visible rows to the
// viewer's team memberships, so each EXISTS auto-scopes to the viewer's
// teams with no explicit team_id — the same RLS-does-the-scoping pattern
// ListActiveJiraTeamScoped uses for the Jira discovery deck. The org bind
// (via e.org_id) is defense-in-depth so the filter still holds on the admin
// pool where RLS is bypassed: team_github_repos, jira_project_status_rules
// and team_slack_channels bind it on their own org_id, linear_team_rules
// through the teams join.

// factoryGitHubRepoTrackedExists scopes a github entities row (alias e)
// to the tracked repos of the viewer's teams.
//
// The entity and the tracking row both reference the repository's registry
// row, so they meet on its id. A rename moves only the registry row's name,
// and a registry row lives on one GitHub host, so a pull request matches
// tracking only on its own host without the host being compared.
const factoryGitHubRepoTrackedExists = `EXISTS (
	SELECT 1 FROM team_github_repos g
	WHERE g.org_id = e.org_id
	  AND g.repository_id = e.repository_id
)`

// factoryJiraProjectTrackedExists scopes a jira entities row (alias e) to
// the projects attached to the viewer's teams. Mirrors
// jiraTeamProjectMembershipExists (the Jira deck's semi-join): the entity
// and the rule both reference the project's registry row, so they meet on its
// id, which a project key rename does not move, and the column outlives the
// snapshot, so a row stays in its teams' tracked set while its source is
// turned off.
const factoryJiraProjectTrackedExists = `EXISTS (
	SELECT 1 FROM jira_project_status_rules jr
	WHERE jr.org_id = e.org_id
	  AND jr.jira_project_id = e.jira_project_id
)`

// factoryLinearTeamTrackedExists scopes a linear entities row (alias e) to
// the Linear teams attached to the viewer's teams. The entity's Linear team
// is entities.linear_team_id, the UUID linear_team_rules is keyed by — not
// the identifier's key prefix, which a team key rename in Linear changes. The
// column outlives the snapshot, so a row stays in its teams' tracked set while
// its source is turned off. Only rules saved under the org's current Linear
// workspace count, as everywhere else rules are read: a rule from a workspace
// the org has moved off names a team no current issue belongs to.
const factoryLinearTeamTrackedExists = `EXISTS (
	SELECT 1 FROM linear_team_rules lr
	JOIN teams tm ON tm.id = lr.team_id
	WHERE tm.org_id = e.org_id
	  AND lr.linear_team_id = e.linear_team_id
	  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = e.org_id)
)`

// factorySlackChannelTrackedExists scopes a slack entities row (alias e) to
// the channels the viewer's teams watch. A thread entity references its
// channel's registry row (entities.slack_channel_id), the row a
// team_slack_channels row binds a team to, so the two meet on its id, which a
// channel id change in Slack does not move. Every team watching the channel
// matches, primary or not: the primary flag decides whose task a mention
// becomes, not whose flow the channel's traffic is.
const factorySlackChannelTrackedExists = `EXISTS (
	SELECT 1 FROM team_slack_channels sc
	WHERE sc.org_id = e.org_id
	  AND sc.slack_channel_id = e.slack_channel_id
)`

// factoryGitHubRepoTrackedForTeams / factoryJiraProjectTrackedForTeams /
// factoryLinearTeamTrackedForTeams / factorySlackChannelTrackedForTeams are
// the team-narrowed variants: the correlated tracked-set row must
// additionally belong to one of the teams in placeholders (the comma-joined
// "$3, $4" list bound to team ids). Each branch stays RLS-scoped to the
// viewer under tf_app (a forged team id the caller isn't on is filtered by
// the SELECT policy) and org-bound for the admin pool the way its unnarrowed
// twin is — the same shape as jiraTeamProjectMembershipForTeam, widened to
// an IN list.
func factoryGitHubRepoTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM team_github_repos g
		WHERE g.org_id = e.org_id
		  AND g.team_id IN (` + placeholders + `)
		  AND g.repository_id = e.repository_id
	)`
}

func factoryJiraProjectTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM jira_project_status_rules jr
		WHERE jr.org_id = e.org_id
		  AND jr.team_id IN (` + placeholders + `)
		  AND jr.jira_project_id = e.jira_project_id
	)`
}

func factoryLinearTeamTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM linear_team_rules lr
		JOIN teams tm ON tm.id = lr.team_id
		WHERE tm.org_id = e.org_id
		  AND lr.team_id IN (` + placeholders + `)
		  AND lr.linear_team_id = e.linear_team_id
		  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = e.org_id)
	)`
}

func factorySlackChannelTrackedForTeams(placeholders string) string {
	return `EXISTS (
		SELECT 1 FROM team_slack_channels sc
		WHERE sc.org_id = e.org_id
		  AND sc.team_id IN (` + placeholders + `)
		  AND sc.slack_channel_id = e.slack_channel_id
	)`
}

// factoryTrackedArm is one source's arm of the tracked set. Every read
// scoped by the tracked set builds its SQL from factoryTrackedArms, so a
// source gains a tracked set by gaining a row there.
type factoryTrackedArm struct {
	source   string
	exists   string
	forTeams func(placeholders string) string
	// unitKey is the entities column naming the row's unit, the column the
	// source's tracking rows reference.
	unitKey string
	// units selects a team's unit ids, bound at $1 (org) and $2 (team), with
	// the same filters as exists, so a unit listed is exactly a unit whose
	// entities exists admits.
	units string
}

var factoryTrackedArms = []factoryTrackedArm{
	{
		source:   "github",
		exists:   factoryGitHubRepoTrackedExists,
		forTeams: factoryGitHubRepoTrackedForTeams,
		unitKey:  `e.repository_id::text`,
		units:    `g.repository_id::text FROM team_github_repos g WHERE g.org_id = $1 AND g.team_id = $2`,
	},
	{
		source:   "jira",
		exists:   factoryJiraProjectTrackedExists,
		forTeams: factoryJiraProjectTrackedForTeams,
		unitKey:  `e.jira_project_id::text`,
		units:    `jr.jira_project_id::text FROM jira_project_status_rules jr WHERE jr.org_id = $1 AND jr.team_id = $2`,
	},
	{
		source:   "linear",
		exists:   factoryLinearTeamTrackedExists,
		forTeams: factoryLinearTeamTrackedForTeams,
		unitKey:  `e.linear_team_id`,
		units: `lr.linear_team_id FROM linear_team_rules lr
	JOIN teams tm ON tm.id = lr.team_id
	WHERE tm.org_id = $1 AND lr.team_id = $2
	  AND lr.linear_workspace_id = (SELECT os.linear_workspace_id FROM org_settings os WHERE os.org_id = $1)`,
	},
	{
		source:   "slack",
		exists:   factorySlackChannelTrackedExists,
		forTeams: factorySlackChannelTrackedForTeams,
		unitKey:  `e.slack_channel_id::text`,
		units:    `sc.slack_channel_id::text FROM team_slack_channels sc WHERE sc.org_id = $1 AND sc.team_id = $2`,
	},
}

// factoryTrackedSQL ORs one SQL fragment per arm, each behind its source.
// An entity of a source with no arm never matches.
func factoryTrackedSQL(fragment func(factoryTrackedArm) string) string {
	arms := make([]string, len(factoryTrackedArms))
	for i, a := range factoryTrackedArms {
		arms[i] = "(e.source = '" + a.source + "' AND " + fragment(a) + ")"
	}
	return "(" + strings.Join(arms, " OR ") + ")"
}

// factoryEntityTrackedExists matches an entities row (alias e) that any of
// the viewer's teams tracks.
var factoryEntityTrackedExists = factoryTrackedSQL(func(a factoryTrackedArm) string { return a.exists })

// factoryEntityTrackedForTeams matches an entities row (alias e) that one of
// the teams in placeholders tracks.
func factoryEntityTrackedForTeams(placeholders string) string {
	return factoryTrackedSQL(func(a factoryTrackedArm) string { return a.forTeams(placeholders) })
}

// factoryTrackedSources names the sources that have a tracked set.
func factoryTrackedSources() []string {
	out := make([]string, len(factoryTrackedArms))
	for i, a := range factoryTrackedArms {
		out[i] = a.source
	}
	return out
}

// teamPlaceholders numbers one bind placeholder per team id from start
// ("$3, $4, …") and returns them with the ids as bind arguments.
func teamPlaceholders(start int, teamIDs []string) (string, []any) {
	ph := make([]string, len(teamIDs))
	args := make([]any, len(teamIDs))
	for i, id := range teamIDs {
		ph[i] = fmt.Sprintf("$%d", start+i)
		args[i] = id
	}
	return strings.Join(ph, ", "), args
}

// factoryOnBelt matches an entities row (alias e) the belt can carry: one of
// db.FactoryBeltKind in the viewer's tracked set. The station counters read
// it too, so a station's numbers count only what its chips can be.
var factoryOnBelt = `e.kind = '` + db.FactoryBeltKind + `' AND ` + factoryEntityTrackedExists

// factoryEventOnBelt / factoryTaskOnBelt lift factoryOnBelt onto an events
// row (alias ev) / tasks row (alias t) for the station counters. A system
// event (NULL entity_id) or a task whose entity is off the belt joins to no
// qualifying entity and is excluded.
var factoryEventOnBelt = `EXISTS (
	SELECT 1 FROM entities e
	WHERE e.id = ev.entity_id AND e.org_id = ev.org_id
	  AND ` + factoryOnBelt + `
)`

var factoryTaskOnBelt = `EXISTS (
	SELECT 1 FROM entities e
	WHERE e.id = t.entity_id AND e.org_id = t.org_id
	  AND ` + factoryOnBelt + `
)`

func (s *factoryReadStore) Entities(ctx context.Context, orgID string, limit int, teamIDs []string) ([]domain.FactoryEntityRow, error) {
	// The tracked set scopes the belt to the viewer's teams through its
	// tracking tables' RLS. The optional per-page filter narrows it further
	// to a set of teams by tightening each arm's correlated tracking row to
	// one any selected team holds. The team placeholders appear before LIMIT
	// in the text but bind by number, which Postgres resolves regardless of
	// order — active args start the teams at $3 (after orgID, limit), closed
	// at $4 (after orgID, cutoff, FactoryClosedGraceLimit).
	activeMembership := factoryEntityTrackedExists
	closedMembership := factoryEntityTrackedExists
	activeArgs := []any{orgID, limit}
	closedArgs := []any{orgID, time.Now().UTC().Add(-db.FactoryClosedGracePeriod), db.FactoryClosedGraceLimit}
	if len(teamIDs) > 0 {
		ph, ids := teamPlaceholders(3, teamIDs)
		activeMembership = factoryEntityTrackedForTeams(ph)
		activeArgs = append(activeArgs, ids...)
		ph, ids = teamPlaceholders(4, teamIDs)
		closedMembership = factoryEntityTrackedForTeams(ph)
		closedArgs = append(closedArgs, ids...)
	}

	active, err := queryFactoryEntities(ctx, s.q, `
		SELECT `+pgFactoryEntitySelectColumns+`
		FROM entities e
		WHERE e.org_id = $1 AND e.state = 'active'
		  AND e.kind = '`+db.FactoryBeltKind+`'
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
		  AND e.kind = '`+db.FactoryBeltKind+`'
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
			&row.Entity.RepositoryID,
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
