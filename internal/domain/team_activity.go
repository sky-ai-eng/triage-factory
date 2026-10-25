package domain

import "time"

// TeamActivity is the store-side payload of the team activity node — the
// windowed synthetic read GET /api/teams/{team_id}/activity. Cuts, not rows:
// each slice is a fixed named aggregation over the same window, per the
// synthetic-reads rule in the API design section of CLAUDE.md.
type TeamActivity struct {
	ByDay       []TeamActivityDay
	ByDaySource []TeamActivityDaySource
	BySource    []TeamActivitySource
	ByTracked   []TeamActivityTracked
}

// TeamActivityDay is one UTC calendar day's flow across every source:
// events recorded, tasks created, pull requests merged, and runs that ended
// failed. The four are counted over the same window but not the same rows —
// merged is an event-log fact (a PR in the team's tracked set merged, whether
// or not TF touched it), failed a conversation fact (a run this team owns
// reached the failed terminal). Both are plain ints: every team has a defined
// answer, unlike TeamActivitySource.Events.
//
// Merged and Failed have no source axis and so appear here only, never on
// TeamActivityDaySource: merged is GitHub's alone, and a failed run names no
// source at all.
type TeamActivityDay struct {
	Date   string // YYYY-MM-DD (UTC)
	Events int
	Tasks  int
	Merged int
	Failed int
}

// TeamActivityDaySource is one (UTC day, source) cell — the long-format
// series a client pivots into a per-source stacked chart, mirroring the
// usage node's by_day_model.
type TeamActivityDaySource struct {
	Date   string
	Source string
	Events int
	Tasks  int
}

// TeamActivitySource is one source's totals over the window. Events is a
// pointer because a count can be undefined rather than zero: a team's
// events are defined by its tracked set, and a source with no tracked-set
// notion (Slack — push-delivered, no repo/project attachment) has no
// team-scoped event count to report. Nil means "not defined", which a UI
// renders as a dash, never as 0.
//
// LastPollAt is the source's poller's last completed cycle — instantaneous
// state riding the windowed node the way /proc files mix series and now.
// Nil for a source with no poller (Slack) or one that has never completed.
// The store leaves it nil; the handler stitches it in from poll_readiness,
// which lives on the admin pool.
type TeamActivitySource struct {
	Source     string
	Events     *int
	Tasks      int
	Runs       int
	LastPollAt *time.Time
}

// TeamActivityTracked is one unit of the team's tracked set and its flow over
// the window: a GitHub repository (UnitID is its registry row id), a Jira
// project (its jira_projects row id) or a Linear team (its Linear UUID) —
// the ids the source pages key their rows on. A tracked unit that was quiet
// in the window is a row of zeros; a unit the team does not track has no row.
// Slack has no tracked set, so it has no rows.
//
// Events, Tasks and Runs are counted the way the by-source cut counts them,
// so a source's rows sum to its TeamActivitySource. The two part only on
// activity attached to a unit outside the tracked set, which has no row to
// land on: tasks and runs a team keeps on a unit it stopped tracking, and,
// in local mode, whose by-source cut is unscoped, events on such a unit too.
//
// LastEventAt is the unit's latest event before the window's end, however
// long before its start — nil only for a unit that has never sent one.
type TeamActivityTracked struct {
	Source      string
	UnitID      string
	Events      int
	Tasks       int
	Runs        int
	LastEventAt *time.Time
}
