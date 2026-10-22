package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// acmeTeamsGraphQL is GitHub's organization.teams answer for acme (9): teams
// platform (11) and security (12).
const acmeTeamsGraphQL = `{"data":{"organization":{"databaseId":9,"teams":{
	"pageInfo":{"hasNextPage":false,"endCursor":""},
	"nodes":[
		{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}},
		{"databaseId":12,"slug":"security","name":"Security","members":{"totalCount":40}}
	]}}}}`

// TestGitHubGroupCandidates_MembershipAndCounts exercises the candidate path
// the onboarding wizard now reads: GET .../github-groups?include_membership=true
// returns the org's *full* team list (via the GraphQL ListOrgTeamsDetailed,
// carrying member counts) and flags the teams the caller personally belongs
// to (Mine) so the wizard can pre-check them — the same list the Settings
// editor reads, differing only in the pre-check. Local mode: candidates come
// from GraphQL organization.teams; membership from REST /user/teams.
func TestGitHubGroupCandidates_MembershipAndCounts(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			// Candidates: organization.teams (no userLogins filter) — the
			// org's full team list with member counts.
			_, _ = w.Write([]byte(acmeTeamsGraphQL))
		case strings.HasSuffix(r.URL.Path, "/user/teams"):
			// Membership: the caller is on platform only, listed under a slug
			// the candidate list no longer uses, so it matches by id.
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`[{"id":11,"slug":"platform-old","name":"Platform","members_count":8,"organization":{"id":9,"login":"acme"}}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL})
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups?include_membership=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET github-groups = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	byKey := map[string]gitHubGroupCandidateJSON{}
	for _, c := range resp.Candidates {
		byKey[c.OrgLogin+"/"+c.TeamSlug] = c
	}
	if len(byKey) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(byKey), resp.Candidates)
	}
	if p := byKey["acme/platform"]; !p.Mine || p.MemberCount != 8 || p.GitHubTeamID != "11" || p.GitHubOrgID != "9" {
		t.Errorf("platform = %+v; want ids 11/9, Mine=true, MemberCount=8", p)
	}
	if s := byKey["acme/security"]; s.Mine || s.MemberCount != 40 || s.GitHubTeamID != "12" {
		t.Errorf("security = %+v; want id 12, Mine=false, MemberCount=40", s)
	}
}

// TestGitHubGroupCandidates_CredentialsMissing covers the disambiguation the
// setup wizard relies on: a team that tracks repos but whose org has NO
// resolvable GitHub credential must report credentials_missing=true with an
// empty candidate list, so the UI can tell "reconnect GitHub" apart from
// "these orgs have no teams." A tracked repo exists (so there's an owner to
// resolve) but no creds are seeded, so ClientFor returns ErrNoGitHubCredentials
// for the sole owner.
func TestGitHubGroupCandidates_CredentialsMissing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// Track a repo (gives the candidate path an owner to resolve) but seed
	// no GitHub credential — the org PAT is absent and there's no App.
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET github-groups = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if !resp.CredentialsMissing {
		t.Errorf("credentials_missing=false with a tracked repo and no creds; want true")
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0: %+v", len(resp.Candidates), resp.Candidates)
	}
}

// TestGitHubGroupCandidates_ResolvedButNoTeams is the load-bearing complement
// to the missing-creds case: an empty candidate list with a WORKING credential
// must report credentials_missing=false. This is the exact disambiguation the
// flag exists for — "this org's teams are empty" must not read as "reconnect
// GitHub." The owner resolves via the org PAT but GraphQL returns no teams.
func TestGitHubGroupCandidates_ResolvedButNoTeams(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			// Owner resolves (PAT), but the org genuinely has no teams.
			_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)

	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL})
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET github-groups = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if resp.CredentialsMissing {
		t.Errorf("credentials_missing=true with a working credential and no teams; want false")
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0: %+v", len(resp.Candidates), resp.Candidates)
	}
}

// TestGitHubGroupCandidates_OwnersFromTrackedReposOnly pins where the owners
// come from: the repositories a team tracks, not the whole registry. A
// registry row no team tracks (a GitHub entity's repository gets one) names
// an owner the org does not work with, so the credential is never asked for
// that owner's teams and none of them become candidates.
func TestGitHubGroupCandidates_OwnersFromTrackedReposOnly(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()

	var askedOrgs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Variables struct {
				Org string `json:"org"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		askedOrgs = append(askedOrgs, req.Variables.Org)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"organization":{"databaseId":9,"teams":{
			"pageInfo":{"hasNextPage":false,"endCursor":""},
			"nodes":[{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}}]}}}}`))
	}))
	t.Cleanup(ts.Close)

	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL})
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")
	seedUntrackedRepo(t, srv, "other", "thing")

	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET github-groups = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if len(askedOrgs) != 1 || askedOrgs[0] != "acme" {
		t.Errorf("teams asked for owners %v; want [acme] — an untracked repository's owner is not one the org works with", askedOrgs)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].OrgLogin != "acme" {
		t.Errorf("candidates = %+v; want acme/platform alone", resp.Candidates)
	}
}

// TestGitHubGroupCandidates_NoReposNotMissing guards the inverse: with no
// tracked repos there is no owner to resolve, so credentials_missing must stay
// false — an unconfigured team is not a creds failure, and the wizard reaches
// this step only after repos are persisted anyway.
func TestGitHubGroupCandidates_NoReposNotMissing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET github-groups = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if resp.CredentialsMissing {
		t.Errorf("credentials_missing=true with no tracked repos; want false")
	}
}

// TestUserTeamsMulti_ReconstructsViaGraphQL drives the multi-mode path
// (userTeamsMulti) directly: it resolves the caller's host-verified login,
// fans out one GraphQL organization.teams(userLogins:) query per
// tracked-repo owner (an untracked registry row's owner is not asked), and
// de-dupes the result. Calling the method
// rather than the HTTP handler sidesteps the multi-mode session/claims
// middleware the local-mode test harness doesn't stand up, while still
// exercising the membership-reconstruction logic the reviewer flagged.
func TestUserTeamsMulti_ReconstructsViaGraphQL(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()

	var (
		gotLogin string
		gotOrgs  []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			// patBorrowUser's whoami (logging only) and anything else can
			// 404 harmlessly — the team resolution only needs GraphQL.
			http.NotFound(w, r)
			return
		}
		var req struct {
			Variables struct {
				Login string `json:"login"`
				Org   string `json:"org"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotLogin = req.Variables.Login
		gotOrgs = append(gotOrgs, req.Variables.Org)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(acmeTeamsGraphQL))
	}))
	t.Cleanup(ts.Close)

	// Org base URL + PAT both point at the stub so ghResolver.ClientFor
	// resolves a tier-3 PAT-borrow client aimed at our GraphQL endpoint.
	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL})
	seedSourceHost(t, srv, eventsource.KindGitHub, ts.URL)
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	// The caller's host-verified GitHub login, keyed on the same host
	// resolveGitHubHost derives from the org base URL.
	host, ok := resolveGitHubHost(ts.URL)
	if !ok {
		t.Fatalf("resolveGitHubHost(%q) failed", ts.URL)
	}
	if err := srv.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "", "", "connect_oauth"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	// A tracked repo under owner "acme" is the org the fan-out queries. The
	// registry row no team tracks names an owner the fan-out skips.
	seedConfiguredRepo(t, srv, "acme", "api")
	seedUntrackedRepo(t, srv, "other", "thing")

	teams, err := srv.userTeamsMulti(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("userTeamsMulti: %v", err)
	}
	if gotLogin != "octocat" || len(gotOrgs) != 1 || gotOrgs[0] != "acme" {
		t.Errorf("GraphQL queries = login %q, orgs %v; want octocat, [acme]", gotLogin, gotOrgs)
	}
	if len(teams) != 2 {
		t.Fatalf("got %d teams, want 2: %+v", len(teams), teams)
	}
	if teams[0].OrgSlug != "acme" || teams[0].TeamSlug != "platform" || teams[0].MemberCount != 8 || teams[0].ID != "11" || teams[0].OrgID != "9" {
		t.Errorf("teams[0] = %+v; want acme/platform 8, ids 11/9", teams[0])
	}
	if teams[1].TeamSlug != "security" || teams[1].MemberCount != 40 {
		t.Errorf("teams[1] = %+v; want security 40", teams[1])
	}
}

// TestUserTeamsMulti_NoLoginIsEmpty proves the multi path degrades to an
// empty list (not an error) when the caller has no host-verified GitHub
// identity — the user can still Skip the step.
func TestUserTeamsMulti_NoLoginIsEmpty(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()
	dbtest.SeedOrgSettings(t, srv.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: "https://github.example.com"})
	seedConfiguredRepo(t, srv, "acme", "api")

	teams, err := srv.userTeamsMulti(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("userTeamsMulti with no identity should not error: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("got %d teams, want 0 (no bound login)", len(teams))
	}
}

// groupsCandidateServer points the org's GitHub at a server answering
// acmeTeamsGraphQL, counting the reads in calls.
func groupsCandidateServer(t *testing.T, srv *Server, calls *int) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			http.NotFound(w, r)
			return
		}
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(acmeTeamsGraphQL))
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
}

// putGitHubGroups writes ids as the default team's GitHub groups and returns
// the groups it answers, failing unless it is a 200.
func putGitHubGroups(t *testing.T, srv *Server, ids ...string) []gitHubGroupJSON {
	t.Helper()
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", map[string]any{"github_team_ids": ids})
	var out teamGitHubGroupsPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("github-groups PUT %v = %d, want 200; body=%s", ids, rec.Code, rec.Body.String())
	}
	return out.Groups
}

// readGitHubGroups reads the default team's stored GitHub groups.
func readGitHubGroups(t *testing.T, srv *Server) []gitHubGroupJSON {
	t.Helper()
	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups", nil)
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("github-groups GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return resp.Groups
}

// TestUserTeams_WizardWriteReadBack: the wizard's Continue writes the checked
// teams, by GitHub's ids, to the per-team github-groups replace-set endpoint,
// and a read-back returns exactly that set with the names the candidates gave
// them.
func TestUserTeams_WizardWriteReadBack(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var calls int
	groupsCandidateServer(t, srv, &calls)

	want := []gitHubGroupJSON{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "platform"},
		{GitHubTeamID: "12", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "security"},
	}
	if got := putGitHubGroups(t, srv, "11", "12"); !reflect.DeepEqual(got, want) {
		t.Errorf("PUT echoed %+v; want %+v", got, want)
	}
	if got := readGitHubGroups(t, srv); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored groups = %+v, want %+v", got, want)
	}

	// A re-run with a subset replaces the set, so Skip-then-configure-later
	// and re-onboarding are safe. Every id it names is already held, so
	// GitHub is not read.
	before := calls
	putGitHubGroups(t, srv, "11")
	if calls != before {
		t.Errorf("a PUT naming only held teams read GitHub %d time(s); want none", calls-before)
	}
	if got := readGitHubGroups(t, srv); len(got) != 1 || got[0].GitHubTeamID != "11" {
		t.Errorf("after replace got %+v; want only acme/platform", got)
	}
}

// A body of the wrong shape is a 400 naming every bad entry, an id no
// candidate carries is a 422 naming its entry, and nothing is written.
func TestTeamGitHubGroupsPut_RefusesWhatItCannotResolve(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var calls int
	groupsCandidateServer(t, srv, &calls)
	putGitHubGroups(t, srv, "11")

	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		fields []string
	}{
		{"no field", map[string]any{}, http.StatusBadRequest, []string{"github_team_ids"}},
		{"the handle shape", map[string]any{"groups": []map[string]string{{"org_login": "acme", "team_slug": "platform"}}}, http.StatusBadRequest, []string{"groups"}},
		{"malformed ids", map[string]any{"github_team_ids": []string{"", "acme/platform", "011", "11", "11"}}, http.StatusBadRequest,
			[]string{"github_team_ids[0]", "github_team_ids[1]", "github_team_ids[2]", "github_team_ids[4]"}},
		{"a team no candidate carries", map[string]any{"github_team_ids": []string{"11", "99"}}, http.StatusUnprocessableEntity, []string{"github_team_ids[1]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("PUT = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := errorFields(t, rec); !reflect.DeepEqual(got, tc.fields) {
				t.Errorf("error fields = %v, want %v", got, tc.fields)
			}
			if got := readGitHubGroups(t, srv); len(got) != 1 || got[0].GitHubTeamID != "11" {
				t.Errorf("groups after a refused PUT = %+v; want acme/platform untouched", got)
			}
		})
	}
}
