package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// orgCredentialClass reads the org's stored class straight from the settings
// row — the assertion every transition test below makes.
func orgCredentialClass(t *testing.T, s *Server) domain.GitHubCredentialClass {
	t.Helper()
	set, err := s.orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	return set.GitHubCredentialClass
}

func assertCredentialClass(t *testing.T, s *Server, want domain.GitHubCredentialClass, afterWhat string) {
	t.Helper()
	if got := orgCredentialClass(t, s); got != want {
		t.Errorf("credential class after %s = %q; want %q", afterWhat, got, want)
	}
}

// TestCredentialClass_PATBind: a fresh org is in the PAT class with nothing
// bound, and binding a token keeps it there.
func TestCredentialClass_PATBind(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "provisioning a fresh org")

	rec := bindOrgGitHubPAT(t, s, stub.URL, "ghp_valid")
	if rec.Code != http.StatusOK {
		t.Fatalf("bind pat = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "a PAT bind")
}

// unknownCredentialClass is a class no build defines, so the refusing arms it
// exercises can never be implemented out from under the tests.
const unknownCredentialClass = domain.GitHubCredentialClass("not-a-credential-class")

// TestCredentialClass_PATBindRefusedForUnknownClass: an org in a class this
// build cannot name, with no App row, is refused a PAT bind rather than moved
// into the PAT class on the strength of the missing row.
func TestCredentialClass_PATBindRefusedForUnknownClass(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	ctx := context.Background()

	if _, err := s.orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, unknownCredentialClass); err != nil {
		t.Fatalf("seed unknown class: %v", err)
	}

	rec := bindOrgGitHubPAT(t, s, stub.URL, "ghp_valid")
	if rec.Code != http.StatusConflict {
		t.Fatalf("bind pat under an unknown class = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Nothing was written — not the token, and not the class.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("a refused bind stored a PAT: %q", v)
	}
	if got := orgCredentialClass(t, s); got != unknownCredentialClass {
		t.Errorf("a refused bind rewrote the class to %q; want it untouched", got)
	}
}

// TestCredentialClass_ImportStaged: an imported App staged behind a live PAT
// writes the BYO-App class while the row stays inactive.
func TestCredentialClass_ImportStaged(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	stub := newImportStub(t, importStubCfg{appID: 7, slug: "acme-bot", clientID: "Iv1.x"})
	setOrgGitHubBase(t, s, stub.URL)
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/import",
		importBody("7", testRSAPEM(t), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || app.Active {
		t.Fatalf("app = %+v, want a staged (active=false) row", app)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "an App import staged behind a live PAT")
}

// TestCredentialClass_Cutover flips the staged App live and deletes the PAT in
// one transaction, and the class stays BYO-App.
func TestCredentialClass_Cutover(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{
		appInstalls: []stubInstall{{ID: 1, Login: "acme"}},
	})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, false) // staged
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "staging the App")

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cutover = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil || !app.Active {
		t.Fatalf("app not active after cutover: %+v", app)
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("PAT survived cutover: %q — the App is now live", v)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "a cutover")
}

// TestCredentialClass_SwitchToPAT validates and stores the PAT, tears the App
// down (row, installations, secrets), and moves the org back to the PAT class.
func TestCredentialClass_SwitchToPAT(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, true) // live App
	seedInstallation(t, s, 1, "acme")
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "registering a live App")

	rec := doJSON(t, s, http.MethodPost,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/switch-to", map[string]string{"pat": "ghp_valid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("switch-to-pat = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	if app, _ := s.githubApps.GetForOrgSystem(ctx, runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app survived switch-to-pat: %+v", app)
	}
	if insts, _ := s.githubApps.ListInstallationsForOrgSystem(ctx, runmode.LocalDefaultOrgID); len(insts) != 0 {
		t.Errorf("installations survived switch-to-pat: %+v", insts)
	}
	if v := getSecret(t, s, "github_app_1_pem"); v != "" {
		t.Errorf("app pem secret survived teardown: %q", v)
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_valid" {
		t.Errorf("PAT = %q after switch, want ghp_valid stored", v)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "a switch to PAT")
}

// TestCredentialClass_DiscardStaged tears down a staged registration (row,
// installations, secrets), leaves the live PAT untouched, and returns the org
// to the PAT class.
func TestCredentialClass_DiscardStaged(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false) // staged
	seedInstallation(t, s, 1, "acme")
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "staging the App")

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discard = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	if app, _ := s.githubApps.GetForOrgSystem(ctx, runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app still registered after discard: %+v", app)
	}
	if insts, _ := s.githubApps.ListInstallationsForOrgSystem(ctx, runmode.LocalDefaultOrgID); len(insts) != 0 {
		t.Errorf("installations survived discard: %+v", insts)
	}
	if v := getSecret(t, s, "github_app_1_pem"); v != "" {
		t.Errorf("app pem secret survived discard: %q", v)
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q after discard, want it untouched (ghp_live)", v)
	}
	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "discarding a staged App")
}

// TestCredentialClass_BulkSettingsSaveIsNoBackDoor: a settings save, with or
// without a github_credential_class field (strict decoding rejects one), leaves
// the class where the credential transitions put it, while its own fields apply.
func TestCredentialClass_BulkSettingsSaveIsNoBackDoor(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true) // a BYO-App org
	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "registering the App")

	for _, tc := range []struct {
		name       string
		body       map[string]any
		wantStatus int // 0 = 200
	}{
		{
			name: "ordinary_save",
			body: map[string]any{
				"github_base_url":      "https://github.com",
				"github_poll_interval": "7m0s",
				"jira_poll_interval":   "5m0s",
			},
		},
		{
			name: "save_attempting_to_carry_the_class",
			body: map[string]any{
				"github_base_url":         "https://github.com",
				"github_poll_interval":    "5m0s",
				"jira_poll_interval":      "5m0s",
				"github_credential_class": "pat",
			},
			wantStatus: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchOrgSettings(t, s, tc.body)
			want := tc.wantStatus
			if want == 0 {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("settings save = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
			}
			assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "a bulk org-settings save")
		})
	}

	set, err := s.orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if set.GitHubBaseURL != "https://github.com" {
		t.Errorf("github_base_url = %q; want the saved value — the settings writer must still own its own columns", set.GitHubBaseURL)
	}
}
