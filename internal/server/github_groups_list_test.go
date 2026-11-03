package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// acmeFourTeamsGraphQL lists four of acme's teams, out of order, with one name
// carrying a literal percent sign and another a name a LIKE wildcard reading
// of "100%" would also match.
const acmeFourTeamsGraphQL = `{"data":{"organization":{"databaseId":9,"teams":{
	"pageInfo":{"hasNextPage":false,"endCursor":""},
	"nodes":[
		{"databaseId":13,"slug":"web-core","name":"Frontend 100% guild","members":{"totalCount":3}},
		{"databaseId":12,"slug":"security","name":"Security","members":{"totalCount":40}},
		{"databaseId":14,"slug":"thousand","name":"1000 club","members":{"totalCount":2}},
		{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}}
	]}}}}`

// switchableGroupsServer points the org's GitHub at a server answering acme's
// organization.teams query with whatever set() last stored, and tracks a
// repository of acme.
func switchableGroupsServer(t *testing.T, srv *Server, body string) (set func(string)) {
	t.Helper()
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL})
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")
	return func(b string) {
		mu.Lock()
		defer mu.Unlock()
		body = b
	}
}

func itemIDs(items []gitHubGroupListItemJSON) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.GitHubTeamID
	}
	return out
}

// The list orders the whole candidate set by folded handle, then pages it with
// a real total: a page token walks it, and the next page holds what the first
// did not.
func TestTeamGitHubGroupsList_OrdersAndPages(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)

	first := listGitHubGroups(t, srv, map[string]any{"page_size": 2})
	if got, want := itemIDs(first.Items), []string{"11", "12"}; !reflect.DeepEqual(got, want) || first.TotalCount != 4 {
		t.Fatalf("page 1 = %v (total %d); want %v of 4 — platform, security by handle", got, first.TotalCount, want)
	}
	if first.NextPageToken == "" {
		t.Fatal("page 1 has no next_page_token; want one")
	}
	second := listGitHubGroups(t, srv, map[string]any{"page_size": 2, "page_token": first.NextPageToken})
	if got, want := itemIDs(second.Items), []string{"14", "13"}; !reflect.DeepEqual(got, want) || second.NextPageToken != "" {
		t.Fatalf("page 2 = %v (next %q); want %v and no further page — thousand, web-core", got, second.NextPageToken, want)
	}

	// page_size 0 is the count-only read.
	if count := listGitHubGroups(t, srv, map[string]any{"page_size": 0}); len(count.Items) != 0 || count.TotalCount != 4 {
		t.Errorf("count-only = %d items, total %d; want 0 items, total 4", len(count.Items), count.TotalCount)
	}
}

// The search covers the handle and the name, case-insensitively and as a
// literal: a % is a percent sign, not a wildcard.
func TestTeamGitHubGroupsList_Search(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)

	for _, tc := range []struct {
		search string
		want   []string
	}{
		{"ACME/SEC", []string{"12"}},    // the handle, folded
		{"frontend", []string{"13"}},    // the name
		{"  platform ", []string{"11"}}, // trimmed
		{"100%", []string{"13"}},        // a literal percent: "1000 club" does not match
		{"acme/", []string{"11", "12", "14", "13"}},
		{"nothing-like-it", []string{}},
	} {
		resp := listGitHubGroups(t, srv, map[string]any{"search": tc.search})
		if got := itemIDs(resp.Items); !reflect.DeepEqual(got, tc.want) || resp.TotalCount != len(tc.want) {
			t.Errorf("search %q = %v (total %d); want %v", tc.search, got, resp.TotalCount, tc.want)
		}
	}
}

// A bad body is refused, naming the field: the search cap, a token minted
// under another search or filter, and the field this route does not have.
func TestTeamGitHubGroupsList_RefusesBadBodies(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)

	first := listGitHubGroups(t, srv, map[string]any{"page_size": 1})
	if first.NextPageToken == "" {
		t.Fatal("page 1 has no next_page_token; want one")
	}
	for _, tc := range []struct {
		name   string
		body   map[string]any
		reason string
		field  string
	}{
		{"search over the cap", map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes+1)}, httpx.ReasonInvalidField, "search"},
		{"token under another search", map[string]any{"page_size": 1, "page_token": first.NextPageToken, "search": "acme"}, httpx.ReasonInvalidParam, "page_token"},
		{"token under another filter", map[string]any{"page_size": 1, "page_token": first.NextPageToken, "only_mine": true}, httpx.ReasonInvalidParam, "page_token"},
		{"a field the route lacks", map[string]any{"q": "acme"}, httpx.ReasonUnknownField, "q"},
		{"a mistyped flag", map[string]any{"include_membership": "true"}, httpx.ReasonInvalidField, "include_membership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, srv, http.MethodPost, "/api/teams/default/github-groups/list", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			items := errorItems(t, rec)
			if len(items) != 1 || items[0].Reason != tc.reason || items[0].Field != tc.field {
				t.Errorf("errors = %+v; want one %s on %q", items, tc.reason, tc.field)
			}
		})
	}

	// A search at the cap is a search, not a fault.
	if resp := listGitHubGroups(t, srv, map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes)}); len(resp.Items) != 0 {
		t.Errorf("a search at the cap matched %v; want nothing", itemIDs(resp.Items))
	}
	// include_membership annotates and narrows nothing, so a token minted
	// without it still pages with it.
	rec := doJSON(t, srv, http.MethodPost, "/api/teams/default/github-groups/list",
		map[string]any{"page_size": 1, "page_token": first.NextPageToken, "include_membership": true})
	if rec.Code != http.StatusOK {
		t.Errorf("token plus include_membership = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// A mapping GitHub no longer lists is still a row, unlisted, so an admin can
// un-assign it; it is searched and ordered with the candidates.
func TestTeamGitHubGroupsList_KeepsAMappingGitHubStoppedListing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	set := switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)
	putGitHubGroups(t, srv, "11", "13")

	// web-core (13) is deleted on GitHub.
	set(acmeTeamsGraphQL)
	resp := listGitHubGroups(t, srv, nil)
	if got, want := itemIDs(resp.Items), []string{"11", "12", "13"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %v; want %v — the unlisted mapping ordered by its handle", got, want)
	}
	if resp.Items[0].Listed != true || resp.Items[2].Listed != false || resp.Items[2].TeamSlug != "web-core" {
		t.Errorf("items = %+v; want platform listed and web-core unlisted", resp.Items)
	}
	if only := listGitHubGroups(t, srv, map[string]any{"search": "web"}); !reflect.DeepEqual(itemIDs(only.Items), []string{"13"}) {
		t.Errorf("search web = %v; want the unlisted mapping alone", itemIDs(only.Items))
	}
}

// gitHubGroupRows is the set the list pages: one row per GitHub team, a
// stored mapping folded into the candidate it names, ordered by folded handle
// then id.
func TestGitHubGroupRows(t *testing.T) {
	candidates := []gitHubGroupCandidateJSON{
		{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "security", Name: "Security"},
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "platform", Name: "Platform"},
	}
	stored := []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "platform"},
		// A local mapping stored before ids were recorded is the candidate
		// with its handle, however it is cased.
		{OrgLogin: "ACME", TeamSlug: "Security"},
		{GitHubTeamID: "99", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "gone"},
		// A deleted team whose slug a new team took: two rows, one handle.
		{GitHubTeamID: "98", GitHubOrgID: "9", OrgLogin: "Acme", TeamSlug: "platform"},
	}
	rows := gitHubGroupRows(candidates, stored)
	type row struct {
		id     string
		handle string
		listed bool
	}
	got := make([]row, len(rows))
	for i, r := range rows {
		got[i] = row{r.GitHubTeamID, r.OrgLogin + "/" + r.TeamSlug, r.Listed}
	}
	want := []row{
		{"99", "acme/gone", false},
		{"11", "acme/platform", true},
		{"98", "Acme/platform", false},
		{"12", "acme/security", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v\nwant %+v", got, want)
	}
}
