package server

import (
	"errors"
	"net/http"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The labels an issue in a Linear team can carry: the team's own and the
// workspace's, which Linear event predicates name by id.
//
//   POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list
//   GET  /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/{label_id}
//
// Proxied live and member-gated like the rest of the Linear catalog
// (linear_teams_handler.go). The labels are read whole and windowed here, by
// name and then id, because whether a label is a group is only known from the
// whole list: a group is a label other labels name as their parent, and a
// child can sit on any page. The list is a proxy list, so total_count is null.
// --------------------------------------------------------------------

// linearLabelListRequest is the labels list's body: the paging pair alone.
type linearLabelListRequest struct {
	httpx.PageRequest
}

// linearLabelListFilterKey fingerprints a labels page token to the org and the
// team, whose list the wrapped offset points into.
type linearLabelListFilterKey struct {
	OrgID        string `json:"org_id"`
	LinearTeamID string `json:"linear_team_id"`
}

// linearLabelJSON is one label. The id is what a predicate names it by. Group
// is the label group it sits in, null for none; is_group says it is a group
// itself, which an issue cannot carry and label_group_id_in names.
type linearLabelJSON struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Group   *linearLabelRefJSON `json:"group"`
	IsGroup bool                `json:"is_group"`
}

// linearLabelRefJSON names a label group by its id and name.
type linearLabelRefJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list
func (s *Server) handleLinearLabelsList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	teamID, ok := linearPathID(w, r, "linear_team_id", "linear team")
	if !ok {
		return
	}
	var req linearLabelListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(linearLabelListFilterKey{OrgID: orgID, LinearTeamID: teamID}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseLinearCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	labels, ok := s.readLinearTeamLabels(w, r, orgID, teamID)
	if !ok {
		return
	}
	groups := linearLabelGroupIDs(labels)
	items, next := jiraWindow(labels, startAt, page.Limit, func(l linear.Label) linearLabelJSON {
		return toLinearLabelJSON(l, groups)
	})
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/{label_id}
//
// A label that exists but belongs to another team is a 404: the path names
// the team, and a label is addressed through a team that can use it.
func (s *Server) handleLinearLabelGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	teamID, ok := linearPathID(w, r, "linear_team_id", "linear team")
	if !ok {
		return
	}
	labelID, ok := linearPathID(w, r, "label_id", "linear label")
	if !ok {
		return
	}
	labels, ok := s.readLinearTeamLabels(w, r, orgID, teamID)
	if !ok {
		return
	}
	i := slices.IndexFunc(labels, func(l linear.Label) bool { return l.ID == labelID })
	if i < 0 {
		httpx.NotFound(w, "linear label")
		return
	}
	writeJSON(w, http.StatusOK, toLinearLabelJSON(labels[i], linearLabelGroupIDs(labels)))
}

// readLinearTeamLabels reads every label an issue in teamID can carry, sorted
// by name and then id, writing the response and reporting false when it
// cannot. The team is read first: the labels read filters on the team and
// answers a team that does not exist with the workspace's labels alone.
func (s *Server) readLinearTeamLabels(w http.ResponseWriter, r *http.Request, orgID, teamID string) ([]linear.Label, bool) {
	client, ok := s.linearSystemClient(w, r, orgID, "linear/labels", "Linear is not connected for this organization")
	if !ok {
		return nil, false
	}
	if _, err := client.GetTeam(r.Context(), teamID); errors.Is(err, linear.ErrNotFound) {
		httpx.NotFound(w, "linear team")
		return nil, false
	} else if err != nil {
		writeLinearUpstream(w, orgID, "Linear team "+teamID, err)
		return nil, false
	}
	labels, err := client.ListLabels(r.Context(), teamID)
	if err != nil {
		writeLinearUpstream(w, orgID, "the labels of Linear team "+teamID, err)
		return nil, false
	}
	return labels, true
}

// linearLabelGroupIDs is the ids of the labels in labels that another label
// names as its group.
func linearLabelGroupIDs(labels []linear.Label) map[string]bool {
	groups := map[string]bool{}
	for _, l := range labels {
		if l.GroupID != "" {
			groups[l.GroupID] = true
		}
	}
	return groups
}

func toLinearLabelJSON(l linear.Label, groups map[string]bool) linearLabelJSON {
	out := linearLabelJSON{ID: l.ID, Name: l.Name, IsGroup: groups[l.ID]}
	if l.GroupID != "" {
		out.Group = &linearLabelRefJSON{ID: l.GroupID, Name: l.Group}
	}
	return out
}
