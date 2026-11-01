package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestIntegrationsStatus_SetupCompleteGate pins the server-side gate the
// AuthGate keys on: setup_step walks org → team → done as GitHub access and a
// tracked repo land, and it counts the tracked set, never the registry.
func TestIntegrationsStatus_SetupCompleteGate(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	check := func(stage string, wantRepos float64, wantComplete bool, wantStep string) map[string]any {
		t.Helper()
		st := getIntegrationsStatus(t, s)
		if st["github_repos"] != wantRepos || st["setup_complete"] != wantComplete || st["setup_step"] != wantStep {
			t.Errorf("%s: github_repos=%v setup_complete=%v setup_step=%v; want %v, %v, %v",
				stage, st["github_repos"], st["setup_complete"], st["setup_step"], wantRepos, wantComplete, wantStep)
		}
		return st
	}

	check("fresh org", 0, false, "org")

	// GitHub configured (URL + PAT) but still no tracked repo → resume at team.
	seedSourceHost(t, s, eventsource.KindGitHub, "https://github.com")
	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save creds: %v", err)
	}
	if st := check("GitHub and no repos", 0, false, "team"); st["github_ready"] != true {
		t.Errorf("github_ready=%v after PAT saved; want true", st["github_ready"])
	}

	// A registry row no team tracks (a GitHub entity's repository gets one)
	// neither completes setup nor shows in github_repos.
	seedUntrackedRepo(t, s, "other", "thing")
	check("only an untracked registry row", 0, false, "team")

	seedConfiguredRepo(t, s, "acme", "widgets")
	check("one tracked repo beside the untracked row", 1, true, "done")

	// Untracking every repository resumes at team; both registry rows stay.
	if err := dbtest.TrackRepos(ctx, s.allStores.Repos, s.allStores.TeamGitHubRepos, org, runmode.LocalDefaultTeamID, seedRepoHost(t, s), nil); err != nil {
		t.Fatalf("untrack every repo: %v", err)
	}
	check("every repo untracked", 0, false, "team")
	if _, n, err := s.allStores.Repos.List(ctx, org, seedRepoHost(t, s), db.ListOpts{CountOnly: true}); err != nil || n != 2 {
		t.Errorf("registry rows after untracking = %d, %v; want 2", n, err)
	}
}

// getIntegrationsStatus GETs /api/integrations/status and decodes the body.
func getIntegrationsStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/integrations/status", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return body
}

// seedLocalGitHubApp inserts an org_github_apps row for the local org, the
// App-based alternative to a PAT. An inactive App must not count as configured
// GitHub access, so `active` sets the row's flag.
func seedLocalGitHubApp(t *testing.T, s *Server, clientID string, active bool) {
	t.Helper()
	act := 0
	if active {
		act = 1
	}
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO org_github_apps (
			org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref, active
		) VALUES (?, ?, 'tf-app', ?, 'cs', 'pem', 'wh', ?)
		ON CONFLICT(org_id) DO UPDATE SET
			client_id = excluded.client_id,
			active    = excluded.active
	`, runmode.LocalDefaultOrgID, "app-"+clientID, clientID, act); err != nil {
		t.Fatalf("seed org_github_apps: %v", err)
	}
	// Registration writes the credential class with the row; the setup gate
	// reads the class to decide whether there is an App to count at all.
	seedBYOAppCredentialClass(t, s, runmode.LocalDefaultOrgID)
}

// TestIntegrationsStatus_SetupCompleteGate_AppPath: a registered, active GitHub
// App satisfies the GitHub step without a PAT, so an App org walks the same
// team → done progression.
func TestIntegrationsStatus_SetupCompleteGate_AppPath(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)

	// Active App, no PAT: github (PAT-only display) stays false, but
	// github_ready is true off the App alone. No repo yet → resume at team.
	seedLocalGitHubApp(t, s, "client-xyz", true)
	st := getIntegrationsStatus(t, s)
	if st["github"] != false {
		t.Errorf("github=%v with an App but no PAT; want false (PAT-only signal)", st["github"])
	}
	if st["github_ready"] != true {
		t.Errorf("github_ready=%v with an active App; want true", st["github_ready"])
	}
	if st["setup_complete"] != false {
		t.Errorf("setup_complete=%v with an App but no repo; want false", st["setup_complete"])
	}
	if st["setup_step"] != "team" {
		t.Errorf("setup_step=%v with an App and no repo; want team", st["setup_step"])
	}

	// A tracked repo completes setup via the App path.
	seedConfiguredRepo(t, s, "acme", "widgets")
	st = getIntegrationsStatus(t, s)
	if st["setup_complete"] != true {
		t.Errorf("setup_complete=%v with App + repo; want true", st["setup_complete"])
	}
	if st["setup_step"] != "done" {
		t.Errorf("setup_step=%v when complete; want done", st["setup_step"])
	}
}

// TestIntegrationsStatus_InactiveAppNotReady: an inactive App row (a stale
// registration) is not configured GitHub access. GetForOrg does not filter on
// active, so the status handler must.
func TestIntegrationsStatus_InactiveAppNotReady(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)

	seedLocalGitHubApp(t, s, "client-xyz", false)
	st := getIntegrationsStatus(t, s)
	if st["github_ready"] != false {
		t.Errorf("github_ready=%v with only an INACTIVE App; want false", st["github_ready"])
	}
	if st["setup_step"] != "org" {
		t.Errorf("setup_step=%v with only an inactive App; want org", st["setup_step"])
	}
}

// TestGitHubPATPut_PersistsOrgGitHubIdentity: a validated org PAT stores the
// account it authenticates as (login, numeric id, email) on the org's agents
// row, where the org commit identity is read, and writes no per-user identity.
func TestGitHubPATPut_PersistsOrgGitHubIdentity(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	org := runmode.LocalDefaultOrgID

	// Fake GHE host: ValidateGitHub hits {base}/api/v3/user for non-github.com.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "acme-bot", "id": 4242})
			return
		}
		if r.URL.Path == "/api/v3/user/emails" {
			writeGitHubPrimaryEmail(w, "acme-bot@example.com")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gh.Close()

	rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_test")
	if rec.Code != http.StatusOK {
		t.Fatalf("pat bind status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var login, userID, email sql.NullString
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT github_org_login, github_org_user_id, github_org_email FROM agents WHERE org_id = ?`, org,
	).Scan(&login, &userID, &email); err != nil {
		t.Fatalf("read agents GitHub identity: %v", err)
	}
	if login.String != "acme-bot" || userID.String != "4242" || email.String != "acme-bot@example.com" {
		t.Errorf("agents GitHub identity = %q (%q) <%s>, want acme-bot (4242) <acme-bot@example.com>", login.String, userID.String, email.String)
	}

	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM user_github_identities`,
	).Scan(&n); err != nil {
		t.Fatalf("count user_github_identities: %v", err)
	}
	if n != 0 {
		t.Errorf("user_github_identities has %d rows; the org bind must not write per-user identity", n)
	}
}

// TestSetupModelPicks: multi-mode setup requires the org's background-jobs
// model and the default team's model, each reported on its own; local requires
// neither. The predicate is called directly, since the route's other inputs decide nothing.
func TestSetupModelPicks(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)

	picks := func() (orgPick, teamPick bool) {
		t.Helper()
		if err := s.tx.WithTx(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
			func(tx db.TxStores) error {
				var e error
				orgPick, teamPick, e = setupModelPicks(t.Context(), tx, runmode.LocalDefaultOrgID)
				return e
			}); err != nil {
			t.Fatalf("setupModelPicks: %v", err)
		}
		return orgPick, teamPick
	}

	setBackgroundJobsModel(t, s, "claude-haiku-4-5-20251001")
	setTeamDefaultModel(t, s, "claude-sonnet-5")
	if org, team := picks(); !org || !team {
		t.Fatalf("picks = (org %v, team %v) with both stored; want both true", org, team)
	}

	// Each missing pick is reported on its own, because they are two different
	// screens and the resume point has to name the one that is missing.
	setBackgroundJobsModel(t, s, "")
	if org, team := picks(); org || !team {
		t.Errorf("picks = (org %v, team %v) with no background-jobs model; want (false, true)", org, team)
	}

	setBackgroundJobsModel(t, s, "claude-haiku-4-5-20251001")
	setTeamDefaultModel(t, s, "")
	if org, team := picks(); !org || team {
		t.Errorf("picks = (org %v, team %v) with no team default model; want (true, false)", org, team)
	}

	// Local pre-fills both from its own column defaults and never blocks on
	// them. Both are still cleared here.
	runmode.SetForTest(t, runmode.ModeLocal)
	if org, team := picks(); !org || !team {
		t.Errorf("local blocked on model picks (org %v, team %v); it gates on neither", org, team)
	}
}

// TestIntegrationsStatus_SetupCompleteGate_ModelPicksLocal pins that the gate
// the AuthGate actually keys on carries that local arm end to end: an install
// with neither pick stored still finishes setup.
func TestIntegrationsStatus_SetupCompleteGate_ModelPicksLocal(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)

	seedSourceHost(t, s, eventsource.KindGitHub, "https://github.com")
	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save creds: %v", err)
	}
	seedConfiguredRepo(t, s, "acme", "widgets")
	setBackgroundJobsModel(t, s, "")
	setTeamDefaultModel(t, s, "")

	if st := getIntegrationsStatus(t, s); st["setup_complete"] != true {
		t.Errorf("setup_complete=%v in local with neither pick stored; want true", st["setup_complete"])
	}
}

// setBackgroundJobsModel writes org_settings.background_jobs_model directly.
// The PATCH route refuses a model this deployment does not offer, and these
// fixtures need the EMPTY state a fresh multi org is provisioned into, which no
// route can produce.
func setBackgroundJobsModel(t *testing.T, s *Server, model string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO org_settings (org_id, background_jobs_model) VALUES (?, ?)
		ON CONFLICT(org_id) DO UPDATE SET background_jobs_model = excluded.background_jobs_model
	`, runmode.LocalDefaultOrgID, model); err != nil {
		t.Fatalf("seed background_jobs_model: %v", err)
	}
}

// setTeamDefaultModel writes team_settings.default_model for the default team
// directly, for the same reason: the team PATCH refuses to clear it.
func setTeamDefaultModel(t *testing.T, s *Server, model string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO team_settings (team_id, default_model) VALUES (?, ?)
		ON CONFLICT(team_id) DO UPDATE SET default_model = excluded.default_model
	`, runmode.LocalDefaultTeamID, model); err != nil {
		t.Fatalf("seed team default_model: %v", err)
	}
}
