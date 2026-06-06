package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

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
			_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},
				"nodes":[
					{"slug":"platform","name":"Platform","members":{"totalCount":8}},
					{"slug":"security","name":"Security","members":{"totalCount":40}}
				]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/user/teams"):
			// Membership: the caller is on platform only.
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`[{"slug":"platform","name":"Platform","members_count":8,"organization":{"login":"acme"}}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	if err := srv.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups?include_membership=true", nil)
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
	if p := byKey["acme/platform"]; !p.Mine || p.MemberCount != 8 {
		t.Errorf("platform = %+v; want Mine=true, MemberCount=8", p)
	}
	if s := byKey["acme/security"]; s.Mine || s.MemberCount != 40 {
		t.Errorf("security = %+v; want Mine=false, MemberCount=40", s)
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

	rec := doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups", nil)
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

	if err := srv.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")

	rec := doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups", nil)
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

// TestGitHubGroupCandidates_NoReposNotMissing guards the inverse: with no
// tracked repos there is no owner to resolve, so credentials_missing must stay
// false — an unconfigured team is not a creds failure, and the wizard reaches
// this step only after repos are persisted anyway.
func TestGitHubGroupCandidates_NoReposNotMissing(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	rec := doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups", nil)
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
// configured-repo owner, and de-dupes the result. Calling the method
// rather than the HTTP handler sidesteps the multi-mode session/claims
// middleware the local-mode test harness doesn't stand up, while still
// exercising the membership-reconstruction logic the reviewer flagged.
func TestUserTeamsMulti_ReconstructsViaGraphQL(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ctx := context.Background()

	var gotLogin, gotOrg string
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
		gotLogin, gotOrg = req.Variables.Login, req.Variables.Org
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"organization":{"teams":{
			"pageInfo":{"hasNextPage":false,"endCursor":""},
			"nodes":[
				{"slug":"platform","name":"Platform","members":{"totalCount":8}},
				{"slug":"security","name":"Security","members":{"totalCount":40}}
			]}}}}`))
	}))
	t.Cleanup(ts.Close)

	// Org base URL + PAT both point at the stub so ghResolver.ClientFor
	// resolves a tier-3 PAT-borrow client aimed at our GraphQL endpoint.
	if err := srv.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: ts.URL}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
	if err := integrations.Save(ctx, srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
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
	if err := srv.users.UpsertGitHubIdentity(ctx, runmode.LocalDefaultUserID, host, "octocat", "connect_oauth"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	// A configured repo under owner "acme" is the org the fan-out queries.
	seedConfiguredRepo(t, srv, "acme", "api")

	teams, err := srv.userTeamsMulti(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("userTeamsMulti: %v", err)
	}
	if gotLogin != "octocat" || gotOrg != "acme" {
		t.Errorf("GraphQL query vars = (%q, %q); want (octocat, acme)", gotLogin, gotOrg)
	}
	if len(teams) != 2 {
		t.Fatalf("got %d teams, want 2: %+v", len(teams), teams)
	}
	if teams[0].OrgSlug != "acme" || teams[0].TeamSlug != "platform" || teams[0].MemberCount != 8 {
		t.Errorf("teams[0] = %+v; want acme/platform 8", teams[0])
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
	if err := srv.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: "https://github.example.com"}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
	seedConfiguredRepo(t, srv, "acme", "api")

	teams, err := srv.userTeamsMulti(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	if err != nil {
		t.Fatalf("userTeamsMulti with no identity should not error: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("got %d teams, want 0 (no bound login)", len(teams))
	}
}

// TestUserTeams_WizardWriteReadBack is the integration-shaped check the test
// plan calls for: the wizard's Continue writes the checked teams to the
// existing per-team github-groups replace-set endpoint, and a read-back
// returns exactly that set — proving the wizard reuses the one canonical
// bulk write rather than a parallel path.
func TestUserTeams_WizardWriteReadBack(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// The wizard sends the checked teams as github-groups via PUT (replace-set).
	rec := doJSON(t, srv, http.MethodPut, "/api/settings/team/default/github-groups", map[string]any{
		"groups": []map[string]string{
			{"org_login": "acme", "team_slug": "platform"},
			{"org_login": "acme", "team_slug": "security"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("github-groups PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Read-back: the stored mappings round-trip.
	rec = doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("github-groups GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Groups []gitHubGroupJSON `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode groups: %v; raw=%s", err, rec.Body.String())
	}
	if len(resp.Groups) != 2 {
		t.Fatalf("got %d stored groups, want 2: %+v", len(resp.Groups), resp.Groups)
	}

	// Idempotent re-run with a subset replaces the set (replace-within-team
	// semantics), proving Skip-then-configure-later and re-onboarding are safe.
	rec = doJSON(t, srv, http.MethodPut, "/api/settings/team/default/github-groups", map[string]any{
		"groups": []map[string]string{{"org_login": "acme", "team_slug": "platform"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("github-groups replace PUT = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, srv, http.MethodGet, "/api/settings/team/default/github-groups", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode groups after replace: %v", err)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].TeamSlug != "platform" {
		t.Errorf("after replace got %+v; want only acme/platform", resp.Groups)
	}
}
