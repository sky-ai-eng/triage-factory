package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

func jiraOrgPath(orgID string) string { return "/api/orgs/" + orgID + "/jira" }

var jiraProjectsListPath = jiraOrgPath(runmode.LocalDefaultOrgID) + "/projects/list"

// jiraProjectPath addresses the fake catalog's project key by its Jira id
// (fakeJiraProjectID), which is how the routes address a project.
func jiraProjectPath(key string) string { return jiraProjectSegmentPath(fakeJiraProjectID(key)) }

// jiraProjectSegmentPath puts segment in the path as it is, for the tests of
// what is not a project id.
func jiraProjectSegmentPath(segment string) string {
	return jiraOrgPath(runmode.LocalDefaultOrgID) + "/projects/" + segment
}

func jiraStatusesListPath(key string) string { return jiraProjectPath(key) + "/statuses/list" }

func jiraStatusPath(key, statusID string) string {
	return jiraProjectPath(key) + "/statuses/" + statusID
}

// TestJiraProjectsList_TokenIsFingerprintedOnQ: the search filters and pages the
// catalog under Jira's ids with a null total, and a token continues only its own
// search: another filter is refused, a re-spelling in case and spacing is not.
func TestJiraProjectsList_TokenIsFingerprintedOnQ(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS", "DESK")

	first := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "sk", "page_size": 1}))
	if want := []jiraProjectJSON{{JiraID: fakeJiraProjectID("SKY"), Key: "SKY", Name: "SKY Project"}}; !slices.Equal(first.Items, want) ||
		first.NextPageToken == "" || first.TotalCount != nil {
		t.Fatalf("page 1 = %+v (token %q, total %v), want %+v with a token and a null total", first.Items, first.NextPageToken, first.TotalCount, want)
	}

	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"page_size": 1, "page_token": first.NextPageToken})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token minted under a different filter; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); items[0].Field != "page_token" {
		t.Errorf("faulting field = %q, want page_token", items[0].Field)
	}

	// OPS does not match, so the second and last page is DESK.
	second := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "  SK  ", "page_size": 1, "page_token": first.NextPageToken}))
	if got := projectKeysOf(second.Items); got != "DESK" || second.NextPageToken != "" {
		t.Errorf("page 2 under a re-spelled filter = %s (token %q), want DESK and no token", got, second.NextPageToken)
	}
}

// TestJiraProjectsList_StrictBody: an unknown field, a count-only read (a proxy
// list has no total) and an out-of-range page size are each a 400 that never
// reaches Jira.
func TestJiraProjectsList_StrictBody(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"query": "sk"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "query")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"page_size": 5000}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	if fake.Calls() != 0 {
		t.Errorf("rejected body still reached Jira (%d calls)", fake.Calls())
	}
}

func projectKeysOf(items []jiraProjectJSON) string {
	keys := make([]string, len(items))
	for i, p := range items {
		keys[i] = p.Key
	}
	return strings.Join(keys, ",")
}

// TestJiraCatalog_MalformedOrgIsNotFound: an org id that is not one is a 404
// that never reaches Jira. Membership itself is N=1 in local mode; the
// Postgres test covers a caller outside the org.
func TestJiraCatalog_MalformedOrgIsNotFound(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	org := jiraOrgPath("not-an-org")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"projects list": doJSON(t, s, http.MethodPost, org+"/projects/list", map[string]any{}),
		"project":       doJSON(t, s, http.MethodGet, org+"/projects/"+fakeJiraProjectID("SKY"), nil),
		"statuses list": doJSON(t, s, http.MethodPost, org+"/projects/"+fakeJiraProjectID("SKY")+"/statuses/list", map[string]any{}),
		"status":        doJSON(t, s, http.MethodGet, org+"/projects/"+fakeJiraProjectID("SKY")+"/statuses/"+statusDoneID, nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusNotFound, "NOT_FOUND", "")
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a malformed org reached Jira %d times", fake.Calls())
	}
}

// TestJiraCatalog_UpstreamFailure: a failed Jira call is a 502 on every read,
// never an empty page or a 404.
func TestJiraCatalog_UpstreamFailure(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.SetFailing(true)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"projects list": doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{}),
		"project":       doJSON(t, s, http.MethodGet, jiraProjectPath("SKY"), nil),
		"statuses list": doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{}),
		"status":        doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", statusDoneID), nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
		})
	}
}

// TestJiraProjectGet: one project by its Jira id, carrying only the id, key and
// name. A project Jira cannot show is a 404, and so is a segment that is not a
// project id (its key included), which never reaches Jira.
func TestJiraProjectGet(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")

	rec := doJSON(t, s, http.MethodGet, jiraProjectPath("OPS"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET project = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 || got["jira_id"] != fakeJiraProjectID("OPS") || got["key"] != "OPS" || got["name"] != "OPS Project" {
		t.Errorf("project = %v, want exactly OPS's jira_id, key OPS and name OPS Project", got)
	}

	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("GONE"), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	fake.Hide("SKY")
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("SKY"), nil),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	for _, segment := range []string{"OPS", "ops", "-1", "1e3"} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectSegmentPath(segment), nil),
			http.StatusNotFound, "NOT_FOUND", "")
	}
	if fake.Calls() != before {
		t.Errorf("a segment that is not a project id reached Jira %d times", fake.Calls()-before)
	}
}

// TestJiraStatusesList_TokenIsBoundToTheProject: a project's whole workflow
// pages by name, each status under the id the team write takes, and a token is
// an offset into that project's workflow that cannot page another's.
func TestJiraStatusesList_TokenIsBoundToTheProject(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")
	got, token := readTwoPages[jiraStatusJSON](t, s, jiraStatusesListPath("SKY"))
	want := []jiraStatusJSON{
		{ID: statusInReviewID, Name: "Code Review"},
		{ID: statusDoneID, Name: "Done"},
		{ID: statusInProgressID, Name: "In Progress"},
		{ID: statusToDoID, Name: "To Do"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("statuses = %+v, want %+v", got, want)
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("OPS"),
		map[string]any{"page_size": 2, "page_token": token}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
}

// TestJiraStatusesList_Refusals: a project Jira cannot show is a 404, as is a
// segment that is not a project id; the body is strict and count-only has no
// answer. Only the first of these reaches Jira.
func TestJiraStatusesList_Refusals(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.Hide("GONE")

	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("GONE"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraProjectSegmentPath("SKY")+"/statuses/list", map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{"project": "OPS"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "project")
	if fake.Calls() != before {
		t.Errorf("a refused request reached Jira %d times", fake.Calls()-before)
	}
}

// TestJiraStatusGet: one status through the project whose workflow uses it.
// Another project's status, an id Jira does not know and a project Jira cannot
// show are 404s; a path outside the id grammar is a 404 that never reaches Jira.
func TestJiraStatusGet(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	opsOnly := fakeJiraStatus{ID: "20000", Name: "Queued"}
	fake.SetStatuses("OPS", opsOnly)

	rec := doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", statusInProgressID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got jiraStatusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := (jiraStatusJSON{ID: statusInProgressID, Name: "In Progress"}); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}

	if rec := doJSON(t, s, http.MethodGet, jiraStatusPath("OPS", opsOnly.ID), nil); rec.Code != http.StatusOK {
		t.Errorf("GET the status at its own project = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", opsOnly.ID), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", statusUnknownID), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	fake.Hide("GONE")
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraStatusPath("GONE", statusDoneID), nil),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	for _, path := range []string{
		jiraStatusPath("SKY", "Done"),
		jiraStatusPath("SKY", "-1"),
		jiraProjectSegmentPath("SKY") + "/statuses/" + statusDoneID,
	} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, path, nil), http.StatusNotFound, "NOT_FOUND", "")
	}
	if fake.Calls() != before {
		t.Errorf("a path outside the grammar reached Jira %d times", fake.Calls()-before)
	}
}

// TestJiraCatalog_NotConnectedEverywhere: with no service credential every
// read is a 409 naming the fix, never an empty list or a 404 for a project
// that may exist.
func TestJiraCatalog_NotConnectedEverywhere(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"projects list": doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{}),
		"project":       doJSON(t, s, http.MethodGet, jiraProjectPath("SKY"), nil),
		"statuses list": doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{}),
		"status":        doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", statusDoneID), nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusConflict, "NOT_CONFIGURED", "")
		})
	}
}
