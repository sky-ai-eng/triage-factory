package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- test fixtures -------------------------------------------------------

// ghAccessStub models the GitHub REST endpoints the either/or switch flows
// touch: /user (PAT whoami), /user/repos (PAT reach), /app/installations (App
// installation list for cutover backfill), the per-installation token mint, and
// /installation/repositories (App reach for the cutover preview). All paths are
// optional — a test sets only the fields it exercises.
type ghAccessStub struct {
	login        string              // /api/v3/user → 200 {login}; "" → 401
	userRepos    []string            // /api/v3/user/repos
	appInstalls  []stubInstall       // /api/v3/app/installations
	installRepos map[string][]string // /api/v3/installation/repositories, keyed by installation id
}

type stubInstall struct {
	ID    int64
	Login string
}

func newGitHubAccessStub(t *testing.T, cfg ghAccessStub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		if cfg.login == "" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": cfg.login})
	})

	mux.HandleFunc("/api/v3/user/repos", func(w http.ResponseWriter, r *http.Request) {
		// ListUserRepos paginates until an empty page; serve everything on page 1.
		repos := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			for _, n := range cfg.userRepos {
				repos = append(repos, map[string]any{"full_name": n})
			}
		}
		_ = json.NewEncoder(w).Encode(repos)
	})

	mux.HandleFunc("/api/v3/app/installations", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, len(cfg.appInstalls))
		for _, in := range cfg.appInstalls {
			out = append(out, map[string]any{
				"id":         in.ID,
				"account":    map[string]any{"login": in.Login, "type": "Organization"},
				"created_at": "2026-01-01T00:00:00Z",
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		// POST /api/v3/app/installations/{id}/access_tokens → mint ghs_{id}.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		var id string
		for i, p := range parts {
			if p == "installations" && i+1 < len(parts) {
				id = parts[i+1]
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + id,
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})

	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ghs_")
		names := cfg.installRepos[id]
		repos := make([]map[string]any, 0, len(names))
		for _, n := range names {
			repos = append(repos, map[string]any{"full_name": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(repos), "repositories": repos})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seedLocalApp registers an org App with the given active flag and stores its
// three Vault/keychain secrets (a real RSA PEM so minting paths work). Refs
// follow the github_app_{id}_* convention the register callback uses.
func seedLocalApp(t *testing.T, s *Server, active bool) {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	for ref, val := range map[string]string{
		"github_app_1_client_secret":  "client-secret",
		"github_app_1_pem":            testRSAPEM(t),
		"github_app_1_webhook_secret": "webhook-secret",
	} {
		if err := s.secrets.Put(ctx, org, ref, val, ""); err != nil {
			t.Fatalf("put secret %s: %v", ref, err)
		}
	}
	if err := s.githubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: org, AppID: "1", Slug: "tf-bot", ClientID: "Iv1.x",
		ClientSecretRef:  "github_app_1_client_secret",
		PEMRef:           "github_app_1_pem",
		WebhookSecretRef: "github_app_1_webhook_secret",
		Active:           active,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
}

func seedInstallation(t *testing.T, s *Server, id int64, login string) {
	t.Helper()
	if err := s.githubApps.UpsertInstallation(context.Background(), domain.OrgGitHubAppInstallation{
		InstallationID: strconv.FormatInt(id, 10),
		OrgID:          runmode.LocalDefaultOrgID,
		AccountType:    "Organization",
		AccountLogin:   login,
	}); err != nil {
		t.Fatalf("seed installation %s: %v", login, err)
	}
}

func setOrgGitHubBase(t *testing.T, s *Server, base string) {
	t.Helper()
	if err := s.orgs.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: base}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
}

func getSecret(t *testing.T, s *Server, key string) string {
	t.Helper()
	v, err := s.secrets.Get(context.Background(), runmode.LocalDefaultOrgID, key)
	if err != nil {
		t.Fatalf("get secret %s: %v", key, err)
	}
	return v
}

// --- staging at registration ---------------------------------------------

// TestGitHubAppRegisterCallback_Staging pins the either/or staging rule
// (TFAC-328): an App registered while an org PAT is live is written
// active=false (staged — the PAT stays live until a cutover); a fresh setup
// with no PAT registers active=true.
func TestGitHubAppRegisterCallback_Staging(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	// Stub GitHub's manifest-conversion endpoint.
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

	cases := []struct {
		name       string
		pat        string
		wantActive bool
	}{
		{"pat_present_stages_inactive", "ghp_live", false},
		{"no_pat_registers_active", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit() // fresh mock keychain so a prior subtest's PAT doesn't leak
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
				"/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/register/callback?code=c&state="+signed, nil)
			if rec.Code != http.StatusFound {
				t.Fatalf("callback = %d, want 302; body=%s", rec.Code, rec.Body.String())
			}
			app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
			if app == nil {
				t.Fatal("no app row written by callback")
			}
			if app.Active != tc.wantActive {
				t.Errorf("Active = %v, want %v (pat=%q)", app.Active, tc.wantActive, tc.pat)
			}
		})
	}
}

// --- discard -------------------------------------------------------------

// TestGitHubAppDiscard_Staged tears down a staged registration (row +
// installations + secrets) and leaves the live PAT untouched.
func TestGitHubAppDiscard_Staged(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false) // staged
	seedInstallation(t, s, 1, "acme")
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE github-app = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app still registered after discard: %+v", app)
	}
	if insts, _ := s.githubApps.ListInstallationsForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); len(insts) != 0 {
		t.Errorf("installations survived discard: %+v", insts)
	}
	if v := getSecret(t, s, "github_app_1_pem"); v != "" {
		t.Errorf("app pem secret survived discard: %q", v)
	}
	// The live PAT must be untouched by a staged-switch discard.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q after discard, want it untouched (ghp_live)", v)
	}
}

// TestGitHubAppDiscard_Active rejects discarding a live App — removing it only
// happens through switch-to-pat.
func TestGitHubAppDiscard_Active(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true) // active

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE active github-app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("active app was deleted on a 409 discard")
	}
}

// --- cutover -------------------------------------------------------------

func TestGitHubAppCutover_NoApp404(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/cutover", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cutover with no app = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubAppCutover_AlreadyActive409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/cutover", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cutover of active app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHubAppCutover_NoInstallations409 backfills (the stub reports zero
// installations) and rejects the cutover — switching to an App installed
// nowhere would dark the org.
func TestGitHubAppCutover_NoInstallations409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{}) // no app installations
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, false)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/cutover", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cutover with no installations = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil || app.Active {
		t.Errorf("app should still be staged after a 409 cutover, got %+v", app)
	}
}

// TestGitHubAppCutover_Success flips the staged App live and deletes the PAT in
// one transaction; after it XOR holds.
func TestGitHubAppCutover_Success(t *testing.T) {
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

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/cutover", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cutover = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || !app.Active {
		t.Fatalf("app not active after cutover: %+v", app)
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("PAT survived cutover: %q (XOR violated — App is now live)", v)
	}
}

// --- switch to PAT -------------------------------------------------------

// TestGitHubAccessSwitchToPAT_Success validates the PAT, stores it, and tears
// the App down (row + installations + secrets).
func TestGitHubAccessSwitchToPAT_Success(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, true)
	seedInstallation(t, s, 1, "acme")

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-access/switch-to-pat", map[string]string{"pat": "ghp_valid"})
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
}

// TestGitHubAccessSwitchToPAT_InvalidPAT rejects an unvalidatable token and
// changes nothing.
func TestGitHubAccessSwitchToPAT_InvalidPAT(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: ""}) // /user → 401
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-access/switch-to-pat", map[string]string{"pat": "ghp_bad"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("switch-to-pat with bad token = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("app was torn down despite a failed PAT validation")
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("PAT stored on a 422 switch: %q", v)
	}
}

func TestGitHubAccessSwitchToPAT_NoApp404(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-access/switch-to-pat", map[string]string{"pat": "ghp_valid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("switch-to-pat with no app = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- preflights ----------------------------------------------------------

// TestGitHubAccessPATPreflight_StoresNothing returns the reachability diff +
// login and does NOT persist the probed PAT.
func TestGitHubAccessPATPreflight_StoresNothing(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{
		login:     "octocat",
		userRepos: []string{"acme/web"}, // reaches web but not api
	})
	setOrgGitHubBase(t, s, stub.URL)
	seedConfiguredRepo(t, s, "acme", "web")
	seedConfiguredRepo(t, s, "acme", "api")

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-access/pat-preflight", map[string]string{"pat": "ghp_x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pat-preflight = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tracked   int    `json:"tracked"`
		Reachable int    `json:"reachable"`
		Login     string `json:"login"`
		DarkRepos []struct {
			Repo  string   `json:"repo"`
			Teams []string `json:"teams"`
		} `json:"dark_repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tracked != 2 || out.Reachable != 1 {
		t.Errorf("tracked=%d reachable=%d, want 2/1", out.Tracked, out.Reachable)
	}
	if len(out.DarkRepos) != 1 || out.DarkRepos[0].Repo != "acme/api" {
		t.Errorf("dark_repos=%+v, want [acme/api]", out.DarkRepos)
	}
	if out.Login != "octocat" {
		t.Errorf("login=%q, want octocat", out.Login)
	}
	// The probed PAT must not be persisted by a preflight.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("pat-preflight stored the token: %q", v)
	}
}

// TestGitHubAppCutoverPreflight_Diff previews a staged App's reach by minting
// directly (the resolver would skip the inactive App).
func TestGitHubAppCutoverPreflight_Diff(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{
		// The preflight reconciles the mirror first, so /app/installations must
		// report installation 1 or the backfill would prune the seeded row.
		appInstalls:  []stubInstall{{ID: 1, Login: "acme"}},
		installRepos: map[string][]string{"1": {"acme/web"}}, // installation 1 grants web only
	})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, false) // staged
	seedInstallation(t, s, 1, "acme")
	seedConfiguredRepo(t, s, "acme", "web")
	seedConfiguredRepo(t, s, "acme", "api")

	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github-app/cutover-preflight", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cutover-preflight = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// This GET reconciles the installation mirror, so its 200 body must not be
	// cached (a cached preview would skip the reconcile).
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (GET has a write side-effect)", cc)
	}
	var out struct {
		Tracked   int `json:"tracked"`
		Reachable int `json:"reachable"`
		DarkRepos []struct {
			Repo string `json:"repo"`
		} `json:"dark_repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tracked != 2 || out.Reachable != 1 {
		t.Errorf("tracked=%d reachable=%d, want 2/1", out.Tracked, out.Reachable)
	}
	if len(out.DarkRepos) != 1 || out.DarkRepos[0].Repo != "acme/api" {
		t.Errorf("dark_repos=%+v, want [acme/api]", out.DarkRepos)
	}
}

// --- App XOR PAT guard ---------------------------------------------------

// TestGitHubPATPut_WithApp409 rejects binding a PAT while an App is registered;
// the switch flow is the only path between the two. The guard lives on the
// credential resource now — the bulk settings save can no longer carry a token
// at all, so it has nothing to guard.
func TestGitHubPATPut_WithApp409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{
		"base_url": "https://github.com", "pat": "ghp_new",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("pat bind with app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestOrgSettingsSave_CarriesNoCredential proves the bulk settings save can't
// reach the vault: a body carrying a token is accepted as an ordinary config
// save (the field is not part of the schema, so it's ignored) and stores
// nothing. This is the structural version of the old XOR guard on this route —
// there is no longer a code path from here to a credential write to guard.
func TestOrgSettingsSave_CarriesNoCredential(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPost, "/api/settings/org", map[string]any{"github_pat": "ghp_new"})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings save = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "" {
		t.Errorf("settings save stored a PAT (%q) — it must not be a vault write path", stored)
	}
}

// --- the bound identity Settings shows next to the token ------------------

// TestOrgSettingsGet_ReportsBoundPATLogin pins the @login the Settings GitHub
// section renders beside its "Replace token" control. It tracks the LIVE
// credential: it appears on a bind, re-points on a rotation, and disappears
// with the token — a name left behind by a since-replaced credential would tell
// the operator they're about to rotate an account they aren't.
func TestOrgSettingsGet_ReportsBoundPATLogin(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	if got := orgPATLogin(t, s); got != "" {
		t.Errorf("github_pat_login = %q with nothing bound, want empty", got)
	}

	gh := githubUserStub(t, "acme-bot")
	if rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{
		"base_url": gh.URL, "pat": "ghp_first",
	}); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "acme-bot" {
		t.Errorf("github_pat_login = %q after bind, want acme-bot", got)
	}

	// Rotation: same route, a token that authenticates as someone else.
	rotated := githubUserStub(t, "acme-bot-2")
	if rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{
		"base_url": rotated.URL, "pat": "ghp_second",
	}); rec.Code != http.StatusOK {
		t.Fatalf("pat rotate = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "acme-bot-2" {
		t.Errorf("github_pat_login = %q after rotation, want acme-bot-2", got)
	}

	if rec := doJSON(t, s, http.MethodDelete, patRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("pat unbind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "" {
		t.Errorf("github_pat_login = %q after unbind, want empty", got)
	}
}

// orgPATLogin reads github_pat_login off the org settings GET.
func orgPATLogin(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/settings/org", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Login string `json:"github_pat_login"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.Login
}

// patRoute is the org's GitHub-PAT credential resource in local mode.
func patRoute() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/github-access/pat"
}
