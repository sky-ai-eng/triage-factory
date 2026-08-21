package db

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TeamActivityStore computes the team activity node's cuts — the windowed
// synthetic read behind GET /api/teams/{team_id}/activity.
//
// Scoping follows the standing entity-read rule: on Postgres a team's events
// are the tracked-set semi-join (the factory belt's gate — a github entity in
// one of the team's tracked repos, a jira entity in one of its attached
// projects), and its tasks and runs ride the task list's team predicate, so
// the node's totals reconcile with the counts the task list answers for the
// same filters. SQLite/local is N=1 and stays unscoped. Read it under the
// caller's claims (TxStores) — the Postgres predicates lean on tf.user_in_team
// and RLS, so the Stores-level admin binding exists for wiring symmetry, not
// for serving requests.
type TeamActivityStore interface {
	// TeamActivity computes the cuts for one team over [since, until).
	// LastPollAt on the by-source cut is left nil — poll times live in
	// poll_readiness on the admin pool, and the handler stitches them in.
	TeamActivity(ctx context.Context, orgID, teamID string, since, until time.Time) (domain.TeamActivity, error)
}

// TeamActivityCount is one aggregation row a dialect hands to
// BuildTeamActivity: a (source, UTC day) cell and its count. Day is empty on
// rows from queries that group by source alone.
type TeamActivityCount struct {
	Source string
	Day    string
	N      int
}

// BuildTeamActivity assembles the node's cuts from the three aggregation
// queries both dialects run: events per (source, day), tasks per (source,
// day), and delegation runs per source. It exists so the two impls can't
// drift in how the cuts are derived — each contributes only its own SQL.
//
// eventsDefinedFor names the sources whose team-scoped event count is
// defined; any other source's Events comes back nil (see
// domain.TeamActivitySource). Nil means every source is defined — the
// SQLite/local case, where the team is the org and an unscoped count is the
// honest answer for every source.
//
// The by-source cut always carries a row for each of the catalog's external
// sources (EventSources minus "system" — system events have no entity, so no
// task or run can descend from one), plus any source the data mentions, so
// the response shape doesn't wobble with traffic.
func BuildTeamActivity(events, tasks []TeamActivityCount, runs map[string]int, eventsDefinedFor []string) domain.TeamActivity {
	type cell struct{ events, tasks int }
	days := map[string]*cell{}
	daySources := map[[2]string]*cell{}
	sources := map[string]*cell{}
	at := func(m map[string]*cell, k string) *cell {
		if m[k] == nil {
			m[k] = &cell{}
		}
		return m[k]
	}
	atDS := func(k [2]string) *cell {
		if daySources[k] == nil {
			daySources[k] = &cell{}
		}
		return daySources[k]
	}
	for _, r := range events {
		at(days, r.Day).events += r.N
		atDS([2]string{r.Day, r.Source}).events += r.N
		at(sources, r.Source).events += r.N
	}
	for _, r := range tasks {
		at(days, r.Day).tasks += r.N
		atDS([2]string{r.Day, r.Source}).tasks += r.N
		at(sources, r.Source).tasks += r.N
	}

	var out domain.TeamActivity
	for day, c := range days {
		out.ByDay = append(out.ByDay, domain.TeamActivityDay{Date: day, Events: c.events, Tasks: c.tasks})
	}
	sort.Slice(out.ByDay, func(i, j int) bool { return out.ByDay[i].Date < out.ByDay[j].Date })
	for k, c := range daySources {
		out.ByDaySource = append(out.ByDaySource, domain.TeamActivityDaySource{Date: k[0], Source: k[1], Events: c.events, Tasks: c.tasks})
	}
	sort.Slice(out.ByDaySource, func(i, j int) bool {
		a, b := out.ByDaySource[i], out.ByDaySource[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		return a.Source < b.Source
	})

	rowSet := map[string]bool{}
	for _, s := range domain.EventSources() {
		if s != "system" {
			rowSet[s] = true
		}
	}
	for s := range sources {
		rowSet[s] = true
	}
	for s := range runs {
		rowSet[s] = true
	}
	names := make([]string, 0, len(rowSet))
	for s := range rowSet {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		row := domain.TeamActivitySource{Source: s, Runs: runs[s]}
		if c := sources[s]; c != nil {
			row.Tasks = c.tasks
		}
		if eventsDefinedFor == nil || slices.Contains(eventsDefinedFor, s) {
			n := 0
			if c := sources[s]; c != nil {
				n = c.events
			}
			row.Events = &n
		}
		out.BySource = append(out.BySource, row)
	}
	return out
}

// scanTeamActivityCounts drains rows of (source, day, n) into counts —
// shared by both dialects' grouped queries.
func ScanTeamActivityCounts(rows RowScanner) ([]TeamActivityCount, error) {
	var out []TeamActivityCount
	for rows.Next() {
		var c TeamActivityCount
		if err := rows.Scan(&c.Source, &c.Day, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RowScanner is the slice of *sql.Rows the scan helpers need — an interface
// so this package doesn't take a database/sql dependency for one helper.
type RowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}
