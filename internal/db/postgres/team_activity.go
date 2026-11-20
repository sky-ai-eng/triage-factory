package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamActivityStore is the Postgres impl of db.TeamActivityStore.
//
// A team set's events are its tracked set's events — entityTrackedForTeams,
// the predicate the entity list's team_ids filter uses — so the node and the
// entity reads agree on what a team's flow is. Tasks and runs use the task
// list's two team arms (teamActivityTaskTeamPredicate), so for a member of
// the subject teams the node's totals reconcile with the counts POST
// /api/tasks/list answers for team_ids + sources + created_since. The failed
// cut is the exception on both axes: it reaches no entity and takes no task
// predicate, scoping on the conversation's own team_id — the team that owns
// the run.
//
// A source outside teamActivityEventSources has no tracked set, so there is
// no team-scoped definition of its events: its count comes back nil
// (undefined) rather than a zero that would read as "nothing happened".
type teamActivityStore struct{ q queryer }

func newTeamActivityStore(q queryer) db.TeamActivityStore {
	return &teamActivityStore{q: q}
}

var _ db.TeamActivityStore = (*teamActivityStore)(nil)

// teamActivityEventSources are the sources for which a tracked set — and
// therefore a team event count — exists.
var teamActivityEventSources = trackedSources()

// teamActivityTaskTeamPredicate scopes a tasks row (alias t) to the subject
// teams, bound at placeholders: owned by one of them, or visible to one of
// them through task_teams while unclaimed — the same two arms as
// pgTaskTeamFilter, whose claimed-implies-owned gate keeps a claimed task
// answering only for its owning team. The list filter's tf.user_in_team gates
// are deliberately absent here: they exist there because team_ids is a
// caller-supplied narrowing that must not widen past the viewer, while these
// teams are the route's authorized subject — the handler has verified
// membership, and RLS still bounds the base rows.
func teamActivityTaskTeamPredicate(placeholders string) string {
	return ` AND (
		t.team_id IN (` + placeholders + `)
		OR (t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL
			AND EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tt.team_id IN (` + placeholders + `)))
	)`
}

// pgTeamActivityUnitKey is an entities row's (alias e) unit in its source's
// tracked set (trackedArm.unitKey). Empty for a source with no
// tracked set, which names no unit.
var pgTeamActivityUnitKey = func() string {
	var b strings.Builder
	b.WriteString("COALESCE(CASE e.source")
	for _, a := range trackedArms {
		b.WriteString(" WHEN '" + a.source + "' THEN " + a.unitKey)
	}
	b.WriteString(" END, '')")
	return b.String()
}()

// pgTeamActivityTrackedSet lists the subject teams' tracked set as distinct
// (source, unit id) rows, with $1 bound to the org and the teams at
// placeholders, one arm per trackedArm. A unit the teams track on a retired
// repository is still listed: they still track it, and its page still shows
// the row.
func pgTeamActivityTrackedSet(placeholders string) string {
	arms := make([]string, len(trackedArms))
	for i, a := range trackedArms {
		arms[i] = "SELECT '" + a.source + "', " + a.units(placeholders)
	}
	return strings.Join(arms, "\n\tUNION\n\t")
}

func (s *teamActivityStore) TeamActivity(ctx context.Context, orgID string, teamIDs []string, since, until time.Time) (domain.TeamActivity, error) {
	if len(teamIDs) == 0 {
		return db.BuildTeamActivity(db.TeamActivityCuts{EventsDefinedFor: teamActivityEventSources}), nil
	}
	sinceArg, untilArg := since.UTC(), until.UTC()
	// Every windowed query binds $1 org, $2 since, $3 until and the teams
	// after them.
	ph, teamArgs := teamPlaceholders(4, teamIDs)
	windowed := append([]any{orgID, sinceArg, untilArg}, teamArgs...)
	tracked4 := entityTrackedForTeams(ph)
	taskTeams := teamActivityTaskTeamPredicate(ph)

	// A system event (NULL entity_id) joins to no entity and drops out —
	// the node counts the flow the sources sent, not TF's own emissions.
	events, err := s.counts(ctx, `
		SELECT e.source, to_char(ev.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ev.org_id = $1 AND ev.created_at >= $2 AND ev.created_at < $3
		  AND `+tracked4+`
		GROUP BY 1, 2`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	tasks, err := s.counts(ctx, `
		SELECT e.source, to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM tasks t
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.created_at >= $2 AND t.created_at < $3`+taskTeams+`
		GROUP BY 1, 2`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Merged rides the events cut's scoping exactly — same table, same
	// tracked-set predicate — so the day a client reads as "6 merged" is a
	// subset of the events it reads beside it. It counts what GitHub told us,
	// not what TF did: a pull request merged by hand in a tracked repo counts
	// the same as one a run opened.
	merged, err := s.dayCounts(ctx, `
		SELECT to_char(ev.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ev.org_id = $1 AND ev.event_type = 'github:pr:merged'
		  AND ev.created_at >= $2 AND ev.created_at < $3
		  AND `+tracked4+`
		GROUP BY 1`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Failed is the one cut that reaches no entity: a run that died is the
	// team's fact whatever it was about, so it scopes on the conversation's
	// own owning team rather than through its task's entity, and windows on
	// completed_at — when the failure happened, not when the work started.
	failed, err := s.dayCounts(ctx, `
		SELECT to_char(c.completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM conversations c
		WHERE c.org_id = $1 AND c.status = 'failed'
		  AND c.completed_at >= $2 AND c.completed_at < $3
		  AND c.team_id IN (`+ph+`)
		GROUP BY 1`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Runs are delegated engagements: delegation conversations windowed on
	// started_at, attributed to a source through their task's entity, team-
	// scoped by the same task predicate as the tasks cut.
	runRows, err := s.q.QueryContext(ctx, `
		SELECT e.source, COUNT(*)
		FROM conversations r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE r.org_id = $1 AND r.type = 'delegation'
		  AND r.started_at >= $2 AND r.started_at < $3`+taskTeams+`
		GROUP BY 1`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer runRows.Close()
	runs, err := db.ScanTeamActivityKeyCounts(runRows)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	trackedPH, trackedArgs := teamPlaceholders(2, teamIDs)
	trackedRows, err := s.q.QueryContext(ctx, pgTeamActivityTrackedSet(trackedPH), append([]any{orgID}, trackedArgs...)...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer trackedRows.Close()
	tracked, err := db.ScanTeamActivityUnits(trackedRows)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// The tracked-set cut's events ride the events cut's predicate and
	// window, so a source's units sum to its by-source events.
	unitEvents, err := s.unitCounts(ctx, `
		SELECT e.source, `+pgTeamActivityUnitKey+`, COUNT(*)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ev.org_id = $1 AND ev.created_at >= $2 AND ev.created_at < $3
		  AND `+tracked4+`
		GROUP BY 1, 2`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	// A unit's latest event has no lower bound — it is wanted however long
	// before the window it was — so it is read per tracked entity, one probe
	// of idx_events_org_entity_created each, rather than by scanning the
	// tracked set's whole event history.
	lastPH, lastArgs := teamPlaceholders(3, teamIDs)
	lastRows, err := s.q.QueryContext(ctx, `
		SELECT e.source, `+pgTeamActivityUnitKey+`, MAX(last.created_at)
		FROM entities e
		CROSS JOIN LATERAL (
			SELECT ev.created_at FROM events ev
			WHERE ev.org_id = e.org_id AND ev.entity_id = e.id AND ev.created_at < $2
			ORDER BY ev.created_at DESC
			LIMIT 1
		) last
		WHERE e.org_id = $1
		  AND `+entityTrackedForTeams(lastPH)+`
		GROUP BY 1, 2`, append([]any{orgID, untilArg}, lastArgs...)...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer lastRows.Close()
	lastEvent := map[db.TeamActivityUnit]time.Time{}
	for lastRows.Next() {
		var u db.TeamActivityUnit
		var at time.Time
		if err := lastRows.Scan(&u.Source, &u.UnitID, &at); err != nil {
			return domain.TeamActivity{}, err
		}
		lastEvent[u] = at.UTC()
	}
	if err := lastRows.Err(); err != nil {
		return domain.TeamActivity{}, err
	}

	// Tasks and runs per unit take the tasks and runs cuts' team predicate
	// and no tracked-set gate: a task the team keeps on a unit it stopped
	// tracking still counts in by_source, and has no row here to land on —
	// the builder drops it.
	unitTasks, err := s.unitCounts(ctx, `
		SELECT e.source, `+pgTeamActivityUnitKey+`, COUNT(*)
		FROM tasks t
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.created_at >= $2 AND t.created_at < $3`+taskTeams+`
		GROUP BY 1, 2`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	unitRuns, err := s.unitCounts(ctx, `
		SELECT e.source, `+pgTeamActivityUnitKey+`, COUNT(*)
		FROM conversations r
		JOIN tasks t ON t.id = r.task_id AND t.org_id = r.org_id
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE r.org_id = $1 AND r.type = 'delegation'
		  AND r.started_at >= $2 AND r.started_at < $3`+taskTeams+`
		GROUP BY 1, 2`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// The event-type cut counts the events cut's rows and the tasks cut's
	// rows by type, so a type's figures sum across types to the by-source
	// totals.
	typeRows, err := s.q.QueryContext(ctx, `
		SELECT ev.event_type, COUNT(*), COUNT(DISTINCT ev.entity_id)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ev.org_id = $1 AND ev.created_at >= $2 AND ev.created_at < $3
		  AND `+tracked4+`
		GROUP BY 1`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer typeRows.Close()
	typeEvents, err := db.ScanTeamActivityEventTypeCounts(typeRows)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	typeTaskRows, err := s.q.QueryContext(ctx, `
		SELECT t.event_type, COUNT(*)
		FROM tasks t
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.created_at >= $2 AND t.created_at < $3`+taskTeams+`
		GROUP BY 1`, windowed...)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer typeTaskRows.Close()
	typeTasks, err := db.ScanTeamActivityKeyCounts(typeTaskRows)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	return db.BuildTeamActivity(db.TeamActivityCuts{
		Events:           events,
		Tasks:            tasks,
		Runs:             runs,
		Merged:           merged,
		Failed:           failed,
		Tracked:          tracked,
		UnitEvents:       unitEvents,
		UnitTasks:        unitTasks,
		UnitRuns:         unitRuns,
		LastEvent:        lastEvent,
		EventTypeEvents:  typeEvents,
		EventTypeTasks:   typeTasks,
		EventsDefinedFor: teamActivityEventSources,
	}), nil
}

func (s *teamActivityStore) counts(ctx context.Context, query string, args ...any) ([]db.TeamActivityCount, error) {
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.ScanTeamActivityCounts(rows)
}

func (s *teamActivityStore) dayCounts(ctx context.Context, query string, args ...any) ([]db.TeamActivityDayCount, error) {
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.ScanTeamActivityDayCounts(rows)
}

func (s *teamActivityStore) unitCounts(ctx context.Context, query string, args ...any) ([]db.TeamActivityUnitCount, error) {
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.ScanTeamActivityUnitCounts(rows)
}
