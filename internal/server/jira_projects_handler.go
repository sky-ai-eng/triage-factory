package server

import (
	"cmp"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The org's Jira catalog: the projects its credential can see, and each
// project's workflow statuses.
//
//   POST /api/orgs/{org_id}/jira/projects/list
//   GET  /api/orgs/{org_id}/jira/projects/{jira_id}
//   GET  /api/orgs/{org_id}/jira/projects/by-name/{key}
//   POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list
//   GET  /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/{status_id}
//
// A project is addressed by Jira's own project id (jira_id), which a key
// rename leaves alone, and which the team write names a project being added
// by. Its key is served for display.
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
	// Search is matched against the project key and the project name as a
	// case-insensitive literal substring (httpx.Search). Absent matches every
	// project. Cloud applies it upstream as /project/search's `query`, which
	// Jira matches as a literal string, so a % or _ in it is a character and
	// not a wildcard; Data Center, whose catalog endpoint takes no filter,
	// applies it to the catalog in hand before the window — one contract, two
	// implementations (jira.ListProjects).
	Search string `json:"search" doc:"Text the project's key or name must contain, matched without regard to case; absent matches every project."`
	httpx.PageRequest
}

// jiraProjectListFilterKey is the canonicalized filter set the page token is
// fingerprinted against, so a token minted for one search cannot address
// another's results. The org is in it because the token is an offset into one
// instance's catalog.
type jiraProjectListFilterKey struct {
	OrgID  string `json:"org_id"`
	Search string `json:"search"`
}

// jiraProjectJSON is one project: Jira's id for it, which is what a client
// names it by, and its key and name for display. Nothing else: a project
// object from either deployment carries avatars, lead, category and a self
// link, none of which the picker renders — and a proxy list that echoes
// whatever the upstream sent is how a provider's shape becomes our contract.
type jiraProjectJSON struct {
	JiraID string `json:"jira_id" doc:"Jira's id for the project, which stays the same when the project's key changes."`
	Key    string `json:"key" doc:"The project's key, such as PROJ."`
	Name   string `json:"name" doc:"The project's name."`
}

var opJiraProjectsList = httpx.NewListOp[jiraProjectListRequest, jiraProjectJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/jira/projects/list",
	ID:          "list_jira_projects",
	Summary:     "List the organization's Jira projects",
	Description: "Reads one page of the Jira projects the organization's Jira credential can see, live from Jira. total_count is always null, and page_size 0 is refused, because Jira reports no total.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// POST /api/orgs/{org_id}/jira/projects/list
func (s *Server) handleJiraProjectsList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	req, ok := opJiraProjectsList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	search := httpx.Search(&v, req.Search)
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraProjectListFilterKey{OrgID: orgID, Search: httpx.SearchKey(search)}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.jiraSystemClient(w, r, orgID, "jira/projects", "Jira is not connected for this organization")
	if !ok {
		return
	}
	upstream, err := client.ListProjects(r.Context(), search, startAt, page.Limit)
	if err != nil {
		writeJiraUpstream(w, orgID, "the organization's Jira projects", err)
		return
	}
	items := make([]jiraProjectJSON, 0, len(upstream.Projects))
	for _, p := range upstream.Projects {
		items = append(items, jiraProjectJSON{JiraID: p.ID, Key: p.Key, Name: p.Name})
	}
	next := ""
	if upstream.NextStartAt > 0 {
		next = strconv.Itoa(upstream.NextStartAt)
	}
	opJiraProjectsList.WriteProxy(w, page, items, next)
}

var opJiraProjectGet = httpx.NewJSONOp[httpx.NoBody, jiraProjectJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/jira/projects/{jira_id}",
	ID:          "get_jira_project",
	Summary:     "Get a Jira project",
	Description: "Reads one Jira project live from Jira, under the organization's Jira credential.",
	PathParams: map[string]string{
		"org_id":  "The organization's id.",
		"jira_id": "Jira's id for the project, a decimal number; a project key is not accepted.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// GET /api/orgs/{org_id}/jira/projects/{jira_id}
func (s *Server) handleJiraProjectGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	jiraID, ok := jiraPathProjectID(w, r)
	if !ok {
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/project", "Jira is not connected for this organization")
	if !ok {
		return
	}
	project, err := client.GetProject(r.Context(), jiraID)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "Jira project "+jiraID, err)
		return
	}
	opJiraProjectGet.Write(w, jiraProjectJSON{JiraID: project.ID, Key: project.Key, Name: project.Name})
}

var opJiraProjectGetByName = httpx.NewJSONOp[httpx.NoBody, jiraProjectJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/jira/projects/by-name/{key}",
	ID:          "get_jira_project_by_name",
	Summary:     "Get a Jira project by its key",
	Description: "Reads the Jira project whose current key is exactly the one given, live from Jira, under the organization's Jira credential. A key the project had before it was renamed is not found.",
	PathParams: map[string]string{
		"org_id": "The organization's id.",
		"key":    "The project's key as Jira writes it, such as PROJ: an uppercase letter, then uppercase letters, digits, or underscores.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// jiraProjectKeyRe is a Jira project key as both deployments write one: an
// uppercase letter, then uppercase letters, digits and underscores. The key
// is put into Jira's URL as it stands, so nothing else may reach it, and a
// lower-case spelling is not found rather than folded.
var jiraProjectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}$`)

// GET /api/orgs/{org_id}/jira/projects/by-name/{key}
func (s *Server) handleJiraProjectGetByName(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !jiraProjectKeyRe.MatchString(key) {
		httpx.NotFound(w, "jira project")
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/project", "Jira is not connected for this organization")
	if !ok {
		return
	}
	project, err := client.GetProject(r.Context(), key)
	// Jira answers a key the project was renamed from with the project under
	// its current key. The route answers to the current key alone, so a key
	// names one project at a time.
	if jira.IsNotFound(err) || (err == nil && project.Key != key) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "Jira project "+key, err)
		return
	}
	opJiraProjectGetByName.Write(w, jiraProjectJSON{JiraID: project.ID, Key: project.Key, Name: project.Name})
}

// jiraStatusListRequest is the statuses list's body: a search over the status
// name plus the paging pair.
type jiraStatusListRequest struct {
	// Search is matched against the status name as a case-insensitive literal
	// substring (httpx.Search). Jira's statuses read takes no filter, so it is
	// applied to the workflow in hand, before the window. Absent matches every
	// status.
	Search string `json:"search" doc:"Text the status's name must contain, matched without regard to case; absent matches every status."`
	httpx.PageRequest
}

// jiraStatusListFilterKey fingerprints a statuses page token to the org, the
// project, whose statuses the wrapped offset points into, and the search,
// which decides which of them the offset counts.
type jiraStatusListFilterKey struct {
	OrgID  string `json:"org_id"`
	JiraID string `json:"jira_id"`
	Search string `json:"search"`
}

// jiraStatusJSON is one workflow status. The id is what the team write takes
// and what a rule is keyed on; the name is what a person recognizes it by.
type jiraStatusJSON struct {
	ID   string `json:"id" doc:"Jira's id for the status."`
	Name string `json:"name" doc:"The status's name."`
}

var opJiraStatusesList = httpx.NewListOp[jiraStatusListRequest, jiraStatusJSON](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list",
	ID:          "list_jira_project_statuses",
	Summary:     "List a Jira project's workflow statuses",
	Description: "Reads one page of the statuses the project's workflows use, live from Jira, ordered by name and then id. total_count is always null, and page_size 0 is refused.",
	PathParams: map[string]string{
		"org_id":  "The organization's id.",
		"jira_id": "Jira's id for the project, a decimal number; a project key is not accepted.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list
//
// Jira serves a project's statuses in one unpaged response, grouped by issue
// type, which the client unions (jira.ProjectStatuses). The search and then
// the window are applied here to that list, ordered by name and then id so an
// offset addresses the same rows on every read.
func (s *Server) handleJiraStatusesList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	jiraID, ok := jiraPathProjectID(w, r)
	if !ok {
		return
	}

	req, ok := opJiraStatusesList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	search := httpx.Search(&v, req.Search)
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraStatusListFilterKey{OrgID: orgID, JiraID: jiraID, Search: httpx.SearchKey(search)}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	client, ok := s.jiraSystemClient(w, r, orgID, "jira/statuses", "Jira is not connected for this organization")
	if !ok {
		return
	}
	statuses, err := client.ProjectStatuses(r.Context(), jiraID)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the workflow statuses of Jira project "+jiraID, err)
		return
	}
	statuses = slices.DeleteFunc(statuses, func(st jira.Status) bool { return !httpx.SearchMatches(search, st.Name) })
	slices.SortFunc(statuses, func(a, b jira.Status) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	items, next := jiraWindow(statuses, startAt, page.Limit, func(st jira.Status) jiraStatusJSON {
		return jiraStatusJSON{ID: st.ID, Name: st.Name}
	})
	opJiraStatusesList.WriteProxy(w, page, items, next)
}

var opJiraStatusGet = httpx.NewJSONOp[httpx.NoBody, jiraStatusJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/{status_id}",
	ID:          "get_jira_project_status",
	Summary:     "Get a Jira project's workflow status",
	Description: "Reads one status of the project's workflows live from Jira. A status that exists in Jira but is not in this project's workflows is not found.",
	PathParams: map[string]string{
		"org_id":    "The organization's id.",
		"jira_id":   "Jira's id for the project, a decimal number; a project key is not accepted.",
		"status_id": "Jira's id for the status, a decimal number.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// GET /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/{status_id}
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
	jiraID, ok := jiraPathProjectID(w, r)
	if !ok {
		return
	}
	statusID := r.PathValue("status_id")
	if !jiraStatusIDRe.MatchString(statusID) {
		httpx.NotFound(w, "jira workflow status")
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/status", "Jira is not connected for this organization")
	if !ok {
		return
	}
	statuses, err := client.ProjectStatuses(r.Context(), jiraID)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the workflow statuses of Jira project "+jiraID, err)
		return
	}
	i := slices.IndexFunc(statuses, func(st jira.Status) bool { return st.ID == statusID })
	if i < 0 {
		httpx.NotFound(w, "jira workflow status")
		return
	}
	opJiraStatusGet.Write(w, jiraStatusJSON{ID: statuses[i].ID, Name: statuses[i].Name})
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

// jiraPathProjectID reads a Jira project id from the path. Anything that is
// not one — a project key included — is a 404 rather than a lookup, so one
// project has one address.
func jiraPathProjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	jiraID := r.PathValue("jira_id")
	if !jiraProjectIDRe.MatchString(jiraID) {
		httpx.NotFound(w, "jira project")
		return "", false
	}
	return jiraID, true
}

// jiraSystemClient resolves the org's Jira service credential into a client.
// No credential is a 409 NOT_CONFIGURED, and Jira turned off by an org admin a
// 409 SOURCE_DISABLED, which no Jira call is made past; a failed read is a
// 500, never "not connected", so a store outage cannot send an admin off to
// rebind a credential that is fine.
func (s *Server) jiraSystemClient(w http.ResponseWriter, r *http.Request, orgID, op, notConfigured string) (*jira.Client, bool) {
	client, err := s.jiraResolver.ForSystem(r.Context(), orgID)
	if errors.Is(err, eventsource.ErrDisabled) {
		writeSourceDisabled(w, eventsource.KindJira)
		return nil, false
	}
	if errors.Is(err, jira.ErrNoJiraSystemCredential) {
		writeNotConfigured(w, notConfigured)
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
