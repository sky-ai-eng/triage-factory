package postgres

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamActivityStore is the Postgres impl of db.TeamActivityStore.
//
// A team's events are its tracked set's events — the same
// factoryEntityTrackedForTeams predicate the belt's per-team filter uses —
// so the node and the factory agree on what a team's flow is. Tasks and
// runs use the task list's two team arms (teamActivityTaskTeamPredicate),
// so for a member of the subject team the node's totals reconcile with the
// counts POST /api/tasks/list answers for team_ids + sources +
// created_since. The failed cut is the exception on both axes: it reaches no
// entity and takes no task predicate, scoping on the conversation's own
// team_id — the team that owns the run.
//
// Slack is deliberately absent from teamActivityEventSources: with no
// tracked set there is no team-scoped definition of "a slack event", so its
// count comes back nil (undefined) rather than a zero that would read as
// "nothing happened".
type teamActivityStore struct{ q queryer }

func newTeamActivityStore(q queryer) db.TeamActivityStore {
	return &teamActivityStore{q: q}
}

var _ db.TeamActivityStore = (*teamActivityStore)(nil)

// teamActivityEventSources mirrors factoryEntityTrackedExists's arms: the
// sources for which a tracked set — and therefore a team event count —
// exists.
var teamActivityEventSources = []string{"github", "jira"}

// teamActivityTaskTeamPredicate scopes a tasks row (alias t) to the subject
// team, bound at $4: owned by it, or visible to it through task_teams while
// unclaimed — the same two arms as pgTaskTeamFilter, whose claimed-implies-
// owned gate keeps a claimed task answering only for its owning team. The
// list filter's tf.user_in_team gates are deliberately absent here: they
// exist there because team_ids is a caller-supplied narrowing that must not
// widen past the viewer, while this team is the route's authorized subject —
// the handler has verified membership, and RLS still bounds the base rows.
const teamActivityTaskTeamPredicate = ` AND (
		t.team_id = $4
		OR (t.claimed_by_agent_id IS NULL AND t.claimed_by_user_id IS NULL
			AND EXISTS (SELECT 1 FROM task_teams tt WHERE tt.task_id = t.id AND tt.team_id = $4))
	)`

func (s *teamActivityStore) TeamActivity(ctx context.Context, orgID, teamID string, since, until time.Time) (domain.TeamActivity, error) {
	sinceArg, untilArg := since.UTC(), until.UTC()

	// A system event (NULL entity_id) joins to no entity and drops out —
	// the node counts the flow the sources sent, not TF's own emissions.
	events, err := s.counts(ctx, `
		SELECT e.source, to_char(ev.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ev.org_id = $1 AND ev.created_at >= $2 AND ev.created_at < $3
		  AND `+factoryEntityTrackedForTeams("$4")+`
		GROUP BY 1, 2`, orgID, sinceArg, untilArg, teamID)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	tasks, err := s.counts(ctx, `
		SELECT e.source, to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, COUNT(*)
		FROM tasks t
		JOIN entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.created_at >= $2 AND t.created_at < $3`+teamActivityTaskTeamPredicate+`
		GROUP BY 1, 2`, orgID, sinceArg, untilArg, teamID)
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
		  AND `+factoryEntityTrackedForTeams("$4")+`
		GROUP BY 1`, orgID, sinceArg, untilArg, teamID)
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
		  AND c.team_id = $4
		GROUP BY 1`, orgID, sinceArg, untilArg, teamID)
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
		  AND r.started_at >= $2 AND r.started_at < $3`+teamActivityTaskTeamPredicate+`
		GROUP BY 1`, orgID, sinceArg, untilArg, teamID)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	defer runRows.Close()
	runs := map[string]int{}
	for runRows.Next() {
		var src string
		var n int
		if err := runRows.Scan(&src, &n); err != nil {
			return domain.TeamActivity{}, err
		}
		runs[src] = n
	}
	if err := runRows.Err(); err != nil {
		return domain.TeamActivity{}, err
	}

	return db.BuildTeamActivity(db.TeamActivityCuts{
		Events:           events,
		Tasks:            tasks,
		Runs:             runs,
		Merged:           merged,
		Failed:           failed,
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
