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
// POST /api/linear/teams/list — the Linear team picker's option list.
// GET  /api/linear/states     — one Linear team's workflow states.
//
// The Linear siblings of POST /api/jira/projects/list and GET
// /api/jira/statuses, and proxied live for the same reason: a workspace's
// teams are a short list behind one org credential, and the write gate asks
// Linear live too (linear_team_gate.go), so a mirror would have nothing to
// earn.
//
// Both read under the org's service credential, the one the poller reads
// with, resolved through linear.Resolver so either of its shapes (a personal
// API key or an app install) serves them. A read of the org's own catalog does
// not need the caller's personal Linear identity.
// --------------------------------------------------------------------

// linearTeamPageMax is the most teams one upstream page carries — Linear's
// page cap, which the client enforces. A larger page_size is accepted and
// served short, with a next_page_token, the way a Jira Cloud page is: a proxy
// list never promises a full window.
const linearTeamPageMax = 50

// linearTeamListRequest is the body: the picker's search box plus the standard
// paging pair.
type linearTeamListRequest struct {
	// Q is matched case-insensitively against the team key and name. Empty
	// matches every team the credential can see.
	Q string `json:"q"`
	httpx.PageRequest
}

// linearTeamListFilterKey is the canonical filter set a page token is
// fingerprinted against. Lowercased and trimmed because the match itself is
// case-insensitive: two spellings of one query address the same pages.
type linearTeamListFilterKey struct {
	Q string `json:"q"`
}

// linearTeamJSON is one picker option. The id is what the PUT takes; key and
// name are what a person recognizes the team by; private says it is visible to
// its members only, which the picker shows beside it.
type linearTeamJSON struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
}

func (s *Server) handleLinearTeamsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
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
		httpx.FilterFingerprint(linearTeamListFilterKey{Q: strings.ToLower(q)}), 0)
	if page.CountOnly {
		// A proxy list's total is null by contract, so the count-only read has
		// nothing to answer with — an empty page and a null total would read as
		// a count of zero.
		v.OutOfRange("page_size", "page_size must be between 1 and 200; this list is proxied from Linear, which reports no total, so the count-only read (page_size: 0) has no answer here")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.linearSystemClient(w, r, orgID, "linear/teams", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	upstream, err := client.ListTeams(r.Context(), q, page.Cursor, min(page.Limit, linearTeamPageMax))
	if err != nil {
		writeLinearUpstream(w, orgID, "the workspace's Linear teams", err)
		return
	}
	items := make([]linearTeamJSON, 0, len(upstream.Items))
	for _, t := range upstream.Items {
		items = append(items, linearTeamJSON{ID: t.ID, Key: t.Key, Name: t.Name, Private: t.Private})
	}
	next := ""
	if upstream.HasNextPage {
		next = upstream.EndCursor
	}
	httpx.WriteProxyList(w, page, items, next)
}

// linearStateJSON is one workflow state as the API renders it. Type is
// Linear's own vocabulary (triage, backlog, unstarted, started, completed,
// canceled), which is what the picker pre-arms a team's rules from.
type linearStateJSON struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// handleLinearStates returns one Linear team's workflow states in board
// order. Unlike its Jira sibling it takes exactly one team: a Linear team owns
// its workflow outright, so there is no cross-team intersection to compute
// and no fallback to the caller's tracked set.
//
// GET /api/linear/states?team=<linear team id>
func (s *Server) handleLinearStates(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	var v httpx.Validation
	for name := range query {
		if name != "team" {
			v.Param(name, "unknown query parameter "+name+"; this read takes team")
		}
	}
	teams := query["team"]
	switch {
	case len(teams) == 0:
		v.Param("team", "team is required: the id of the Linear team whose workflow states to list")
	case len(teams) > 1:
		v.Param("team", "team takes exactly one Linear team id")
	case !isLinearID(teams[0]):
		v.Param("team", "team must be a Linear team id (a lowercase UUID)")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	teamID := teams[0]

	client, ok := s.linearSystemClient(w, r, orgID, "linear/states", "Linear is not connected for this workspace")
	if !ok {
		return
	}
	states, err := client.ListWorkflowStates(r.Context(), teamID)
	if err != nil {
		writeLinearUpstream(w, orgID, "the workflow states of Linear team "+teamID, err)
		return
	}
	out := make([]linearStateJSON, 0, len(states))
	for _, st := range states {
		out = append(out, linearStateJSON{ID: st.ID, Name: st.Name, Type: st.Type, Position: st.Position})
	}
	writeJSON(w, http.StatusOK, out)
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
