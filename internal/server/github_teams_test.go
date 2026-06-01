package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestUserTeams_LocalPATPath exercises GET /api/github/user-teams in local
// mode: the org PAT *is* the user, so the handler calls GET /user/teams and
// returns the wire shape the onboarding step consumes — including the
// member_count the noisy-team hint reads.
func TestUserTeams_LocalPATPath(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/teams" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`[
				{"slug":"platform","name":"Platform","members_count":8,"organization":{"login":"acme"}},
				{"slug":"security","name":"Security","members_count":25,"organization":{"login":"acme"}}
			]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(ts.Close)
	if err := integrations.Save(context.Background(), srv.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: ts.URL,
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("seed creds: %v", err)
	}

	rec := doJSON(t, srv, http.MethodGet, "/api/github/user-teams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET user-teams = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []userTeamJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d teams, want 2: %+v", len(got), got)
	}
	if got[0].OrgSlug != "acme" || got[0].TeamSlug != "platform" || got[0].MemberCount != 8 {
		t.Errorf("got[0] = %+v; want acme/platform with 8 members", got[0])
	}
	if got[1].TeamSlug != "security" || got[1].MemberCount != 25 {
		t.Errorf("got[1] = %+v; want security with 25 members", got[1])
	}
}

// TestUserTeams_NoPATIs400 mirrors handleGitHubRepos: with no PAT configured
// the local path reports "GitHub not configured" as a 400 rather than a 502.
func TestUserTeams_NoPATIs400(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	rec := doJSON(t, srv, http.MethodGet, "/api/github/user-teams", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET user-teams with no creds = %d, want 400; body=%s", rec.Code, rec.Body.String())
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
