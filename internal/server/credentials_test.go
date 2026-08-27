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
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestJiraCredentialDelete_SurfacesJiraOnlyWarning pins the Jira unbind's
// env-overlay behavior: the delete succeeds, but a TRIAGE_FACTORY_JIRA_* pair
// keeps supplying the credential on read, so the response has to say so rather
// than report a clean disconnect the operator can see isn't one.
func TestJiraCredentialDelete_SurfacesJiraOnlyWarning(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		JiraURL: "https://jira.example.com",
		JiraPAT: "jira-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_JIRA_URL", "https://env.example.com")
	t.Setenv("TRIAGE_FACTORY_JIRA_BOT_PAT", "env-jira-pat")

	req := httptest.NewRequest(http.MethodDelete,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/jira/access/credential", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["warning"]; !ok {
		t.Errorf("expected warning on env-overlay Jira clear, body=%+v", body)
	}
}

// TestIntegrationsStatus_SetupCompleteGate pins the mandatory-configuration
// gate the AuthGate keys on: a provisioned-but-unconfigured org is NOT
// setup_complete, and setup_step walks org → team → done as GitHub access
// and a tracked repo land. This is the server-authoritative signal that
// makes "I'll configure later" unreachable — the frontend can't fake it.
func TestIntegrationsStatus_SetupCompleteGate(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	getStatus := func() map[string]any { return getIntegrationsStatus(t, s) }

	// Provisioned tenant, no GitHub creds, no repos → incomplete, resume at org.
	st := getStatus()
	if st["setup_complete"] != false {
		t.Errorf("setup_complete=%v on a fresh org; want false", st["setup_complete"])
	}
	if st["setup_step"] != "org" {
		t.Errorf("setup_step=%v with no GitHub; want org", st["setup_step"])
	}

	// GitHub configured (PAT) but still no tracked repo → resume at team.
	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save creds: %v", err)
	}
	st = getStatus()
	if st["github_ready"] != true {
		t.Errorf("github_ready=%v after PAT saved; want true", st["github_ready"])
	}
	if st["setup_complete"] != false {
		t.Errorf("setup_complete=%v with GitHub but no repos; want false", st["setup_complete"])
	}
	if st["setup_step"] != "team" {
		t.Errorf("setup_step=%v with GitHub and no repos; want team", st["setup_step"])
	}

	// A tracked repo lands → setup is complete, gate opens.
	seedConfiguredRepo(t, s, "acme", "widgets")
	st = getStatus()
	if st["setup_complete"] != true {
		t.Errorf("setup_complete=%v with GitHub + a repo; want true", st["setup_complete"])
	}
	if st["setup_step"] != "done" {
		t.Errorf("setup_step=%v when complete; want done", st["setup_step"])
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

// seedGitHubApp inserts an org_github_apps row for the local org so tests can
// exercise the App-based GitHub access path (the multi-mode alternative to a
// PAT). `active` toggles the row's active flag — an inactive App must not
// count as configured GitHub access.
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

// TestIntegrationsStatus_SetupCompleteGate_AppPath pins that a registered,
// active GitHub App satisfies the GitHub step on its own — no PAT — so an
// App-based (multi-mode) org walks the same team → done progression. Guards
// the appRegistered branch the PAT-path test never touches.
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

// TestIntegrationsStatus_InactiveAppNotReady pins that an inactive App row
// (a stale registration left by an uninstall/reinstall cycle) does NOT count
// as configured GitHub access — GetForOrg doesn't filter on active, so the
// status handler must, or a deactivated App would falsely complete setup.
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

// TestGitHubPATPut_PersistsOrgGitHubLogin pins TFAC-452's write path through
// the org PAT bind: a validated org PAT persists the credential's own login on
// agents.github_org_login (so OrgIdentityFor can stamp the org commit identity)
// WITHOUT writing user_github_identities — org access and per-user identity stay
// independent. A fake GitHub host returns the login so no real network call is
// made.
//
// It used to be pinned through the fused setup route, which bound the PAT as one
// of the five things it did; the wizard drives this route now, so this is where
// the property lives.
func TestGitHubPATPut_PersistsOrgGitHubIdentity(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	org := runmode.LocalDefaultOrgID

	// Fake GHE host: ValidateGitHub hits {base}/api/v3/user for non-github.com.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "acme-bot"})
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

	// The validated account identity is captured as a pair.
	var login, email sql.NullString
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT github_org_login, github_org_email FROM agents WHERE org_id = ?`, org,
	).Scan(&login, &email); err != nil {
		t.Fatalf("read agents GitHub identity: %v", err)
	}
	if login.String != "acme-bot" || email.String != "acme-bot@example.com" {
		t.Errorf("agents GitHub identity = %q <%s>, want acme-bot <acme-bot@example.com>", login.String, email.String)
	}

	// ...and per-user identity is untouched: the bind writes ACCESS, not identity.
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

// TestSetupModelPicks pins the two model picks multi-mode setup requires before
// the completeness gate opens: the org's background-jobs model and the default
// team's default model. TF ships no fallback for either, so an org
// through the gate without them has background jobs that never run and a team
// whose unpinned steps refuse at dispatch — and neither says so at the moment
// it happens.
//
// The predicate is exercised directly rather than through the status route,
// because the route's other inputs (a GitHub credential, a tracked repo, a
// session) decide nothing here and standing them up would only obscure which
// input moved the answer.
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

	// The other half of the asymmetry: local pre-fills both from its own column
	// defaults and never blocks on them, so a zero-configuration first run is
	// not held on a question nobody was asked. Both are still cleared here.
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

	if err := integrations.Save(t.Context(), s.secrets, runmode.LocalDefaultOrgID, auth.Credentials{
		GitHubURL: "https://github.com",
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
