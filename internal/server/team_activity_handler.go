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
// when the count is undefined for the source (one with no tracked set, for
// which "the team's events" names nothing); last_poll_at is null for a
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
// id, a jira_projects row id, a Linear team's UUID, a slack_channels row id —
// so a client joins with
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

// teamActivityEventTypeJSON is one event type's flow: the events of the type,
// the tasks created at it, and the distinct entities that sent such an event.
type teamActivityEventTypeJSON struct {
	EventType string `json:"event_type"`
	Events    int    `json:"events"`
	Tasks     int    `json:"tasks"`
	Entities  int    `json:"entities"`
}

// activityCutsJSON is the node's cuts, the same at every scope that mounts it.
type activityCutsJSON struct {
	ByDay       []teamActivityDayJSON       `json:"by_day"`
	ByDaySource []teamActivityDaySourceJSON `json:"by_day_source"`
	BySource    []teamActivitySourceJSON    `json:"by_source"`
	ByTracked   []teamActivityTrackedJSON   `json:"by_tracked"`
	ByEventType []teamActivityEventTypeJSON `json:"by_event_type"`
}

type teamActivityResponse struct {
	TeamID string `json:"team_id"`
	activityCutsJSON
}

// meActivityResponse names the teams whose union the cuts are, so a client
// can tell a viewer on no team (an empty list, and zeros) from a quiet one.
type meActivityResponse struct {
	TeamIDs []string `json:"team_ids"`
	activityCutsJSON
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
	cuts, ok := s.activityCuts(w, r, orgID, userID, func(db.TxStores) ([]string, error) {
		return []string{teamID}, nil
	})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, teamActivityResponse{TeamID: teamID, activityCutsJSON: cuts.cuts})
}

// handleMeActivity is the node over the union of the viewer's active teams in
// the org: an event counts once however many of them track its entity.
func (s *Server) handleMeActivity(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	cuts, ok := s.activityCuts(w, r, orgID, userID, func(tx db.TxStores) ([]string, error) {
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
	writeJSON(w, http.StatusOK, meActivityResponse{TeamIDs: cuts.teamIDs, activityCutsJSON: cuts.cuts})
}

// activityRead is one read of the node: the teams it unions and their cuts.
type activityRead struct {
	teamIDs []string
	cuts    activityCutsJSON
}

// activityCuts parses the window, reads the node for the teams teamsOf names
// under the viewer's claims, and renders the cuts. It writes the failure and
// answers false when the read cannot be served.
func (s *Server) activityCuts(w http.ResponseWriter, r *http.Request, orgID, userID string, teamsOf func(db.TxStores) ([]string, error)) (activityRead, bool) {
	now := time.Now().UTC()
	q, ok := httpx.StrictQuery(w, r)
	if !ok {
		return activityRead{}, false
	}
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
	s.api("GET /api/teams/{team_id}/activity", s.handleTeamActivity)
	// The same node over the viewer's own teams, viewer-relative like
	// /api/me/usage.
	s.api("GET /api/me/activity", s.handleMeActivity)
}
