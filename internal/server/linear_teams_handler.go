package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The org's Linear catalog: the teams its credential can see, and each
// team's workflow states.
//
//   POST /api/orgs/{org_id}/linear/teams/list
//   GET  /api/orgs/{org_id}/linear/teams/{linear_team_id}
//   POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list
//   GET  /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/{state_id}
//
// Proxied live: a workspace's teams are a short list behind one org
// credential, and the write gate asks Linear live too (linear_team_gate.go),
// so a mirror would have nothing to earn. Both lists are proxy lists — the
// page token wraps Linear's cursor, total_count is null, and a page may come
// back shorter than page_size with a next_page_token.
//
// Every route is addressed at the org and gated on live membership of it.
// The reads run under the org's service credential, resolved on the admin
// pool through linear.Resolver, so RLS never sees them; the membership check
// is what stops a caller who has left the org, whatever org id their session
// still carries. Membership is enough: the catalog answers "where can this
// org's work come from", which every member may know, and the write that
// changes what a team tracks has its own team-admin gate.
// --------------------------------------------------------------------

// linearPageMax is the most items one upstream page carries — the client's
// page cap. A larger page_size is accepted and served short, with a
// next_page_token, the way a Jira Cloud page is: a proxy list never promises a
// full window.
const linearPageMax = 50

// linearTeamListRequest is the body: the picker's search box plus the standard
// paging pair.
type linearTeamListRequest struct {
	// Q is matched case-insensitively against the team key and name. Empty
	// matches every team the credential can see.
	Q string `json:"q"`
	httpx.PageRequest
}

// linearTeamListFilterKey is the canonical filter set a page token is
// fingerprinted against. The org is in it because the token wraps a cursor
// into one workspace's listing. Q is lowercased and trimmed because the match
// itself is case-insensitive: two spellings of one query address the same
// pages.
type linearTeamListFilterKey struct {
	OrgID string `json:"org_id"`
	Q     string `json:"q"`
}

// linearTeamJSON is one Linear team. The id is what the team write takes; key
// and name are what a person recognizes the team by; private says it is
// visible to its members only.
type linearTeamJSON struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
}

func toLinearTeamJSON(t linear.Team) linearTeamJSON {
	return linearTeamJSON{ID: t.ID, Key: t.Key, Name: t.Name, Private: t.Private}
}

// POST /api/orgs/{org_id}/linear/teams/list
func (s *Server) handleLinearTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var req linearTeamListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	q := strings.TrimSpace(req.Q)
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(linearTeamListFilterKey{OrgID: orgID, Q: strings.ToLower(q)}), 0)
	refuseLinearCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.linearSystemClient(w, r, orgID, "linear/teams", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	upstream, err := client.ListTeams(r.Context(), q, page.Cursor, min(page.Limit, linearPageMax))
	if err != nil {
		writeLinearUpstream(w, orgID, "the workspace's Linear teams", err)
		return
	}
	items := make([]linearTeamJSON, 0, len(upstream.Items))
	for _, t := range upstream.Items {
		items = append(items, toLinearTeamJSON(t))
	}
	next := ""
	if upstream.HasNextPage {
		next = upstream.EndCursor
	}
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/linear/teams/{linear_team_id}
func (s *Server) handleLinearTeamGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	teamID, ok := linearPathID(w, r, "linear_team_id", "linear team")
	if !ok {
		return
	}
	client, ok := s.linearSystemClient(w, r, orgID, "linear/team", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	team, err := client.GetTeam(r.Context(), teamID)
	if errors.Is(err, linear.ErrNotFound) {
		httpx.NotFound(w, "linear team")
		return
	}
	if err != nil {
		writeLinearUpstream(w, orgID, "Linear team "+teamID, err)
		return
	}
	writeJSON(w, http.StatusOK, toLinearTeamJSON(team))
}

// linearStateListRequest is the states list's body: the paging pair alone,
// since a team's workflow is short enough that nothing narrows it.
type linearStateListRequest struct {
	httpx.PageRequest
}

// linearStateListFilterKey fingerprints a states page token to the org and
// the team, whose listing the wrapped cursor points into.
type linearStateListFilterKey struct {
	OrgID        string `json:"org_id"`
	LinearTeamID string `json:"linear_team_id"`
}

// linearStateJSON is one workflow state. Type is Linear's own vocabulary
// (triage, backlog, unstarted, started, completed, canceled), which is what
// the settings editor pre-fills a team's rules from. Position is the board
// order; the list is in Linear's order, so a caller that wants board order
// sorts on it.
type linearStateJSON struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

func toLinearStateJSON(st linear.WorkflowState) linearStateJSON {
	return linearStateJSON{ID: st.ID, Name: st.Name, Type: st.Type, Position: st.Position}
}

// POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list
func (s *Server) handleLinearStatesList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	teamID, ok := linearPathID(w, r, "linear_team_id", "linear team")
	if !ok {
		return
	}

	var req linearStateListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(linearStateListFilterKey{OrgID: orgID, LinearTeamID: teamID}), 0)
	refuseLinearCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.linearSystemClient(w, r, orgID, "linear/states", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	upstream, err := client.ListTeamStates(r.Context(), teamID, page.Cursor, min(page.Limit, linearPageMax))
	if errors.Is(err, linear.ErrNotFound) {
		httpx.NotFound(w, "linear team")
		return
	}
	if err != nil {
		writeLinearUpstream(w, orgID, "the workflow states of Linear team "+teamID, err)
		return
	}
	items := make([]linearStateJSON, 0, len(upstream.Items))
	for _, st := range upstream.Items {
		items = append(items, toLinearStateJSON(st))
	}
	next := ""
	if upstream.HasNextPage {
		next = upstream.EndCursor
	}
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/{state_id}
//
// A state that exists but belongs to another team's workflow is a 404: the
// path names the team, and a state is addressed through the team that owns it.
func (s *Server) handleLinearStateGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	teamID, ok := linearPathID(w, r, "linear_team_id", "linear team")
	if !ok {
		return
	}
	stateID, ok := linearPathID(w, r, "state_id", "linear workflow state")
	if !ok {
		return
	}
	client, ok := s.linearSystemClient(w, r, orgID, "linear/state", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	state, owner, err := client.GetWorkflowState(r.Context(), stateID)
	if errors.Is(err, linear.ErrNotFound) || (err == nil && owner != teamID) {
		httpx.NotFound(w, "linear workflow state")
		return
	}
	if err != nil {
		writeLinearUpstream(w, orgID, "Linear workflow state "+stateID, err)
		return
	}
	writeJSON(w, http.StatusOK, toLinearStateJSON(state))
}

// refuseLinearCountOnly refuses page_size: 0. A proxy list's total is null by
// contract, so the count-only read has nothing to answer with — an empty page
// and a null total would read as a count of zero.
func refuseLinearCountOnly(v *httpx.Validation, page httpx.Page) {
	if page.CountOnly {
		v.OutOfRange("page_size", "page_size must be between 1 and 200; this list is proxied from Linear, which reports no total, so the count-only read (page_size: 0) has no answer here")
	}
}

// linearPathID reads a Linear id from the path. Anything but a Linear id is a
// 404 naming what was not found — no such resource is addressable — the way a
// malformed org id is.
func linearPathID(w http.ResponseWriter, r *http.Request, name, thing string) (string, bool) {
	id := r.PathValue(name)
	if !isLinearID(id) {
		httpx.NotFound(w, thing)
		return "", false
	}
	return id, true
}

// linearSystemClient resolves the org's Linear service credential into a
// client. No credential is a 409 NOT_CONFIGURED carrying notConfigured; a
// failed read is a 500, never "not connected", so a store outage cannot send
// an admin off to rebind a credential that is fine.
func (s *Server) linearSystemClient(w http.ResponseWriter, r *http.Request, orgID, op, notConfigured string) (*linear.Client, bool) {
	client, err := s.linearResolver.ForSystem(r.Context(), orgID)
	if errors.Is(err, linear.ErrNoLinearSystemCredential) {
		writeNotConfigured(w, notConfigured)
		return nil, false
	}
	if err != nil {
		internalError(w, op, err)
		return nil, false
	}
	return client, true
}

// writeLinearUpstream answers a Linear call that failed: 502, with the detail
// only in local mode. Nothing a caller sent caused it, and nothing it can
// change in the request fixes it.
func writeLinearUpstream(w http.ResponseWriter, orgID, what string, err error) {
	serverLog.Warn("linear read failed", "org", orgID, "reading", what, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "could not read " + what + " from Linear" + httpx.LocalDetail(err),
	})
}

// isLinearID reports whether s is a Linear entity id as Linear writes one: a
// UUID in canonical lowercase form. Other spellings uuid.Parse would accept
// (braces, a urn prefix, upper case) are refused rather than normalized, so an
// id is stored exactly as Linear hands it back.
func isLinearID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}
