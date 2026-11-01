package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// acmeTeamsGraphQL is GitHub's organization.teams answer for acme (9): teams
// platform (11) and security (12).
const acmeTeamsGraphQL = `{"data":{"organization":{"databaseId":9,"teams":{
	"pageInfo":{"hasNextPage":false,"endCursor":""},
	"nodes":[
		{"databaseId":11,"slug":"platform","name":"Platform","members":{"totalCount":8}},
		{"databaseId":12,"slug":"security","name":"Security","members":{"totalCount":40}}
	]}}}}`

// TestGitHubGroupCandidates_MembershipAndCounts: ?include_membership=true lists
// every team of the tracked owners with member counts (GraphQL) and flags the
// caller's own (REST /user/teams, matched by id) so the wizard can pre-check them.
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

	resp := getGitHubGroups(t, srv, "?include_membership=true")
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

// TestGitHubGroupCandidates_CredentialsMissing: credentials_missing is true only
// when tracked repos have owners and no credential reaches any of them, so the
// wizard can tell "reconnect GitHub" from "nothing to list".
func TestGitHubGroupCandidates_CredentialsMissing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// No tracked repos: no owner to resolve, so nothing is missing.
	if resp := getGitHubGroups(t, srv, ""); resp.CredentialsMissing {
		t.Errorf("credentials_missing=true with no tracked repos; want false")
	}

	// A tracked repo gives an owner, and no credential is seeded for it.
	seedConfiguredRepo(t, srv, "acme", "api")
	resp := getGitHubGroups(t, srv, "")
	if !resp.CredentialsMissing {
		t.Errorf("credentials_missing=false with a tracked repo and no creds; want true")
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0: %+v", len(resp.Candidates), resp.Candidates)
	}
}

// TestGitHubGroupCandidates_OwnersFromTrackedReposOnly: candidates come from the
// owners of tracked repositories only, so an untracked registry row's owner is
// never asked for its teams.
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

	resp := getGitHubGroups(t, srv, "")
	if len(askedOrgs) != 1 || askedOrgs[0] != "acme" {
		t.Errorf("teams asked for owners %v; want [acme] — an untracked repository's owner is not one the org works with", askedOrgs)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].OrgLogin != "acme" {
		t.Errorf("candidates = %+v; want acme/platform alone", resp.Candidates)
	}
}

// TestUserTeamsMulti_ReconstructsViaGraphQL: userTeamsMulti queries GraphQL
// with the caller's host-verified login once per tracked-repo owner and
// de-dupes the teams. Called directly, past the multi-mode session middleware.
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

// TestUserTeamsMulti_NoLoginIsEmpty: a caller with no host-verified GitHub
// identity gets an empty list, not an error.
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

// getGitHubGroups reads the default team's github-groups, query appended to
// the path, failing unless it is a 200.
func getGitHubGroups(t *testing.T, srv *Server, query string) teamGitHubGroupsResponse {
	t.Helper()
	rec := doJSON(t, srv, http.MethodGet, "/api/teams/default/github-groups"+query, nil)
	var resp teamGitHubGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("github-groups GET%s = %d, want 200; body=%s", query, rec.Code, rec.Body.String())
	}
	return resp
}

// readGitHubGroups reads the default team's stored GitHub groups.
func readGitHubGroups(t *testing.T, srv *Server) []gitHubGroupJSON {
	t.Helper()
	return getGitHubGroups(t, srv, "").Groups
}

// TestUserTeams_WizardWriteReadBack: the wizard writes the checked teams by
// GitHub id to the replace-set endpoint, and a read-back returns that set with
// the candidates' names.
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

// twoOwnerGitHub points the org's GitHub at a server that answers acme's teams
// (acmeTeamsGraphQL) and fails every other organization's with a 503, and
// tracks a repository of acme and one of down.
func twoOwnerGitHub(t *testing.T, srv *Server) string {
	t.Helper()
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
		if req.Variables.Org != "acme" {
			// A Retry-After past the client's backoff ceiling: no retries.
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
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
	seedConfiguredRepo(t, srv, "down", "svc")
	return ts.URL
}

// assertGitHubGroupsUnconfirmed checks a PUT answered 502 UPSTREAM_UNAVAILABLE
// with no field fault, and left the stored set at acme/platform.
func assertGitHubGroupsUnconfirmed(t *testing.T, srv *Server, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	items := errorItems(t, rec)
	if len(items) != 1 || items[0].Reason != httpx.ReasonUpstreamUnavailable || items[0].Field != "" ||
		!strings.Contains(items[0].Message, "down") || !strings.Contains(items[0].Message, "99") {
		t.Errorf("errors = %+v; want one UPSTREAM_UNAVAILABLE naming owner down and team 99, with no field", items)
	}
	if got := readGitHubGroups(t, srv); len(got) != 1 || got[0].GitHubTeamID != "11" {
		t.Errorf("groups after a 502 = %+v; want acme/platform untouched", got)
	}
}

// When one owner's team listing fails, the browse list still carries the other
// owners' teams, and a team found nowhere may be the failed owner's, so it is a
// 502 rather than a 422 blaming the request.
func TestTeamGitHubGroupsPut_UnlistedOwnerIsAnUpstreamFault(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	twoOwnerGitHub(t, srv)

	putGitHubGroups(t, srv, "11")
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", map[string]any{"github_team_ids": []string{"11", "99"}})
	assertGitHubGroupsUnconfirmed(t, srv, rec)

	if resp := getGitHubGroups(t, srv, ""); len(resp.Candidates) != 2 || resp.CredentialsMissing {
		t.Errorf("GET candidates = %+v, credentials_missing=%v; want acme's two teams, false", resp.Candidates, resp.CredentialsMissing)
	}
}

// ownerFailingResolver is repositoriesByIDResolver except that no client
// resolves for one owner.
type ownerFailingResolver struct {
	repositoriesByIDResolver
	owner string
}

func (r ownerFailingResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	if target == r.owner {
		return nil, errors.New("mint installation token: connection refused")
	}
	return r.repositoriesByIDResolver.ClientFor(ctx, orgID, target)
}

// A client that fails to resolve for a reason other than a missing credential
// leaves its owner unlisted, the same as a failed listing.
func TestTeamGitHubGroupsPut_UnresolvedClientIsAnUpstreamFault(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	url := twoOwnerGitHub(t, srv)
	srv.ghResolver = ownerFailingResolver{repositoriesByIDResolver{url: url}, "down"}

	putGitHubGroups(t, srv, "11")
	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", map[string]any{"github_team_ids": []string{"11", "99"}})
	assertGitHubGroupsUnconfirmed(t, srv, rec)
}

// failingTrackedRepos is a repository store whose tracked-repository read
// fails.
type failingTrackedRepos struct{ db.RepositoryStore }

func (failingTrackedRepos) ListTrackedSystem(context.Context, string, string, db.ListOpts) ([]domain.Repository, int, error) {
	return nil, 0, errors.New("tracked repositories unreadable")
}

// With the tracked repositories unreadable there are no owners to list, so an
// added team is a 500, not a refusal of the id.
func TestTeamGitHubGroupsPut_TrackedReposReadFailureIs500(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	var calls int
	groupsCandidateServer(t, srv, &calls)
	srv.allStores.Repos = failingTrackedRepos{srv.allStores.Repos}

	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", map[string]any{"github_team_ids": []string{"11"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := readGitHubGroups(t, srv); len(got) != 0 {
		t.Errorf("groups after a 500 = %+v; want none", got)
	}
}

// An owner no connected credential reaches cannot be listed with what the org
// has, which is the caller's to fix: the added team stays a 422, naming the
// owner.
func TestTeamGitHubGroupsPut_UncredentialedOwnerIs422(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodPut, "/api/teams/default/github-groups", map[string]any{"github_team_ids": []string{"11"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	items := errorItems(t, rec)
	if len(items) != 1 || items[0].Field != "github_team_ids[0]" || !strings.Contains(items[0].Message, "reaches acme") {
		t.Errorf("errors = %+v; want github_team_ids[0] naming acme as unreached by any credential", items)
	}
}
