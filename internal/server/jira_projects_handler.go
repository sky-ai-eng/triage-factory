package server

import (
	"cmp"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The org's Jira catalog: the projects its credential can see, and each
// project's workflow statuses.
//
//   POST /api/orgs/{org_id}/jira/projects/list
//   GET  /api/orgs/{org_id}/jira/projects/{project_key}
//   POST /api/orgs/{org_id}/jira/projects/{project_key}/statuses/list
//   GET  /api/orgs/{org_id}/jira/projects/{project_key}/statuses/{status_id}
//
// The GitHub sibling (POST /api/github/repos/list) reads a local mirror; these
// proxy Jira live, and the difference is a property of the two estates rather
// than a preference. Reachability on GitHub is computed across credential
// classes over estates that run to thousands behind tight rate limits, and the
// write gate consumes the mirror locally. Jira has one org credential and a
// catalog of dozens that arrives in one call, and its write gate asks Jira live
// too (jira_project_gate.go) — so nothing would read a mirror that a live call
// doesn't serve, and there is nothing for one to earn.
//
// Both lists are proxy lists: total_count is null, and next_page_token wraps an
// offset — into the upstream catalog for projects, and into the one response
// Jira serves a project's statuses in for statuses. See httpx.WriteProxyList.
//
// Every route is addressed at the org and gated on live membership of it. The
// reads run under the org's Jira service credential — the same one the poller
// reads with — resolved on the admin pool through jira.Resolver, so RLS never
// sees them; the membership check is what stops a caller who has left the org,
// whatever org id their session still carries. Membership is enough: the
// catalog answers "where can this org's work come from", which every member
// may know, and the write that changes what a team tracks has its own
// team-admin gate. Nor does a read of the org's own catalog need the caller's
// personal Jira identity: requiring one would make the picker unusable for
// exactly the members who have not bound theirs yet.
// --------------------------------------------------------------------

// jiraProjectListRequest is the body: the picker's search box plus the
// standard paging pair.
type jiraProjectListRequest struct {
	// Q is matched against the project key and the project name, case
	// insensitively. Empty matches everything. Cloud applies it upstream;
	// Data Center, whose catalog endpoint takes no filter, applies it to the
	// catalog in hand — one contract, two implementations (jira.ListProjects).
	Q string `json:"q"`
	httpx.PageRequest
}

// jiraProjectListFilterKey is the canonicalized filter set the page token is
// fingerprinted against, so a token minted for one search cannot address
// another's results. The org is in it because the token is an offset into one
// instance's catalog. Q is lowercased and trimmed because the match itself is
// case-insensitive on both deployments: two spellings of one query are one
// query, and a token minted under either addresses the same page.
type jiraProjectListFilterKey struct {
	OrgID string `json:"org_id"`
	Q     string `json:"q"`
}

// jiraProjectJSON is one project. Key and name and nothing else: a project
// object from either deployment carries avatars, lead, category and a self
// link, none of which the picker renders — and a proxy list that echoes
// whatever the upstream sent is how a provider's shape becomes our contract.
type jiraProjectJSON struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// POST /api/orgs/{org_id}/jira/projects/list
func (s *Server) handleJiraProjectsList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var req jiraProjectListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	q := strings.TrimSpace(req.Q)
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraProjectListFilterKey{OrgID: orgID, Q: strings.ToLower(q)}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.jiraSystemClient(w, r, orgID, "jira/projects")
	if !ok {
		return
	}
	upstream, err := client.ListProjects(r.Context(), q, startAt, page.Limit)
	if err != nil {
		writeJiraUpstream(w, orgID, "the workspace's Jira projects", err)
		return
	}
	items := make([]jiraProjectJSON, 0, len(upstream.Projects))
	for _, p := range upstream.Projects {
		items = append(items, jiraProjectJSON{Key: p.Key, Name: p.Name})
	}
	next := ""
	if upstream.NextStartAt > 0 {
		next = strconv.Itoa(upstream.NextStartAt)
	}
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/jira/projects/{project_key}
func (s *Server) handleJiraProjectGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	key, ok := jiraPathProjectKey(w, r)
	if !ok {
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/project")
	if !ok {
		return
	}
	project, err := client.GetProject(r.Context(), key)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "Jira project "+key, err)
		return
	}
	writeJSON(w, http.StatusOK, jiraProjectJSON{Key: project.Key, Name: project.Name})
}

// jiraStatusListRequest is the statuses list's body: the paging pair alone,
// since a project's workflow is short enough that nothing narrows it.
type jiraStatusListRequest struct {
	httpx.PageRequest
}

// jiraStatusListFilterKey fingerprints a statuses page token to the org and
// the project, whose statuses the wrapped offset points into.
type jiraStatusListFilterKey struct {
	OrgID      string `json:"org_id"`
	ProjectKey string `json:"project_key"`
}

// jiraStatusJSON is one workflow status. The id is what the team write takes
// and what a rule is keyed on; the name is what a person recognizes it by.
type jiraStatusJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// POST /api/orgs/{org_id}/jira/projects/{project_key}/statuses/list
//
// Jira serves a project's statuses in one unpaged response, grouped by issue
// type, which the client unions (jira.ProjectStatuses). The window is applied
// here to that list, ordered by name and then id so an offset addresses the
// same rows on every read.
func (s *Server) handleJiraStatusesList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	key, ok := jiraPathProjectKey(w, r)
	if !ok {
		return
	}

	var req jiraStatusListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraStatusListFilterKey{OrgID: orgID, ProjectKey: key}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.jiraSystemClient(w, r, orgID, "jira/statuses")
	if !ok {
		return
	}
	statuses, err := client.ProjectStatuses(r.Context(), key)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the workflow statuses of Jira project "+key, err)
		return
	}
	slices.SortFunc(statuses, func(a, b jira.Status) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	start := min(startAt, len(statuses))
	end := min(start+page.Limit, len(statuses))
	items := make([]jiraStatusJSON, 0, end-start)
	for _, st := range statuses[start:end] {
		items = append(items, jiraStatusJSON{ID: st.ID, Name: st.Name})
	}
	next := ""
	if end < len(statuses) {
		next = strconv.Itoa(end)
	}
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/jira/projects/{project_key}/statuses/{status_id}
//
// Jira has no read for one status of one project — /status/{id} answers
// instance-wide and says nothing about which workflows use it — so this reads
// the project's statuses and picks the one asked for. That also makes it
// answer for exactly the rows the list serves and the team write accepts. A
// status that exists but is not in this project's workflow is a 404: the path
// names the project, and a status is addressed through the project that uses
// it.
func (s *Server) handleJiraStatusGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	key, ok := jiraPathProjectKey(w, r)
	if !ok {
		return
	}
	statusID := r.PathValue("status_id")
	if !jiraStatusIDRe.MatchString(statusID) {
		httpx.NotFound(w, "jira workflow status")
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/status")
	if !ok {
		return
	}
	statuses, err := client.ProjectStatuses(r.Context(), key)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the workflow statuses of Jira project "+key, err)
		return
	}
	i := slices.IndexFunc(statuses, func(st jira.Status) bool { return st.ID == statusID })
	if i < 0 {
		httpx.NotFound(w, "jira workflow status")
		return
	}
	writeJSON(w, http.StatusOK, jiraStatusJSON{ID: statuses[i].ID, Name: statuses[i].Name})
}

// jiraStatusIDRe is a Jira status id as both deployments write one: a decimal
// integer, carried as a string.
var jiraStatusIDRe = regexp.MustCompile(`^[0-9]+$`)

// jiraStartAtFromCursor decodes the offset the proxy page token carries. An
// empty cursor is the first page. Anything else must be a non-negative integer
// — the token is opaque and only this server mints one, so a value that isn't
// is a tampered or stale token, not a position.
func jiraStartAtFromCursor(v *httpx.Validation, cursor string) int {
	if cursor == "" {
		return 0
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidParam,
			Message: "page_token is not a valid page token",
			Field:   "page_token",
		})
		return 0
	}
	return n
}

// refuseJiraCountOnly refuses page_size: 0. A proxy list's total is null by
// contract, so the count-only read has nothing to answer with — an empty page
// and a null total would read as a count of zero.
func refuseJiraCountOnly(v *httpx.Validation, page httpx.Page) {
	if page.CountOnly {
		v.OutOfRange("page_size", "page_size must be between 1 and 200; this list is proxied from Jira, which reports no total, so the count-only read (page_size: 0) has no answer here")
	}
}

// jiraPathProjectKey reads a project key from the path. A key outside the
// grammar the team write enforces is a 404 — no such project is addressable —
// and is refused as sent rather than normalized, so one project has one
// address.
func jiraPathProjectKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("project_key")
	if !jiraProjectKeyRe.MatchString(key) {
		httpx.NotFound(w, "jira project")
		return "", false
	}
	return key, true
}

// jiraSystemClient resolves the org's Jira service credential into a client.
// No credential is a 409 NOT_CONFIGURED; a failed read is a 500, never "not
// connected", so a store outage cannot send an admin off to rebind a
// credential that is fine.
func (s *Server) jiraSystemClient(w http.ResponseWriter, r *http.Request, orgID, op string) (*jira.Client, bool) {
	client, err := s.jiraResolver.ForSystem(r.Context(), orgID)
	if errors.Is(err, jira.ErrNoJiraSystemCredential) {
		writeNotConfigured(w, "Jira is not connected for this workspace")
		return nil, false
	}
	if err != nil {
		internalError(w, op, err)
		return nil, false
	}
	return client, true
}

// writeJiraUpstream answers a Jira call that failed: 502, with the detail only
// in local mode. Nothing a caller sent caused it, and nothing it can change in
// the request fixes it.
func writeJiraUpstream(w http.ResponseWriter, orgID, what string, err error) {
	serverLog.Warn("jira read failed", "org", orgID, "reading", what, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "could not read " + what + " from Jira" + httpx.LocalDetail(err),
	})
}
