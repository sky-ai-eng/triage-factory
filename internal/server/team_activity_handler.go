package server

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// GET /api/teams/{team_id}/activity?since=&until= — the team's flow node, and
// GET /api/me/activity?since=&until= — the same node over the union of the
// viewer's teams.
//
// A synthetic read per the API design rules: windowed, unpaginated, fixed
// named cuts, the same cuts at both scopes. by_day carries the whole series —
// events, tasks, merged pull requests, failed runs — and by_day_source splits
// the first two by source (merged and failed have no source axis); by_source
// is the per-source totals band (events, became-tasks, runs) plus each
// poller's last completed cycle; by_tracked is the same totals per unit of the
// tracked set, for the source pages' per-row figures; by_event_type is events,
// tasks, and distinct entities per event type. The window grammar is the usage
// node's own (httpx.ParseWindow): RFC3339 or YYYY-MM-DD, defaulting to
// month-to-date, half-open [since, until) — so a caller asking in RFC3339
// windows rows, not whole days, and one passing only until reads with no
// lower bound.
//
// The team node is member-gated like the team settings GET: the figures are
// the team page's, and the team page is every member's. The viewer node is
// viewer-relative, so its teams are the viewer's own and it needs no gate
// beyond org membership. Spend stays on /usage behind its admin gate — the two
// nodes split exactly on that audience line.
// --------------------------------------------------------------------

// teamActivityDayJSON is one UTC day. merged and failed are plain ints, not
// the by-source cut's nullable events: every team has a defined answer for
// both, so a zero here is a real zero.
type teamActivityDayJSON struct {
	Date   string `json:"date" doc:"The UTC calendar day, as YYYY-MM-DD."`
	Events int    `json:"events" doc:"How many events were recorded on the day, across every source."`
	Tasks  int    `json:"tasks" doc:"How many tasks were created on the day."`
	Merged int    `json:"merged" doc:"How many pull requests in the tracked repositories merged on the day, whether or not Triage Factory worked on them."`
	Failed int    `json:"failed" doc:"How many of the teams' runs failed on the day."`
}

type teamActivityDaySourceJSON struct {
	Date   string `json:"date" doc:"The UTC calendar day, as YYYY-MM-DD."`
	Source string `json:"source" doc:"The source the events and tasks came from, such as github or jira."`
	Events int    `json:"events" doc:"How many events from the source were recorded on the day."`
	Tasks  int    `json:"tasks" doc:"How many tasks from the source were created on the day."`
}

// teamActivitySourceJSON is one source's totals. events is null — not 0 —
// when the count is undefined for the source (one with no tracked set, for
// which "the team's events" names nothing); last_poll_at is null for a
// source with no poller or one that hasn't completed a cycle. A client
// renders both as a dash, never as a zero.
type teamActivitySourceJSON struct {
	Source     string     `json:"source" doc:"The source, such as github or jira."`
	Events     *int       `json:"events" doc:"How many events from the source were recorded in the window, or null when the source has no tracked set to count events by."`
	Tasks      int        `json:"tasks" doc:"How many tasks from the source were created in the window."`
	Runs       int        `json:"runs" doc:"How many runs on tasks from the source started in the window."`
	LastPollAt *time.Time `json:"last_poll_at" doc:"When the source's poller last completed a cycle, as an RFC 3339 timestamp, or null for a source with no poller or one that has not completed a cycle."`
}

// teamActivityTrackedJSON is one unit of the team's tracked set. unit_id is
// the id the unit's source page keys its row on — a repository's registry
// id, a jira_projects row id, a Linear team's UUID, a slack_channels row id —
// so a client joins with
// no name lookup. A quiet unit answers zeros; last_event_at is null only for
// a unit that has never sent an event, since it is bounded by the window's
// end alone.
type teamActivityTrackedJSON struct {
	Source      string     `json:"source" doc:"The source the tracked unit belongs to, such as github or jira."`
	UnitID      string     `json:"unit_id" doc:"The tracked unit's id: a repository's id, a Jira project's id, a Linear team's id, or a Slack channel's id, as the source's own routes name it."`
	Events      int        `json:"events" doc:"How many events from the unit were recorded in the window."`
	Tasks       int        `json:"tasks" doc:"How many tasks on the unit were created in the window."`
	Runs        int        `json:"runs" doc:"How many runs on the unit's tasks started in the window."`
	LastEventAt *time.Time `json:"last_event_at" doc:"When the unit's latest event before the window's end was recorded, however long before the window's start, as an RFC 3339 timestamp, or null when it has never sent one."`
}

// teamActivityEventTypeJSON is one event type's flow: the events of the type,
// the tasks created at it, and the distinct entities that sent such an event.
type teamActivityEventTypeJSON struct {
	EventType string `json:"event_type" doc:"The event type, such as github:pr:review_requested."`
	Events    int    `json:"events" doc:"How many events of the type were recorded in the window."`
	Tasks     int    `json:"tasks" doc:"How many tasks were created for events of the type in the window."`
	Entities  int    `json:"entities" doc:"How many distinct entities sent an event of the type in the window."`
}

// activityCutsJSON is the node's cuts, the same at every scope that mounts it.
type activityCutsJSON struct {
	ByDay       []teamActivityDayJSON       `json:"by_day" doc:"The events, tasks, merged pull requests, and failed runs of each UTC day in the window."`
	ByDaySource []teamActivityDaySourceJSON `json:"by_day_source" doc:"The events and tasks of each UTC day in the window, by source."`
	BySource    []teamActivitySourceJSON    `json:"by_source" doc:"The events, tasks, and runs of each source over the window, with when its poller last completed a cycle."`
	ByTracked   []teamActivityTrackedJSON   `json:"by_tracked" doc:"The events, tasks, and runs over the window of each repository, Jira project, Linear team, and Slack channel the teams track, including those with none."`
	ByEventType []teamActivityEventTypeJSON `json:"by_event_type" doc:"The events, tasks, and distinct entities of each event type over the window."`
}

type teamActivityResponse struct {
	TeamID string `json:"team_id" doc:"The team's id."`
	activityCutsJSON
}

// meActivityResponse names the teams whose union the cuts are, so a client
// can tell a viewer on no team (an empty list, and zeros) from a quiet one.
type meActivityResponse struct {
	TeamIDs []string `json:"team_ids" doc:"The ids of the caller's teams whose combined activity the cuts count, an empty array when the caller is on no team."`
	activityCutsJSON
}

var opTeamActivity = httpx.NewJSONOp[httpx.NoBody, teamActivityResponse](httpx.OpSpec{
	Pattern:     "GET /api/teams/{team_id}/activity",
	ID:          "get_team_activity",
	Summary:     "Get a team's activity",
	Description: "Returns the team's events, tasks, runs, merged pull requests, and failed runs over the window, by UTC day, source, tracked unit, and event type, with when each source's poller last completed a cycle.",
	PathParams:  map[string]string{"team_id": "The team's id, or default for the one team in local mode."},
	Query:       httpx.WindowQuery(),
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

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
	q, ok := opTeamActivity.Query(w, r)
	if !ok {
		return
	}
	cuts, ok := s.activityCuts(w, r, q, orgID, userID, func(db.TxStores) ([]string, error) {
		return []string{teamID}, nil
	})
	if !ok {
		return
	}
	opTeamActivity.Write(w, teamActivityResponse{TeamID: teamID, activityCutsJSON: cuts.cuts})
}

var opMeActivity = httpx.NewJSONOp[httpx.NoBody, meActivityResponse](httpx.OpSpec{
	Pattern:     "GET /api/me/activity",
	ID:          "get_my_activity",
	Summary:     "Get the activity of your teams",
	Description: "Returns the combined events, tasks, runs, merged pull requests, and failed runs over the window of the caller's teams in their current organization, by UTC day, source, tracked unit, and event type, counting an event once however many of the teams track it.",
	Query:       httpx.WindowQuery(),
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleMeActivity is the node over the union of the viewer's active teams in
// the org: an event counts once however many of them track its entity.
func (s *Server) handleMeActivity(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	q, ok := opMeActivity.Query(w, r)
	if !ok {
		return
	}
	cuts, ok := s.activityCuts(w, r, q, orgID, userID, func(tx db.TxStores) ([]string, error) {
		// The team set decides the answer rather than being served as rows, so
		// it is read whole.
		teams, _, err := tx.Teams.ListForUser(r.Context(), orgID, db.Unwindowed)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(teams))
		for i, t := range teams {
			ids[i] = t.ID
		}
		return ids, nil
	})
	if !ok {
		return
	}
	opMeActivity.Write(w, meActivityResponse{TeamIDs: cuts.teamIDs, activityCutsJSON: cuts.cuts})
}

// activityRead is one read of the node: the teams it unions and their cuts.
type activityRead struct {
	teamIDs []string
	cuts    activityCutsJSON
}

// activityCuts parses the window from q, the route's query, reads the node for
// the teams teamsOf names under the viewer's claims, and renders the cuts. It
// writes the failure and answers false when the read cannot be served.
func (s *Server) activityCuts(w http.ResponseWriter, r *http.Request, q httpx.QueryValues, orgID, userID string, teamsOf func(db.TxStores) ([]string, error)) (activityRead, bool) {
	now := time.Now().UTC()
	var v httpx.Validation
	since, until := httpx.ParseWindow(&v, q, now)
	if v.Flush(w, http.StatusBadRequest) {
		return activityRead{}, false
	}
	// The store takes concrete bounds. An open upper side means "through
	// now"; an open lower side stays the zero time, which is below every
	// row and reads as unbounded in both dialects.
	if until.IsZero() {
		until = now
	}

	var read activityRead
	var activity domain.TeamActivity
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if read.teamIDs, e = teamsOf(tx); e != nil {
			return e
		}
		activity, e = tx.TeamActivity.TeamActivity(r.Context(), orgID, read.teamIDs, since, until)
		return e
	}); err != nil {
		internalError(w, "activity", err)
		return activityRead{}, false
	}
	pollTimes, err := s.allStores.PollReadiness.LastPollTimes(r.Context(), orgID)
	if err != nil {
		internalError(w, "activity", err)
		return activityRead{}, false
	}

	cuts := activityCutsJSON{
		ByDay:       make([]teamActivityDayJSON, 0, len(activity.ByDay)),
		ByDaySource: make([]teamActivityDaySourceJSON, 0, len(activity.ByDaySource)),
		BySource:    make([]teamActivitySourceJSON, 0, len(activity.BySource)),
		ByTracked:   make([]teamActivityTrackedJSON, 0, len(activity.ByTracked)),
		ByEventType: make([]teamActivityEventTypeJSON, 0, len(activity.ByEventType)),
	}
	for _, d := range activity.ByDay {
		cuts.ByDay = append(cuts.ByDay, teamActivityDayJSON{
			Date: d.Date, Events: d.Events, Tasks: d.Tasks, Merged: d.Merged, Failed: d.Failed,
		})
	}
	for _, d := range activity.ByDaySource {
		cuts.ByDaySource = append(cuts.ByDaySource, teamActivityDaySourceJSON{
			Date: d.Date, Source: d.Source, Events: d.Events, Tasks: d.Tasks,
		})
	}
	for _, src := range activity.BySource {
		row := teamActivitySourceJSON{Source: src.Source, Events: src.Events, Tasks: src.Tasks, Runs: src.Runs}
		if at, ok := pollTimes[src.Source]; ok {
			row.LastPollAt = &at
		}
		cuts.BySource = append(cuts.BySource, row)
	}
	for _, u := range activity.ByTracked {
		cuts.ByTracked = append(cuts.ByTracked, teamActivityTrackedJSON{
			Source: u.Source, UnitID: u.UnitID,
			Events: u.Events, Tasks: u.Tasks, Runs: u.Runs, LastEventAt: u.LastEventAt,
		})
	}
	for _, et := range activity.ByEventType {
		cuts.ByEventType = append(cuts.ByEventType, teamActivityEventTypeJSON{
			EventType: et.EventType, Events: et.Events, Tasks: et.Tasks, Entities: et.Entities,
		})
	}
	if read.teamIDs == nil {
		read.teamIDs = []string{}
	}
	read.cuts = cuts
	return read, true
}

func (s *Server) registerTeamActivityRoutes() {
	// The team's flow node — events/tasks/runs cuts over a window, the
	// spend node's sibling on the same member-level gate: both are the team
	// page's figures, and the team page is every member's.
	s.op(opTeamActivity, s.handleTeamActivity)
	// The same node over the viewer's own teams, viewer-relative like
	// /api/me/usage.
	s.op(opMeActivity, s.handleMeActivity)
}
