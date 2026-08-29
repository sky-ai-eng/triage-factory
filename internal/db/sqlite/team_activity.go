package sqlite

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamActivityStore is the SQLite impl of db.TeamActivityStore. Local mode
// is N=1 — the team is the org — so every count is unscoped (the same
// posture as sqlite/factory.go) and every source's event count is defined,
// which is why the cuts leave EventsDefinedFor nil.
type teamActivityStore struct{ q queryer }

func newTeamActivityStore(q queryer) db.TeamActivityStore {
	return &teamActivityStore{q: q}
}

var _ db.TeamActivityStore = (*teamActivityStore)(nil)

func (s *teamActivityStore) TeamActivity(ctx context.Context, orgID, _ string, since, until time.Time) (domain.TeamActivity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.TeamActivity{}, err
	}
	sinceArg := since.UTC()
	untilArg := until.UTC()

	// julianday() on both sides, not the datetime() the task list's windows
	// use: datetime() truncates to whole seconds, and this node's upper
	// bound is usually server-computed "now", so a row written in the same
	// second as the request would fall out of a window it is genuinely
	// inside. julianday keeps SQLite's full parsed precision instead —
	// milliseconds, which is where its date parser stops — so the residual
	// edge is a row sharing "now"'s exact millisecond, invisible next
	// refetch. The bounds bind as time.Time so the driver serializes them
	// the same way it serialized the stored column. The entity join is
	// what names each row's source; a system event (NULL entity_id) joins
	// to nothing and drops out, which is correct — the node counts the
	// flow the sources sent, not TF's own bookkeeping emissions.
	events, err := s.countsByDay(ctx, `
		SELECT e.source, strftime('%Y-%m-%d', ev.created_at) AS day, COUNT(*)
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id
		WHERE julianday(ev.created_at) >= julianday(?) AND julianday(ev.created_at) < julianday(?)
		GROUP BY 1, 2`, sinceArg, untilArg)
	if err != nil {
		return domain.TeamActivity{}, err
	}
	tasks, err := s.countsByDay(ctx, `
		SELECT e.source, strftime('%Y-%m-%d', t.created_at) AS day, COUNT(*)
		FROM tasks t
		JOIN entities e ON e.id = t.entity_id
		WHERE julianday(t.created_at) >= julianday(?) AND julianday(t.created_at) < julianday(?)
		GROUP BY 1, 2`, sinceArg, untilArg)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Merged rides the events cut — same table, same window grammar — so the
	// day a client reads as "6 merged" is a subset of the events it reads
	// beside it. No entity join: the event type already names github, and the
	// cut has no source axis to fill. It counts what GitHub told us, not what
	// TF did — a pull request merged by hand counts the same as one a run
	// opened.
	merged, err := s.dayCounts(ctx, `
		SELECT strftime('%Y-%m-%d', ev.created_at) AS day, COUNT(*)
		FROM events ev
		WHERE ev.event_type = 'github:pr:merged'
		  AND julianday(ev.created_at) >= julianday(?) AND julianday(ev.created_at) < julianday(?)
		GROUP BY 1`, sinceArg, untilArg)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Failed windows on completed_at — when the run died, not when it started.
	failed, err := s.dayCounts(ctx, `
		SELECT strftime('%Y-%m-%d', c.completed_at) AS day, COUNT(*)
		FROM conversations c
		WHERE c.status = 'failed'
		  AND julianday(c.completed_at) >= julianday(?) AND julianday(c.completed_at) < julianday(?)
		GROUP BY 1`, sinceArg, untilArg)
	if err != nil {
		return domain.TeamActivity{}, err
	}

	// Runs are delegated engagements: delegation conversations, windowed on
	// started_at, attributed to a source through their task's entity.
	runRows, err := s.q.QueryContext(ctx, `
		SELECT e.source, COUNT(*)
		FROM conversations r
		JOIN tasks t ON t.id = r.task_id
		JOIN entities e ON e.id = t.entity_id
		WHERE r.type = 'delegation'
		  AND julianday(r.started_at) >= julianday(?) AND julianday(r.started_at) < julianday(?)
		GROUP BY 1`, sinceArg, untilArg)
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
		Events: events,
		Tasks:  tasks,
		Runs:   runs,
		Merged: merged,
		Failed: failed,
	}), nil
}

func (s *teamActivityStore) countsByDay(ctx context.Context, query string, args ...any) ([]db.TeamActivityCount, error) {
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
