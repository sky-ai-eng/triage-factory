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

// ids keeps the teams an editor has picked and exclude_ids the rest, under
// the search, each with its own total: together they partition what the
// search matches, so a tab of picked teams and a tab of the others each page
// and count only their own rows.
func TestTeamGitHubGroupsList_IDFilters(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)

	for _, tc := range []struct {
		name string
		body map[string]any
		want []string
	}{
		{"ids", map[string]any{"ids": []string{"13", "11"}}, []string{"11", "13"}},
		{"exclude_ids", map[string]any{"exclude_ids": []string{"13", "11"}}, []string{"12", "14"}},
		{"ids under a search", map[string]any{"ids": []string{"11", "12"}, "search": "sec"}, []string{"12"}},
		{"exclude_ids under a search", map[string]any{"exclude_ids": []string{"11", "12"}, "search": "acme/"}, []string{"14", "13"}},
		{"an id no team has", map[string]any{"ids": []string{"404"}}, []string{}},
		{"empty ids narrows nothing", map[string]any{"ids": []string{}}, []string{"11", "12", "14", "13"}},
		{"empty exclude_ids narrows nothing", map[string]any{"exclude_ids": []string{}}, []string{"11", "12", "14", "13"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := listGitHubGroups(t, srv, tc.body)
			if got := itemIDs(resp.Items); !reflect.DeepEqual(got, tc.want) || resp.TotalCount != len(tc.want) {
				t.Errorf("%v = %v (total %d); want %v", tc.body, got, resp.TotalCount, tc.want)
			}
		})
	}

	// A page token carries the ids it was minted under: the same set in
	// another order, or repeated, continues the walk, and another set is
	// refused.
	first := listGitHubGroups(t, srv, map[string]any{"exclude_ids": []string{"11", "13"}, "page_size": 1})
	if got := itemIDs(first.Items); !reflect.DeepEqual(got, []string{"12"}) || first.NextPageToken == "" {
		t.Fatalf("page 1 = %v (next %q); want [12] and a further page", got, first.NextPageToken)
	}
	second := listGitHubGroups(t, srv, map[string]any{
		"exclude_ids": []string{"13", "11", "13"}, "page_size": 1, "page_token": first.NextPageToken,
	})
	if got := itemIDs(second.Items); !reflect.DeepEqual(got, []string{"14"}) {
		t.Errorf("page 2 under the same set reordered = %v; want [14]", got)
	}
	for _, body := range []map[string]any{
		{"exclude_ids": []string{"11"}, "page_size": 1, "page_token": first.NextPageToken},
		{"ids": []string{"11", "13"}, "page_size": 1, "page_token": first.NextPageToken},
	} {
		rec := doJSON(t, srv, http.MethodPost, "/api/teams/default/github-groups/list", body)
		if items := errorItems(t, rec); rec.Code != http.StatusBadRequest || len(items) != 1 || items[0].Field != "page_token" {
			t.Errorf("token under another id filter %v = %d %+v; want one 400 on page_token", body, rec.Code, items)
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
		{"ids naming no GitHub team id", map[string]any{"ids": []string{"11", "platform"}}, httpx.ReasonInvalidField, "ids[1]"},
		{"exclude_ids naming a non-canonical id", map[string]any{"exclude_ids": []string{"011"}}, httpx.ReasonInvalidField, "exclude_ids[0]"},
		{"exclude_ids naming an empty id", map[string]any{"exclude_ids": []string{""}}, httpx.ReasonInvalidField, "exclude_ids[0]"},
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

	// Every bad id is reported, across both filters.
	rec := doJSON(t, srv, http.MethodPost, "/api/teams/default/github-groups/list",
		map[string]any{"ids": []string{"x"}, "exclude_ids": []string{"y"}})
	if items := errorItems(t, rec); rec.Code != http.StatusBadRequest || len(items) != 2 ||
		items[0].Field != "ids[0]" || items[1].Field != "exclude_ids[0]" {
		t.Errorf("two bad ids = %d %+v; want 400 naming ids[0] and exclude_ids[0]", rec.Code, items)
	}

	// A search at the cap is a search, not a fault.
	if resp := listGitHubGroups(t, srv, map[string]any{"search": strings.Repeat("é", httpx.MaxSearchRunes)}); len(resp.Items) != 0 {
		t.Errorf("a search at the cap matched %v; want nothing", itemIDs(resp.Items))
	}
	// include_membership annotates and narrows nothing, so a token minted
	// without it still pages with it.
	rec = doJSON(t, srv, http.MethodPost, "/api/teams/default/github-groups/list",
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

// The checklist's reads share one GitHub listing until it is dropped: a team
// GitHub adds is not listed until the org's entry is forgotten (the tracked
// repositories' PUT does that) or its minute passes.
func TestTeamGitHubGroupsList_HoldsTheListing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	set := switchableGroupsServer(t, srv, acmeFourTeamsGraphQL)

	if got := listGitHubGroups(t, srv, nil); got.TotalCount != 4 {
		t.Fatalf("first read total = %d, want 4", got.TotalCount)
	}
	set(strings.Replace(acmeFourTeamsGraphQL,
		`{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}}`,
		`{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}},
		{"databaseId":15,"slug":"zebra","name":"Zebra","members":{"totalCount":1}}`, 1))
	if got := listGitHubGroups(t, srv, map[string]any{"search": "zebra"}); got.TotalCount != 0 {
		t.Fatalf("held read searching the new team total = %d, want 0: the held listing predates it", got.TotalCount)
	}

	host, err := srv.orgGitHubHost(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	srv.gitHubGroupListings.Forget(gitHubGroupListingKey(runmode.LocalDefaultOrgID, host))
	if got := listGitHubGroups(t, srv, map[string]any{"search": "zebra"}); got.TotalCount != 1 {
		t.Fatalf("read after the entry was dropped total = %d, want 1", got.TotalCount)
	}
}
