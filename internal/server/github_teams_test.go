package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
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

// TestUserTeams_WizardWriteReadBack is the integration-shaped check the test
// plan calls for: the wizard's Continue writes the checked teams to the
// existing per-team github-groups replace-set endpoint, and a read-back
// returns exactly that set — proving the wizard reuses the one canonical
// bulk write rather than a parallel path.
func TestUserTeams_WizardWriteReadBack(t *testing.T) {
	keyring.MockInit()
	srv := newTestServer(t)

	// The wizard POSTs the checked teams as github-groups (replace-set).
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
