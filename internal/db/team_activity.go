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

// TeamActivityDayCount is one UTC-day cell and its count — the shape of the
// cuts with no source axis (see domain.TeamActivityDay).
type TeamActivityDayCount struct {
	Day string
	N   int
}

// TeamActivityCuts is what one dialect's queries produce and
// BuildTeamActivity assembles. A struct rather than a positional argument
// list because Merged and Failed are same-typed neighbours a call site could
// silently swap.
//
// EventsDefinedFor names the sources whose team-scoped event count is
// defined; any other source's Events comes back nil (see
// domain.TeamActivitySource). Nil means every source is defined — the
// SQLite/local case, where the team is the org and an unscoped count is the
// honest answer for every source.
type TeamActivityCuts struct {
	// Events and Tasks are per (source, UTC day); Runs is per source.
	Events []TeamActivityCount
	Tasks  []TeamActivityCount
	Runs   map[string]int
	// Merged and Failed are per UTC day alone.
	Merged []TeamActivityDayCount
	Failed []TeamActivityDayCount

	EventsDefinedFor []string
}

// BuildTeamActivity assembles the node's cuts from the aggregation queries
// both dialects run. It exists so the two impls can't drift in how the cuts
// are derived — each contributes only its own SQL.
//
// The by-source cut always carries a row for each of the catalog's external
// sources (EventSources minus "system" — system events have no entity, so no
// task or run can descend from one), plus any source the data mentions, so
// the response shape doesn't wobble with traffic.
//
// The by-day cut is the union of every day any cut mentions, not the days the
// events/tasks queries happened to find: a day whose only fact is a merged
// pull request — or a failed run — is still a day the team had, and dropping
// it would make a client's sum over by_day disagree with the store.
func BuildTeamActivity(cuts TeamActivityCuts) domain.TeamActivity {
	type cell struct{ events, tasks int }
	type dayCell struct{ events, tasks, merged, failed int }
	days := map[string]*dayCell{}
	daySources := map[[2]string]*cell{}
	sources := map[string]*cell{}
	atSource := func(k string) *cell {
		if sources[k] == nil {
			sources[k] = &cell{}
		}
		return sources[k]
	}
	atDay := func(k string) *dayCell {
		if days[k] == nil {
			days[k] = &dayCell{}
		}
		return days[k]
	}
	atDS := func(k [2]string) *cell {
		if daySources[k] == nil {
			daySources[k] = &cell{}
		}
		return daySources[k]
	}
	for _, r := range cuts.Events {
		atDay(r.Day).events += r.N
		atDS([2]string{r.Day, r.Source}).events += r.N
		atSource(r.Source).events += r.N
	}
	for _, r := range cuts.Tasks {
		atDay(r.Day).tasks += r.N
		atDS([2]string{r.Day, r.Source}).tasks += r.N
		atSource(r.Source).tasks += r.N
	}
	for _, r := range cuts.Merged {
		atDay(r.Day).merged += r.N
	}
	for _, r := range cuts.Failed {
		atDay(r.Day).failed += r.N
	}

	var out domain.TeamActivity
	for day, c := range days {
		out.ByDay = append(out.ByDay, domain.TeamActivityDay{
			Date: day, Events: c.events, Tasks: c.tasks, Merged: c.merged, Failed: c.failed,
		})
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
	for s := range cuts.Runs {
		rowSet[s] = true
	}
	names := make([]string, 0, len(rowSet))
	for s := range rowSet {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		row := domain.TeamActivitySource{Source: s, Runs: cuts.Runs[s]}
		if c := sources[s]; c != nil {
			row.Tasks = c.tasks
		}
		if cuts.EventsDefinedFor == nil || slices.Contains(cuts.EventsDefinedFor, s) {
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

// ScanTeamActivityDayCounts drains rows of (day, n) into day counts — the
// same service for the cuts with no source axis.
func ScanTeamActivityDayCounts(rows RowScanner) ([]TeamActivityDayCount, error) {
	var out []TeamActivityDayCount
	for rows.Next() {
		var c TeamActivityDayCount
		if err := rows.Scan(&c.Day, &c.N); err != nil {
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
