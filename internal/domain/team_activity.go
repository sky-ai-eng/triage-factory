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
}

// TeamActivityDay is one UTC calendar day's flow across every source:
// events recorded and tasks created.
type TeamActivityDay struct {
	Date   string // YYYY-MM-DD (UTC)
	Events int
	Tasks  int
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
