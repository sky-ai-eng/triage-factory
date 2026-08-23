package server

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestCredentialClass_FreshOrgIsPAT pins the column default. An org that has
// bound nothing is in the PAT system with nothing in it — never an empty class,
// which no call site would know how to answer for.
func TestCredentialClass_FreshOrgIsPAT(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "provisioning a fresh org")
}

// TestCredentialClass_PATBind writes pat when the org binds a token.
func TestCredentialClass_PATBind(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})

	rec := bindOrgGitHubPAT(t, s, stub.URL, "ghp_valid")
	if rec.Code != http.StatusOK {
		t.Fatalf("bind pat = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "a PAT bind")
}

// TestCredentialClass_PATBindRefusedForUnknownClass pins the fail-closed arm on
// the one site where the missing-row inference would resolve to a WRITE.
//
// The bind's XOR gate asks "is there an org_github_apps row?" and reads no-row
// as "nothing to conflict with". For an org in a credential system this build
// can't name — one whose credential legitimately leaves no row — that inference
// would hand it a PAT and record it as a PAT org, moving it between credential
// systems on the strength of a question that was never the right one.
//
// Unreachable today: nothing writes a class outside the known set, which is
// exactly what keeps an older build from meeting one. This is the backstop for
// when that stops being true.
func TestCredentialClass_PATBindRefusedForUnknownClass(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	ctx := context.Background()

	// A class from a newer peer. No App row — the shape that makes the XOR gate
	// wave this through.
	if _, err := s.orgs.SetGitHubCredentialClass(ctx, runmode.LocalDefaultOrgID, domain.GitHubCredentialClass("managed_app")); err != nil {
		t.Fatalf("seed unknown class: %v", err)
	}

	rec := bindOrgGitHubPAT(t, s, stub.URL, "ghp_valid")
	if rec.Code != http.StatusConflict {
		t.Fatalf("bind pat under an unknown class = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Nothing was written — not the token, and not the class. A refusal that
	// still moved the org would be the bug wearing a 409.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("a refused bind stored a PAT: %q", v)
	}
	if got := orgCredentialClass(t, s); got != domain.GitHubCredentialClass("managed_app") {
		t.Errorf("a refused bind rewrote the class to %q; want it untouched", got)
	}
}

// TestCredentialClass_RegisterStaged writes byo_app at registration, including
// while the App is STAGED behind a live PAT.
//
// The staged case is the one worth pinning: the class says the org is in the
// App system from the moment the key lands, while org_github_apps.active still
// says the PAT is the live credential. Two orthogonal facts, and collapsing
// them would make the class a duplicate of the Active bit.
func TestCredentialClass_RegisterStaged(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	convStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/app-manifests/") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":4242,"slug":"tf","client_id":"Iv1.x","client_secret":"cs",` +
			`"pem":"-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----","webhook_secret":"wh"}`))
	}))
	t.Cleanup(convStub.Close)

	for _, tc := range []struct {
		name       string
		pat        string
		wantActive bool
	}{
		{"staged_behind_live_pat", "ghp_live", false},
		{"fresh_setup_goes_live", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			s := newTestServer(t)
			var key [32]byte
			if _, err := rand.Read(key[:]); err != nil {
				t.Fatalf("seed key: %v", err)
			}
			s.SetDeployConfig("http://localhost:3000", key)
			setOrgGitHubBase(t, s, convStub.URL)
			if tc.pat != "" {
				if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, tc.pat, ""); err != nil {
					t.Fatalf("seed pat: %v", err)
				}
			}

			state := appRegisterState{
				OrgID: runmode.LocalDefaultOrgID, OwnerType: "user",
				ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			}
			signed, err := state.sign(key)
			if err != nil {
				t.Fatalf("sign state: %v", err)
			}
			rec := doJSON(t, s, http.MethodGet,
				"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/register/callback?code=c&state="+signed, nil)
			if rec.Code != http.StatusFound {
				t.Fatalf("callback = %d, want 302; body=%s", rec.Code, rec.Body.String())
			}

			app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
			if app == nil {
				t.Fatal("no app row written by callback")
			}
			// The class is byo_app either way — Active is what differs.
			if app.Active != tc.wantActive {
				t.Errorf("Active = %v, want %v", app.Active, tc.wantActive)
			}
			assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "an App registration")
		})
	}
}

// TestCredentialClass_ImportStaged is the register test's twin for the
// paste-an-existing-App path: same class, same staging independence.
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

// TestCredentialClass_Cutover holds byo_app across the cutover. The class does
// not change here — the org was already in the App system — so this pins that
// the transition re-asserts rather than drifts.
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

	assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "a cutover")
}

// TestCredentialClass_SwitchToPAT moves the org back to pat when the App is
// torn down for a token.
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

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "a switch to PAT")
}

// TestCredentialClass_DiscardStaged returns the org to pat when an abandoned
// PAT→App switch is discarded — no App row remains, and the PAT that was live
// throughout is the credential again in name as well as in fact.
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

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "discarding a staged App")
}

// TestCredentialClass_PATDisconnectKeepsClass pins that unbinding a token does
// NOT move the org between credential systems. The class names which system the
// org is in, not whether that system currently holds a credential — an org that
// disconnects its PAT is a PAT org with nothing bound, and one that disconnects
// while a registration survives is still a BYO-App org.
func TestCredentialClass_PATDisconnectKeepsClass(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})

	if rec := bindOrgGitHubPAT(t, s, stub.URL, "ghp_valid"); rec.Code != http.StatusOK {
		t.Fatalf("bind pat = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, s, http.MethodDelete, patRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "disconnecting the PAT")
}

// TestCredentialClass_BulkSettingsSaveIsNoBackDoor is the negative space: a
// bulk settings save must leave the class exactly where the credential
// transitions put it, whether or not the request tries to carry one.
//
// This is the product-level half of the guarantee, and it is deliberately not
// the only half. The handler is read-modify-write, so the class survives here
// even if someone folded the column into UpdateSettings' upsert lists — that
// store-level mistake is caught instead by the dbtest conformance case
// (OrgSettings_GitHubCredentialClass_OwnedByTransitionsNotSettingsSave, which
// writes a struct whose class differs from the stored one). What this test
// catches is the other direction: a handler that stops round-tripping, grows a
// github_credential_class field, or otherwise lets an org change credential
// systems by saving settings.
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
			// The handler has no such field, so strict decoding rejects the
			// request — pinned because "the API quietly grew one" is exactly
			// how a projection turns into a second, contradictable choice.
			// Either way the class must not move.
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

	// The save's own fields still applied — this is a scoping test, not a
	// "settings saves do nothing" test.
	set, err := s.orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	if set.GitHubBaseURL != "https://github.com" {
		t.Errorf("github_base_url = %q; want the saved value — the settings writer must still own its own columns", set.GitHubBaseURL)
	}
}
