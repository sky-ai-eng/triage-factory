package server

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// GET /api/teams/{team_id}/activity?since=&until= — the team's flow node.
//
// A synthetic read per the API design rules: windowed, unpaginated, fixed
// named cuts. by_day carries the whole-team series — events, tasks, merged
// pull requests, failed runs — and by_day_source splits the first two by
// source (merged and failed have no source axis); by_source is the
// per-source totals band (events, became-tasks, runs) plus each poller's
// last completed cycle; by_tracked is the same totals per unit of the team's
// tracked set, for the source pages' per-row figures. The window grammar is the usage node's own
// (httpx.ParseWindow): RFC3339 or YYYY-MM-DD, defaulting to month-to-date,
// half-open [since, until) — so a caller asking in RFC3339 windows rows,
// not whole days.
//
// Member-gated like the team settings GET: the figures are the team page's,
// and the team page is every member's. Spend stays on /usage behind its
// admin gate — the two nodes split exactly on that audience line.
// --------------------------------------------------------------------

// teamActivityDayJSON is one UTC day. merged and failed are plain ints, not
// the by-source cut's nullable events: every team has a defined answer for
// both, so a zero here is a real zero.
type teamActivityDayJSON struct {
	Date   string `json:"date"`
	Events int    `json:"events"`
	Tasks  int    `json:"tasks"`
	Merged int    `json:"merged"`
	Failed int    `json:"failed"`
}

type teamActivityDaySourceJSON struct {
	Date   string `json:"date"`
	Source string `json:"source"`
	Events int    `json:"events"`
	Tasks  int    `json:"tasks"`
}

// teamActivitySourceJSON is one source's totals. events is null — not 0 —
// when the count is undefined for the source (Slack has no tracked set, so
// "the team's slack events" names nothing); last_poll_at is null for a
// source with no poller or one that hasn't completed a cycle. A client
// renders both as a dash, never as a zero.
type teamActivitySourceJSON struct {
	Source     string     `json:"source"`
	Events     *int       `json:"events"`
	Tasks      int        `json:"tasks"`
	Runs       int        `json:"runs"`
	LastPollAt *time.Time `json:"last_poll_at"`
}

// teamActivityTrackedJSON is one unit of the team's tracked set. unit_id is
// the id the unit's source page keys its row on — a repository's registry
// id, a jira_projects row id, a Linear team's UUID — so a client joins with
// no name lookup. A quiet unit answers zeros; last_event_at is null only for
// a unit that has never sent an event, since it is bounded by the window's
// end alone.
type teamActivityTrackedJSON struct {
	Source      string     `json:"source"`
	UnitID      string     `json:"unit_id"`
	Events      int        `json:"events"`
	Tasks       int        `json:"tasks"`
	Runs        int        `json:"runs"`
	LastEventAt *time.Time `json:"last_event_at"`
}

type teamActivityResponse struct {
	TeamID      string                      `json:"team_id"`
	ByDay       []teamActivityDayJSON       `json:"by_day"`
	ByDaySource []teamActivityDaySourceJSON `json:"by_day_source"`
	BySource    []teamActivitySourceJSON    `json:"by_source"`
	ByTracked   []teamActivityTrackedJSON   `json:"by_tracked"`
}

func (s *Server) handleTeamActivity(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := s.az.TeamIDFromPath(w, r, "teams/activity", orgID, userID)
	if !ok {
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	now := time.Now().UTC()
	since, until, errMsg := httpx.ParseWindow(r.URL.Query(), now)
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}
	// The store takes concrete bounds. An open upper side means "through
	// now"; an open lower side stays the zero time, which is below every
	// row and reads as unbounded in both dialects.
	if until.IsZero() {
		until = now
	}

	var activity domain.TeamActivity
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		activity, e = tx.TeamActivity.TeamActivity(r.Context(), orgID, teamID, since, until)
		return e
	}); err != nil {
		internalError(w, "teams/activity", err)
		return
	}
	pollTimes, err := s.allStores.PollReadiness.LastPollTimes(r.Context(), orgID)
	if err != nil {
		internalError(w, "teams/activity", err)
		return
	}

	resp := teamActivityResponse{
		TeamID:      teamID,
		ByDay:       make([]teamActivityDayJSON, 0, len(activity.ByDay)),
		ByDaySource: make([]teamActivityDaySourceJSON, 0, len(activity.ByDaySource)),
		BySource:    make([]teamActivitySourceJSON, 0, len(activity.BySource)),
		ByTracked:   make([]teamActivityTrackedJSON, 0, len(activity.ByTracked)),
	}
	for _, d := range activity.ByDay {
		resp.ByDay = append(resp.ByDay, teamActivityDayJSON{
			Date: d.Date, Events: d.Events, Tasks: d.Tasks, Merged: d.Merged, Failed: d.Failed,
		})
	}
	for _, d := range activity.ByDaySource {
		resp.ByDaySource = append(resp.ByDaySource, teamActivityDaySourceJSON{
			Date: d.Date, Source: d.Source, Events: d.Events, Tasks: d.Tasks,
		})
	}
	for _, src := range activity.BySource {
		row := teamActivitySourceJSON{Source: src.Source, Events: src.Events, Tasks: src.Tasks, Runs: src.Runs}
		if at, ok := pollTimes[src.Source]; ok {
			row.LastPollAt = &at
		}
		resp.BySource = append(resp.BySource, row)
	}
	for _, u := range activity.ByTracked {
		resp.ByTracked = append(resp.ByTracked, teamActivityTrackedJSON{
			Source: u.Source, UnitID: u.UnitID,
			Events: u.Events, Tasks: u.Tasks, Runs: u.Runs, LastEventAt: u.LastEventAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
