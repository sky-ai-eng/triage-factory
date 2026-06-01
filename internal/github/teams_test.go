package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListMyTeamsDetailed_PaginationAndFields covers the PAT/local path: it
// paginates GET /user/teams and lifts the name + members_count the full-team
// response carries, so the noisy-team hint needs no extra calls.
func TestListMyTeamsDetailed_PaginationAndFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/teams" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			// A full page (== per_page) forces the paginator to fetch page 2.
			page := make([]map[string]any, 0, teamsPerPage)
			page = append(page, map[string]any{
				"slug":          "platform",
				"name":          "Platform",
				"members_count": 8,
				"organization":  map[string]any{"login": "acme"},
			})
			for i := 1; i < teamsPerPage; i++ {
				page = append(page, map[string]any{
					"slug":         "filler",
					"name":         "Filler",
					"organization": map[string]any{"login": "acme"},
				})
			}
			_ = json.NewEncoder(w).Encode(page)
		case "2":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"slug":          "security",
				"name":          "Security",
				"members_count": 25,
				"organization":  map[string]any{"login": "acme"},
			}})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(ts.Close)

	teams, err := NewClient(ts.URL, "pat").ListMyTeamsDetailed()
	if err != nil {
		t.Fatalf("ListMyTeamsDetailed: %v", err)
	}
	// teamsPerPage from page 1 + 1 from page 2.
	if len(teams) != teamsPerPage+1 {
		t.Fatalf("got %d teams, want %d", len(teams), teamsPerPage+1)
	}
	first := teams[0]
	if first.OrgSlug != "acme" || first.TeamSlug != "platform" || first.Name != "Platform" || first.MemberCount != 8 {
		t.Errorf("first team = %+v; want acme/platform Platform 8", first)
	}
	last := teams[len(teams)-1]
	if last.TeamSlug != "security" || last.MemberCount != 25 {
		t.Errorf("last team = %+v; want security with 25 members", last)
	}
}

// TestListMyTeamsDetailed_SkipsMalformed drops entries missing a slug or org
// login — they can't be routed and would produce a bogus "org/slug".
func TestListMyTeamsDetailed_SkipsMalformed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"slug": "", "organization": map[string]any{"login": "acme"}},
				{"slug": "ok", "organization": map[string]any{"login": ""}},
				{"slug": "good", "organization": map[string]any{"login": "acme"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	t.Cleanup(ts.Close)

	teams, err := NewClient(ts.URL, "pat").ListMyTeamsDetailed()
	if err != nil {
		t.Fatalf("ListMyTeamsDetailed: %v", err)
	}
	if len(teams) != 1 || teams[0].TeamSlug != "good" {
		t.Errorf("got %+v; want only acme/good", teams)
	}
}

// TestListUserTeamsInOrg_FiltersAndCounts covers the multi-mode building
// block: one GraphQL query per org with the userLogins filter returns just
// the user's teams, member counts riding along.
func TestListUserTeamsInOrg_FiltersAndCounts(t *testing.T) {
	var gotLogin string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Login string `json:"login"`
				Org   string `json:"org"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		gotLogin = req.Variables.Login
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
			"pageInfo":{"hasNextPage":false,"endCursor":""},
			"nodes":[
				{"slug":"platform","name":"Platform","members":{"totalCount":8}},
				{"slug":"data","name":"Data","members":{"totalCount":30}}
			]}}}}`))
	}))
	t.Cleanup(ts.Close)

	teams, err := NewClient(ts.URL, "pat").ListUserTeamsInOrg("acme", "octocat")
	if err != nil {
		t.Fatalf("ListUserTeamsInOrg: %v", err)
	}
	if gotLogin != "octocat" {
		t.Errorf("query userLogins login = %q; want octocat", gotLogin)
	}
	if len(teams) != 2 {
		t.Fatalf("got %d teams, want 2: %+v", len(teams), teams)
	}
	if teams[0].OrgSlug != "acme" || teams[0].TeamSlug != "platform" || teams[0].MemberCount != 8 {
		t.Errorf("teams[0] = %+v; want acme/platform 8", teams[0])
	}
	if teams[1].MemberCount != 30 {
		t.Errorf("teams[1] member count = %d; want 30", teams[1].MemberCount)
	}
}

// TestListUserTeamsInOrg_NullOrgIsEmpty proves a user-account owner (or an
// org the token can't read) — which GraphQL returns as organization:null —
// yields an empty result, not an error, so the caller skips it.
func TestListUserTeamsInOrg_NullOrgIsEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"organization":null}}`))
	}))
	t.Cleanup(ts.Close)

	teams, err := NewClient(ts.URL, "pat").ListUserTeamsInOrg("some-user", "octocat")
	if err != nil {
		t.Fatalf("null organization should not error: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("got %d teams, want 0", len(teams))
	}
}

// TestListUserTeamsInOrg_Paginates walks the cursor across two pages.
func TestListUserTeamsInOrg_Paginates(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasCursor := strings.Contains(string(body), `"cursor":"CURSOR1"`)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if !hasCursor {
			_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
				"pageInfo":{"hasNextPage":true,"endCursor":"CURSOR1"},
				"nodes":[{"slug":"a","name":"A","members":{"totalCount":1}}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
			"pageInfo":{"hasNextPage":false,"endCursor":""},
			"nodes":[{"slug":"b","name":"B","members":{"totalCount":2}}]}}}}`))
	}))
	t.Cleanup(ts.Close)

	teams, err := NewClient(ts.URL, "pat").ListUserTeamsInOrg("acme", "octocat")
	if err != nil {
		t.Fatalf("ListUserTeamsInOrg: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d GraphQL calls, want 2", calls)
	}
	if len(teams) != 2 || teams[0].TeamSlug != "a" || teams[1].TeamSlug != "b" {
		t.Errorf("got %+v; want [a, b] across pages", teams)
	}
}
