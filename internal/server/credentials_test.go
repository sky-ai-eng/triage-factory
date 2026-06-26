package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleIntegrationsClear_SurfacesEnvOverlayWarning pins the
// load-bearing UX from the SecretStore sweep: when env vars supply a
// credential the DELETE handler reports success on the keychain layer
// but tells the user the env still surfaces the value. Without this
// the disconnect button looks like a silent no-op.
func TestHandleIntegrationsClear_SurfacesEnvOverlayWarning(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	// Seed keychain creds, then set env vars to provoke the warning.
	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("TRIAGE_FACTORY_GITHUB_URL", "https://env.example.com")
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "env-pat")

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "cleared" {
		t.Errorf("status field got=%v want=cleared", body["status"])
	}
	if _, ok := body["warning"]; !ok {
		t.Errorf("expected warning field surfaced for env-overlay case, body=%+v", body)
	}
	envs, _ := body["env_provided"].([]any)
	found := false
	for _, e := range envs {
		if e == "github" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("env_provided should include github, got=%v", body["env_provided"])
	}
}

// TestHandleIntegrationsClear_NoWarningWithoutEnv confirms the warning
// only fires when env vars are actually present — the happy-path
// clear response stays minimal.
func TestHandleIntegrationsClear_NoWarningWithoutEnv(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	if err := integrations.Save(ctx, s.secrets, org, auth.Credentials{
		GitHubURL: "https://github.example.com",
		GitHubPAT: "ghp-test",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got=%d want=200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["warning"]; ok {
		t.Errorf("unexpected warning field on env-free clear: %+v", body)
	}
}

// TestHandleIntegrationsDeleteJira_SurfacesJiraOnlyWarning pins the
// targeted Jira clear handler's env-overlay behavior: a GitHub env
// overlay shouldn't trigger a warning on a Jira-only clear.
func TestHandleIntegrationsDeleteJira_SurfacesJiraOnlyWarning(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodDelete, "/api/integrations/jira", nil)
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

// TestHandleIntegrationsSetup_PersistsOrgGitHubLogin pins TFAC-452's write path
// through the setup handler: a validated org PAT persists the credential's own
// login on agents.github_org_login (so OrgIdentityFor can stamp the org commit
// identity) WITHOUT writing user_github_identities — org access and per-user
// identity stay independent. A fake GitHub host returns the login so no real
// network call is made.
func TestHandleIntegrationsSetup_PersistsOrgGitHubLogin(t *testing.T) {
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
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gh.Close()

	rec := doJSON(t, s, http.MethodPost, "/api/integrations/setup", map[string]string{
		"github_url":     gh.URL,
		"github_pat":     "ghp_test",
		"clone_protocol": "https",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// agents.github_org_login is the validated login.
	var login sql.NullString
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT github_org_login FROM agents WHERE org_id = ?`, org,
	).Scan(&login); err != nil {
		t.Fatalf("read agents.github_org_login: %v", err)
	}
	if login.String != "acme-bot" {
		t.Errorf("agents.github_org_login = %q, want acme-bot", login.String)
	}

	// ...and per-user identity is untouched: setup writes ACCESS, not identity.
	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM user_github_identities`,
	).Scan(&n); err != nil {
		t.Fatalf("count user_github_identities: %v", err)
	}
	if n != 0 {
		t.Errorf("user_github_identities has %d rows; setup must not bind per-user identity", n)
	}
}
