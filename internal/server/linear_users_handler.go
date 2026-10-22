package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The people of the org's Linear workspace, whom a Linear predicate's people
// filters (assignee_in, creator_in, commenter_in) name by Linear's user id.
//
//   POST /api/orgs/{org_id}/linear/users/list
//   GET  /api/orgs/{org_id}/linear/users/{linear_user_id}
//
// Proxied live under the org's Linear service credential and gated on live
// membership of the org, like the team catalog (linear_teams_handler.go). The
// list is how a caller turns a name into the id a filter stores; the point
// read is how it names a stored id no team member holds.
//
// A user's email is matched by the list's q but never returned: the people
// filters need an id and a name to recognize it by, and an org member need not
// be a member of the Linear workspace.
// --------------------------------------------------------------------

// linearUserListRequest is the body: the text typed into the field plus the
// standard paging pair.
type linearUserListRequest struct {
	// Q is matched case-insensitively against the name, the display name and
	// the email. Empty matches every active user.
	Q string `json:"q"`
	httpx.PageRequest
}

// linearUserListFilterKey fingerprints a page token to the org, whose
// workspace the wrapped cursor points into, and to the query, lowercased and
// trimmed because the match is case-insensitive.
type linearUserListFilterKey struct {
	OrgID string `json:"org_id"`
	Q     string `json:"q"`
}

// linearUserJSON is one Linear user: the id the people filters store, the full
// name, and the display name, which Linear keeps unique within a workspace
// and so tells two people of one name apart. Active is false for a
// deactivated user, whom only the point read returns. App is true for an
// integration's app user rather than a person: one can create issues and
// comment, so creator_in and commenter_in may name it, but it is not who an
// issue is assigned to.
type linearUserJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Active      bool   `json:"active"`
	App         bool   `json:"app"`
}

func toLinearUserJSON(u linear.User) linearUserJSON {
	return linearUserJSON{ID: u.ID, Name: u.Name, DisplayName: u.DisplayName, Active: u.Active, App: u.App}
}

// POST /api/orgs/{org_id}/linear/users/list
func (s *Server) handleLinearUsersList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var req linearUserListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	q := strings.TrimSpace(req.Q)
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(linearUserListFilterKey{OrgID: orgID, Q: strings.ToLower(q)}), 0)
	refuseLinearCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.linearSystemClient(w, r, orgID, "linear/users", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	upstream, err := client.ListUsers(r.Context(), q, page.Cursor, min(page.Limit, linearPageMax))
	if err != nil {
		writeLinearUpstream(w, orgID, "the workspace's Linear users", err)
		return
	}
	items := make([]linearUserJSON, 0, len(upstream.Items))
	for _, u := range upstream.Items {
		items = append(items, toLinearUserJSON(u))
	}
	next := ""
	if upstream.HasNextPage {
		next = upstream.EndCursor
	}
	httpx.WriteProxyList(w, page, items, next)
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
	client, ok := s.linearSystemClient(w, r, orgID, "linear/user", "Linear is not connected for this workspace")
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
	writeJSON(w, http.StatusOK, toLinearUserJSON(user))
}
