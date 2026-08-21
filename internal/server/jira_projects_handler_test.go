package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

const jiraProjectsListPath = "/api/jira/projects/list"

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

// TestJiraProjectsList_NotConnected — an org with no Jira credential gets the
// configuration answer, not an upstream failure standing in for one.
func TestJiraProjectsList_NotConnected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an unconnected workspace; body=%s", rec.Code, rec.Body.String())
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
