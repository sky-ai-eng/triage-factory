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

func jiraProjectPath(key string) string {
	return jiraOrgPath(runmode.LocalDefaultOrgID) + "/projects/" + key
}

func jiraStatusesListPath(key string) string { return jiraProjectPath(key) + "/statuses/list" }

// TestJiraProjectsList_ProxyPagingRoundTrip walks the list the way a client
// does — first page, then the token it was handed — and pins the two halves of
// the proxy-list contract: the rows page, and total_count is null rather than a
// number this route cannot honestly produce.
func TestJiraProjectsList_ProxyPagingRoundTrip(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "ALPHA", "BETA", "GAMMA", "DELTA", "EPSILON")

	first := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"page_size": 2}))
	if got := projectKeysOf(first.Items); got != "ALPHA,BETA" {
		t.Fatalf("page 1 = %s, want ALPHA,BETA", got)
	}
	if first.TotalCount != nil {
		t.Errorf("total_count = %d, want null — a proxy list cannot count itself", *first.TotalCount)
	}
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token with three projects still to serve")
	}

	second := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}))
	if got := projectKeysOf(second.Items); got != "GAMMA,DELTA" {
		t.Fatalf("page 2 = %s, want GAMMA,DELTA", got)
	}
	if second.NextPageToken == "" {
		t.Fatal("page 2 carried no next_page_token with one project still to serve")
	}

	third := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"page_size": 2, "page_token": second.NextPageToken}))
	if got := projectKeysOf(third.Items); got != "EPSILON" {
		t.Fatalf("page 3 = %s, want EPSILON", got)
	}
	if third.NextPageToken != "" {
		t.Errorf("last page minted a next_page_token (%q)", third.NextPageToken)
	}
}

// TestJiraProjectsList_FiltersOnQ covers the search box: the filter matches the
// key and the name, and it is the server that applies it.
func TestJiraProjectsList_FiltersOnQ(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS", "DESK")

	page := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "sk"}))
	if got := projectKeysOf(page.Items); got != "SKY,DESK" {
		t.Errorf("filtered page = %s, want SKY,DESK", got)
	}
}

// TestJiraProjectsList_TokenIsFingerprintedOnQ is why `q` belongs in the
// fingerprint: page 2 of the unfiltered catalog addresses a different result
// set than page 2 of a search, so a token must not cross a filter change.
func TestJiraProjectsList_TokenIsFingerprintedOnQ(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS", "DESK")

	first := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "sk", "page_size": 1, "page_token": first.NextPageToken})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token minted under a different filter; body=%s", rec.Code, rec.Body.String())
	}
	if items := decodeErrorItems(t, rec); items[0].Field != "page_token" {
		t.Errorf("faulting field = %q, want page_token", items[0].Field)
	}
}

// TestJiraProjectsList_StrictBody — an unknown field is a caller mistake, not
// something to ignore: a typo'd filter that silently widens the answer is the
// exact failure the strict decode exists to prevent.
func TestJiraProjectsList_StrictBody(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"query": "sk"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown body field; body=%s", rec.Code, rec.Body.String())
	}
	if fake.Calls() != 0 {
		t.Errorf("rejected body still reached Jira (%d calls)", fake.Calls())
	}
}

// TestJiraProjectsList_CountOnlyRejected: page_size 0 asks for a total, and a
// proxy list has none. Refusing names the reason; answering an empty page with
// a null total would read as a count of nothing.
func TestJiraProjectsList_CountOnlyRejected(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")

	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"page_size": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a count-only read; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeErrorItems(t, rec)
	if items[0].Field != "page_size" {
		t.Errorf("faulting field = %q, want page_size", items[0].Field)
	}
	if fake.Calls() != 0 {
		t.Errorf("rejected read still reached Jira (%d calls)", fake.Calls())
	}
}

// TestJiraProjectsList_PageSizeOutOfRange keeps the shared paging contract:
// an oversized window is rejected, never quietly clamped.
func TestJiraProjectsList_PageSizeOutOfRange(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{"page_size": 5000})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func projectKeysOf(items []jiraProjectJSON) string {
	keys := make([]string, len(items))
	for i, p := range items {
		keys[i] = p.Key
	}
	return strings.Join(keys, ",")
}

// TestJiraProjectsList_FilterCaseFoldsForTheToken: the match is
// case-insensitive on both deployments, so two spellings of one search are one
// search — and a token minted under either continues the same page rather than
// being refused as a different query.
func TestJiraProjectsList_FilterCaseFoldsForTheToken(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "SKYNET", "OPS")

	first := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "sky", "page_size": 1}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token with a second match to serve")
	}
	second := decodeList[jiraProjectJSON](t, doJSON(t, s, http.MethodPost, jiraProjectsListPath,
		map[string]any{"q": "  SKY  ", "page_size": 1, "page_token": first.NextPageToken}))
	if got := projectKeysOf(second.Items); got != "SKYNET" {
		t.Errorf("page 2 under a differently-spelled filter = %s, want SKYNET", got)
	}
}

// TestJiraCatalog_MalformedOrgIsNotFound: an org id that is not one is a 404
// that never reaches Jira. Membership itself is N=1 in local mode; the
// Postgres test covers a caller outside the org.
func TestJiraCatalog_MalformedOrgIsNotFound(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	org := jiraOrgPath("not-an-org")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"projects list": doJSON(t, s, http.MethodPost, org+"/projects/list", map[string]any{}),
		"project":       doJSON(t, s, http.MethodGet, org+"/projects/SKY", nil),
		"statuses list": doJSON(t, s, http.MethodPost, org+"/projects/SKY/statuses/list", map[string]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusNotFound, "NOT_FOUND", "")
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a malformed org reached Jira %d times", fake.Calls())
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
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusConflict, "NOT_CONFIGURED", "")
		})
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
	} {
		t.Run(name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
		})
	}
}

// TestJiraProjectGet: one project by key, carrying the key and name and none of
// the rest of Jira's project object. A project Jira cannot show is a 404, and
// so is a key outside the grammar, which never reaches Jira — including a
// lowercase spelling, since one project has one address.
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
	if len(got) != 2 || got["key"] != "OPS" || got["name"] != "OPS Project" {
		t.Errorf("project = %v, want exactly key OPS and name OPS Project", got)
	}

	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("GONE"), nil),
		http.StatusNotFound, "NOT_FOUND", "")
	fake.Hide("SKY")
	assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath("SKY"), nil),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	for _, key := range []string{"ops", "1OPS", "OPS-1"} {
		assertOneFault(t, doJSON(t, s, http.MethodGet, jiraProjectPath(key), nil),
			http.StatusNotFound, "NOT_FOUND", "")
	}
	if fake.Calls() != before {
		t.Errorf("a key outside the grammar reached Jira %d times", fake.Calls()-before)
	}
}

// TestJiraStatusesList_PagesTheProjectsWorkflow walks a project's statuses a
// page at a time. The pages together are the whole workflow, ordered by name,
// each status carrying the id the team write takes; total_count is null.
func TestJiraStatusesList_PagesTheProjectsWorkflow(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY")

	var got []jiraStatusJSON
	token := ""
	for pages := 0; ; pages++ {
		if pages > len(jiraFixtureStatuses) {
			t.Fatal("the statuses list never stopped minting tokens")
		}
		body := map[string]any{"page_size": 3}
		if token != "" {
			body["page_token"] = token
		}
		page := decodeList[jiraStatusJSON](t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), body))
		if len(page.Items) > 3 {
			t.Fatalf("page of %d, want at most 3", len(page.Items))
		}
		if page.TotalCount != nil {
			t.Errorf("total_count = %d, want null", *page.TotalCount)
		}
		got = append(got, page.Items...)
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	want := []jiraStatusJSON{
		{ID: statusInReviewID, Name: "Code Review"},
		{ID: statusDoneID, Name: "Done"},
		{ID: statusInProgressID, Name: "In Progress"},
		{ID: statusToDoID, Name: "To Do"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("statuses = %+v, want %+v", got, want)
	}
}

// TestJiraStatusesList_TokenIsBoundToTheProject: a token is an offset into one
// project's workflow, so it cannot page another project's.
func TestJiraStatusesList_TokenIsBoundToTheProject(t *testing.T) {
	s, _ := newServerWithJiraCatalog(t, "SKY", "OPS")
	first := decodeList[jiraStatusJSON](t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"),
		map[string]any{"page_size": 2}))
	if first.NextPageToken == "" {
		t.Fatal("page 1 carried no next_page_token")
	}
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("OPS"),
		map[string]any{"page_size": 2, "page_token": first.NextPageToken}),
		http.StatusBadRequest, "INVALID_PARAM", "page_token")
}

// TestJiraStatusesList_Refusals: a project Jira cannot show is a 404, as is a
// key outside the grammar; the body is strict and count-only has no answer.
// Only the first of these reaches Jira.
func TestJiraStatusesList_Refusals(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	fake.Hide("GONE")

	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("GONE"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")

	before := fake.Calls()
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("sky"), map[string]any{}),
		http.StatusNotFound, "NOT_FOUND", "")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{"page_size": 0}),
		http.StatusBadRequest, "OUT_OF_RANGE", "page_size")
	assertOneFault(t, doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{"project": "OPS"}),
		http.StatusBadRequest, "UNKNOWN_FIELD", "project")
	if fake.Calls() != before {
		t.Errorf("a refused request reached Jira %d times", fake.Calls()-before)
	}
}
