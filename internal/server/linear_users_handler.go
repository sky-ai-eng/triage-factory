package server

import (
	"errors"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The people of the org's Linear workspace, whom a Linear predicate's people
// filters (assignee_in, creator_in, commenter_in) name by Linear's user id.
//
//   POST /api/orgs/{org_id}/linear/users/list
//   GET  /api/orgs/{org_id}/linear/users/{linear_user_id}
//   GET  /api/orgs/{org_id}/linear/users/by-name/{display_name}
//
// Proxied live under the org's Linear service credential and gated on live
// membership of the org, like the team catalog (linear_teams_handler.go). The
// list is how a caller turns a name into the id a filter stores, and the read
// by display name how it turns the one name Linear keeps unique; the point
// read is how it names a stored id no team member holds.
//
// A user's email is matched by the list's search but never returned: the people
// filters need an id and a name to recognize it by, and an org member need not
// be a member of the Linear workspace.
// --------------------------------------------------------------------

// linearUserListRequest is the body: the text typed into the field plus the
// standard paging pair.
type linearUserListRequest struct {
	// Search is matched against the name, the display name and the email as a
	// case-insensitive literal substring (httpx.Search). Absent matches every
	// active user. Linear applies it upstream, and each page it serves is
	// re-checked here (readLinearScannedPage).
	Search string `json:"search" doc:"Text the user's name, display name, or email must contain, matched without regard to case; absent matches every active user."`
	httpx.PageRequest
}

// linearUserListFilterKey fingerprints a page token to the org, whose
// workspace the wrapped cursor points into, and to the search.
type linearUserListFilterKey struct {
	OrgID  string `json:"org_id"`
	Search string `json:"search"`
}

// linearUserJSON is one Linear user: the id the people filters store, the full
// name, and the display name, which Linear keeps unique within a workspace
// and so tells two people of one name apart. Active is false for a
// deactivated user, whom only the point read returns. App is true for an
// integration's app user rather than a person: one can create issues and
// comment, so creator_in and commenter_in may name it, but it is not who an
// issue is assigned to.
type linearUserJSON struct {
	ID          string `json:"id" doc:"Linear's id for the user, a UUID."`
	Name        string `json:"name" doc:"The user's full name."`
	DisplayName string `json:"display_name" doc:"The user's display name, which is unique in the workspace."`
	Active      bool   `json:"active" doc:"Whether the user is active; a deactivated user is returned only by the single-user read."`
	App         bool   `json:"app" doc:"Whether the user is an integration's app user rather than a person."`
}

var opLinearUsersList = httpx.NewListOp[linearUserListRequest, linearUserJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/linear/users/list",
	ID:          "list_linear_users",
	Summary:     "List the Linear workspace's users",
	Description: "Reads one page of the active users of the organization's Linear workspace, live from Linear. Emails are matched by search but never returned. A page may hold fewer than page_size users and still have a next_page_token; total_count is always null, and page_size 0 is refused.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

var opLinearUserGet = httpx.NewJSONOp[httpx.NoBody, linearUserJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/linear/users/{linear_user_id}",
	ID:          "get_linear_user",
	Summary:     "Get a Linear user",
	Description: "Reads one user of the organization's Linear workspace live from Linear, including a deactivated one.",
	PathParams: map[string]string{
		"org_id":         "The organization's id.",
		"linear_user_id": "Linear's id for the user, a lowercase UUID.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

func toLinearUserJSON(u linear.User) linearUserJSON {
	return linearUserJSON{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Active: u.Active, App: u.App}
}

// POST /api/orgs/{org_id}/linear/users/list
func (s *Server) handleLinearUsersList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	req, ok := opLinearUsersList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	search := httpx.Search(&v, req.Search)
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(linearUserListFilterKey{OrgID: orgID, Search: httpx.SearchKey(search)}), 0)
	refuseLinearCountOnly(&v, page)
	from := httpx.ScanCursorFrom(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.linearSystemClient(w, r, orgID, "linear/users", "Linear is not connected for this organization")
	if !ok {
		return
	}
	users, next, err := readLinearScannedPage(from, page.Limit, search,
		func(u linear.User) []string { return []string{u.Name, u.DisplayName, u.Email} },
		func(after string) ([]linear.User, bool, string, error) {
			p, err := client.ListUsers(r.Context(), search, after, linearPageMax)
			return p.Items, p.HasNextPage, p.EndCursor, err
		})
	if err != nil {
		writeLinearUpstream(w, orgID, "the workspace's Linear users", err)
		return
	}
	items := make([]linearUserJSON, 0, len(users))
	for _, u := range users {
		items = append(items, toLinearUserJSON(u))
	}
	opLinearUsersList.WriteScannedProxy(w, page, items, next)
}

// GET /api/orgs/{org_id}/linear/users/{linear_user_id}
func (s *Server) handleLinearUserGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	userID, ok := linearPathID(w, r, "linear_user_id", "linear user")
	if !ok {
		return
	}
	client, ok := s.linearSystemClient(w, r, orgID, "linear/user", "Linear is not connected for this organization")
	if !ok {
		return
	}
	user, err := client.GetUser(r.Context(), userID)
	if errors.Is(err, linear.ErrNotFound) {
		httpx.NotFound(w, "linear user")
		return
	}
	if err != nil {
		writeLinearUpstream(w, orgID, "Linear user "+userID, err)
		return
	}
	opLinearUserGet.Write(w, toLinearUserJSON(user))
}

var opLinearUserGetByName = httpx.NewJSONOp[httpx.NoBody, linearUserJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/linear/users/by-name/{display_name}",
	ID:          "get_linear_user_by_name",
	Summary:     "Get a Linear user by display name",
	Description: "Reads the user of the organization's Linear workspace, deactivated or not, whose display name is exactly the one given, matched with case, live from Linear. Linear keeps display names unique within a workspace; a display name it answers for more than one user is a 409.",
	PathParams: map[string]string{
		"org_id":       "The organization's id.",
		"display_name": "The user's display name.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// GET /api/orgs/{org_id}/linear/users/by-name/{display_name}
func (s *Server) handleLinearUserGetByName(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	displayName, ok := linearPathName(w, r, "display_name", "linear user")
	if !ok {
		return
	}
	client, ok := s.linearSystemClient(w, r, orgID, "linear/user", "Linear is not connected for this organization")
	if !ok {
		return
	}
	user, err := client.GetUserByDisplayName(r.Context(), displayName)
	if errors.Is(err, linear.ErrNotFound) {
		httpx.NotFound(w, "linear user")
		return
	}
	if errors.Is(err, linear.ErrAmbiguous) {
		writeLinearAmbiguous(w, "More than one Linear user has the display name "+displayName+"; read the user by their id.")
		return
	}
	if err != nil {
		writeLinearUpstream(w, orgID, "the Linear user "+displayName, err)
		return
	}
	opLinearUserGetByName.Write(w, toLinearUserJSON(user))
}
