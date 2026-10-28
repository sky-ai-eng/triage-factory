package server

import (
	"cmp"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The org's Jira priorities, and each project's issue types: the values a Jira
// event predicate's priority_id_in and issue_type_id_in filters name.
//
//   POST /api/orgs/{org_id}/jira/priorities/list
//   GET  /api/orgs/{org_id}/jira/priorities/{priority_id}
//   POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list
//   GET  /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/{issue_type_id}
//
// Proxied live like the rest of the catalog (jira_projects_handler.go), under
// the org's Jira service credential, member-gated. Each list arrives from Jira
// in one unpaged response, which the window is applied to here; both are proxy
// lists, so total_count is null.
//
// Priorities are the instance's: Jira serves one list (/priority) on both
// deployments. Issue types are read per project (/project/{id}), which both
// deployments serve with the project's issue types, rather than from the
// instance-wide /issuetype list: a team-managed project on Cloud has issue
// types of its own, under ids no other project uses, and a predicate is
// written for the projects a team tracks.
// --------------------------------------------------------------------

// jiraIssueFieldListRequest is the body of both lists: the paging pair alone,
// since either list is short enough that nothing narrows it.
type jiraIssueFieldListRequest struct {
	httpx.PageRequest
}

// jiraPriorityListFilterKey fingerprints a priorities page token to the org,
// whose list the wrapped offset points into.
type jiraPriorityListFilterKey struct {
	OrgID string `json:"org_id"`
}

// jiraIssueTypeListFilterKey fingerprints an issue types page token to the org
// and the project.
type jiraIssueTypeListFilterKey struct {
	OrgID  string `json:"org_id"`
	JiraID string `json:"jira_id"`
}

// jiraPriorityJSON is one priority. The id is what a predicate names it by;
// the name is what a person recognizes it by.
type jiraPriorityJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// jiraIssueTypeJSON is one issue type of a project. Subtask says issues of
// this type are subtasks.
type jiraIssueTypeJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subtask bool   `json:"subtask"`
}

// jiraNumericIDRe is a Jira priority or issue type id as both deployments
// write one: a decimal integer, carried as a string.
var jiraNumericIDRe = regexp.MustCompile(`^[0-9]+$`)

// POST /api/orgs/{org_id}/jira/priorities/list
//
// In Jira's order, which is the instance's priority scheme from highest to
// lowest, the same on every read, so an offset addresses the same rows.
func (s *Server) handleJiraPrioritiesList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	var req jiraIssueFieldListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(jiraPriorityListFilterKey{OrgID: orgID}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/priorities", "Jira is not connected for this workspace")
	if !ok {
		return
	}
	priorities, err := client.ListPriorities(r.Context())
	if err != nil {
		writeJiraUpstream(w, orgID, "the workspace's Jira priorities", err)
		return
	}
	items, next := jiraWindow(priorities, startAt, page.Limit, func(p jira.Priority) jiraPriorityJSON {
		return jiraPriorityJSON{ID: p.ID, Name: p.Name}
	})
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/jira/priorities/{priority_id}
//
// Jira's /priority/{id} answers the same row, but reading the list and picking
// the one asked for keeps this answering for exactly the rows the list serves.
func (s *Server) handleJiraPriorityGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	priorityID := r.PathValue("priority_id")
	if !jiraNumericIDRe.MatchString(priorityID) {
		httpx.NotFound(w, "jira priority")
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/priority", "Jira is not connected for this workspace")
	if !ok {
		return
	}
	priorities, err := client.ListPriorities(r.Context())
	if err != nil {
		writeJiraUpstream(w, orgID, "the workspace's Jira priorities", err)
		return
	}
	i := slices.IndexFunc(priorities, func(p jira.Priority) bool { return p.ID == priorityID })
	if i < 0 {
		httpx.NotFound(w, "jira priority")
		return
	}
	writeJSON(w, http.StatusOK, jiraPriorityJSON{ID: priorities[i].ID, Name: priorities[i].Name})
}

// POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list
//
// Ordered by name and then id, so an offset addresses the same rows on every
// read.
func (s *Server) handleJiraIssueTypesList(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	jiraID, ok := jiraPathProjectID(w, r)
	if !ok {
		return
	}
	var req jiraIssueFieldListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraIssueTypeListFilterKey{OrgID: orgID, JiraID: jiraID}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	refuseJiraCountOnly(&v, page)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/issue-types", "Jira is not connected for this workspace")
	if !ok {
		return
	}
	types, err := client.ListIssueTypes(r.Context(), jiraID)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the issue types of Jira project "+jiraID, err)
		return
	}
	slices.SortFunc(types, func(a, b jira.IssueType) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	items, next := jiraWindow(types, startAt, page.Limit, toJiraIssueTypeJSON)
	httpx.WriteProxyList(w, page, items, next)
}

// GET /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/{issue_type_id}
//
// An issue type that exists but is not one of this project's is a 404: the
// path names the project, and an issue type is addressed through a project
// that uses it.
func (s *Server) handleJiraIssueTypeGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	jiraID, ok := jiraPathProjectID(w, r)
	if !ok {
		return
	}
	typeID := r.PathValue("issue_type_id")
	if !jiraNumericIDRe.MatchString(typeID) {
		httpx.NotFound(w, "jira issue type")
		return
	}
	client, ok := s.jiraSystemClient(w, r, orgID, "jira/issue-type", "Jira is not connected for this workspace")
	if !ok {
		return
	}
	types, err := client.ListIssueTypes(r.Context(), jiraID)
	if jira.IsNotFound(err) {
		httpx.NotFound(w, "jira project")
		return
	}
	if err != nil {
		writeJiraUpstream(w, orgID, "the issue types of Jira project "+jiraID, err)
		return
	}
	i := slices.IndexFunc(types, func(it jira.IssueType) bool { return it.ID == typeID })
	if i < 0 {
		httpx.NotFound(w, "jira issue type")
		return
	}
	writeJSON(w, http.StatusOK, toJiraIssueTypeJSON(types[i]))
}

func toJiraIssueTypeJSON(it jira.IssueType) jiraIssueTypeJSON {
	return jiraIssueTypeJSON{ID: it.ID, Name: it.Name, Subtask: it.Subtask}
}

// jiraWindow is one page of a list Jira serves whole: limit rows from
// startAt, converted, and the page token of the next window, "" when this was
// the last.
func jiraWindow[T, J any](all []T, startAt, limit int, convert func(T) J) ([]J, string) {
	start := min(startAt, len(all))
	end := min(start+limit, len(all))
	items := make([]J, 0, end-start)
	for _, x := range all[start:end] {
		items = append(items, convert(x))
	}
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return items, next
}
