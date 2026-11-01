package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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
	appDeleted   bool                // every App-JWT read answers 404, as GitHub does for a deleted App
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
	mux.HandleFunc("/api/v3/user/emails", func(w http.ResponseWriter, r *http.Request) {
		writeGitHubPrimaryEmail(w, cfg.login+"@example.com")
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

	mux.HandleFunc("/api/v3/app", func(w http.ResponseWriter, r *http.Request) {
		if cfg.appDeleted {
			http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "slug": "tf-test"})
	})

	mux.HandleFunc("/api/v3/app/installations", func(w http.ResponseWriter, r *http.Request) {
		if cfg.appDeleted {
			http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
			return
		}
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
	if _, err := s.githubApps.CreateForOrg(ctx, domain.OrgGitHubApp{
		OrgID: org, AppID: "1", Slug: "tf-bot", ClientID: "Iv1.x",
		ClientSecretRef:  "github_app_1_client_secret",
		PEMRef:           "github_app_1_pem",
		WebhookSecretRef: "github_app_1_webhook_secret",
		Active:           active,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	// Staged (active=false) or live, a registered App means the org is in the
	// BYO-App credential system — registration writes both in one transaction.
	seedBYOAppCredentialClass(t, s, org)
}

// seedBYOAppCredentialClass writes the BYO-App class that registering an App
// writes beside its row. A fixture inserting the row directly needs it, or the
// handlers, which dispatch on the class, serve the org as a PAT org.
func seedBYOAppCredentialClass(t *testing.T, s *Server, orgID string) {
	t.Helper()
	if _, err := s.orgs.SetGitHubCredentialClass(context.Background(), orgID, domain.GitHubCredentialClassBYOApp); err != nil {
		t.Fatalf("set github credential class: %v", err)
	}
}

func seedInstallation(t *testing.T, s *Server, id int64, login string) {
	t.Helper()
	if _, err := s.githubApps.UpsertInstallation(context.Background(), domain.OrgGitHubAppInstallation{
		InstallationID: strconv.FormatInt(id, 10),
		OrgID:          runmode.LocalDefaultOrgID,
		AccountType:    "Organization",
		AccountLogin:   login,
		GitHubHost:     seedRepoHost(t, s),
	}); err != nil {
		t.Fatalf("seed installation %s: %v", login, err)
	}
}

func setOrgGitHubBase(t *testing.T, s *Server, base string) {
	t.Helper()
	dbtest.SeedOrgSettings(t, s.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: base})
}

// ghAppsRaceHook wraps s.githubApps and fires a one-shot hook after the first
// GetForOrgSystem / ListInstallationsOnHostSystem returns, so the handler holds
// the pre-mutation row while a concurrent transition commits. It reaches only
// the reads before the transaction; the re-read under the lock sees the change.
type ghAppsRaceHook struct {
	db.GitHubAppsStore
	afterGet  func() // fires once, after the first GetForOrgSystem
	afterList func() // fires once, after the first ListInstallationsOnHostSystem
	getOnce   sync.Once
	listOnce  sync.Once
}

func (g *ghAppsRaceHook) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	app, err := g.GitHubAppsStore.GetForOrgSystem(ctx, orgID)
	if err == nil && g.afterGet != nil {
		g.getOnce.Do(g.afterGet)
	}
	return app, err
}

func (g *ghAppsRaceHook) ListInstallationsOnHostSystem(ctx context.Context, orgID, host string) ([]domain.OrgGitHubAppInstallation, error) {
	insts, err := g.GitHubAppsStore.ListInstallationsOnHostSystem(ctx, orgID, host)
	if err == nil && g.afterList != nil {
		g.listOnce.Do(g.afterList)
	}
	return insts, err
}

// setActiveNilStore is a GitHubAppsStore whose SetActive always answers
// (nil, nil) — the shape SetActive takes when the row it was about to flip
// isn't there any more. Every other method delegates to the real store
// through the embedded interface.
type setActiveNilStore struct{ db.GitHubAppsStore }

func (setActiveNilStore) SetActive(context.Context, string, bool) (*domain.OrgGitHubApp, error) {
	return nil, nil
}

// setActiveReturnsNilTx swaps a setActiveNilStore into every transaction it
// opens, which is how a test reaches tx.GitHubApps.SetActive inside the lock
// where no real interleaving can.
type setActiveReturnsNilTx struct{ inner db.TxRunner }

func (w setActiveReturnsNilTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.inner.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.GitHubApps = setActiveNilStore{GitHubAppsStore: tx.GitHubApps}
		return fn(tx)
	})
}

func (w setActiveReturnsNilTx) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.inner.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.GitHubApps = setActiveNilStore{GitHubAppsStore: tx.GitHubApps}
		return fn(tx)
	})
}

func (w setActiveReturnsNilTx) WithReadTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.inner.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.GitHubApps = setActiveNilStore{GitHubAppsStore: tx.GitHubApps}
		return fn(tx)
	})
}

func (w setActiveReturnsNilTx) SyntheticClaimsWithReadTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return w.inner.SyntheticClaimsWithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		tx.GitHubApps = setActiveNilStore{GitHubAppsStore: tx.GitHubApps}
		return fn(tx)
	})
}

func (w setActiveReturnsNilTx) SystemWithTx(ctx context.Context, orgID string, fn func(db.SystemTxStores) error) error {
	return w.inner.SystemWithTx(ctx, orgID, fn)
}

// hookGitHubApps installs the race hook on the server and returns the store it
// wrapped, so a hook body can mutate the row without recursing back through
// itself.
func hookGitHubApps(s *Server, hook *ghAppsRaceHook) db.GitHubAppsStore {
	orig := s.githubApps
	hook.GitHubAppsStore = orig
	s.githubApps = hook
	return orig
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

// TestGitHubAppRegisterCallback_Staging: an App registered beside a live org
// PAT is staged (active=false) and a fresh setup goes live; both write the
// BYO-App class, which is independent of the Active bit.
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
				"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/register/callback?code=c&state="+signed, nil)
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
			assertCredentialClass(t, s, domain.GitHubCredentialClassBYOApp, "an App registration")
		})
	}
}

// --- discard -------------------------------------------------------------

// TestGitHubAppDiscard_Active rejects discarding a live App — removing it only
// happens through switch-to-pat.
func TestGitHubAppDiscard_Active(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true) // active

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE active github/app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("active app was deleted on a 409 discard")
	}
}

// TestGitHubAppDiscard_AppGoesLiveUnderTheGuard: a cutover (active flip and PAT
// delete) commits between the discard's read and its teardown, so the guard,
// re-read under the lock, must refuse rather than destroy the org's only credential.
func TestGitHubAppDiscard_AppGoesLiveUnderTheGuard(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false) // staged, behind a live PAT
	seedInstallation(t, s, 1, "acme")
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}

	var apps db.GitHubAppsStore
	hook := &ghAppsRaceHook{afterGet: func() {
		ctx := context.Background()
		if _, err := apps.SetActive(ctx, runmode.LocalDefaultOrgID, true); err != nil {
			t.Errorf("concurrent cutover: set active: %v", err)
		}
		if _, err := s.secrets.Delete(ctx, runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT); err != nil {
			t.Errorf("concurrent cutover: delete pat: %v", err)
		}
	}}
	apps = hookGitHubApps(s, hook)

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("discard of an App that went live under the guard = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// The fixture's cutover really did leave the App as the only credential —
	// without this the assertions below would pass on a weaker interleaving than
	// the one under test.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Fatalf("PAT = %q, want it deleted by the fixture's cutover", v)
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("the now-live App was torn down by a discard that read it as staged — the org has no GitHub credential at all")
	}
	if !app.Active {
		t.Errorf("app.Active = false, want true — the fixture's cutover should have stuck")
	}
	if v := getSecret(t, s, "github_app_1_pem"); v == "" {
		t.Error("the live App's private key was destroyed by the discard")
	}
}

// --- disconnect the live App ---------------------------------------------

// TestGitHubAppDisconnect_Success: registration, installations, secrets and
// host are gone, the class is back at the default, the removal is recorded,
// and tokens are invalidated and the poller kicked.
func TestGitHubAppDisconnect_Success(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	setOrgGitHubBase(t, s, "https://ghe.example.com")
	seedLocalApp(t, s, true)
	seedInstallation(t, s, 1, "acme")
	seedInstallation(t, s, 2, "acme-labs")

	var invalidated []string
	s.onInstallationTokensInvalid = func(orgID, installationID string) {
		invalidated = append(invalidated, installationID)
	}
	kicked := make(chan string, 1)
	s.onGitHubChanged = func(orgID string) { kicked <- orgID }

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/disconnect", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST github/app/disconnect = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status      string `json:"status"`
		Deleted     bool   `json:"github_app_deleted_locally"`
		SettingsURL string `json:"github_app_settings_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "disconnected" || !body.Deleted {
		t.Errorf("body = %+v, want status=disconnected deleted=true", body)
	}
	// The link out names the GitHub the App lived on, read before the host
	// was cleared.
	if body.SettingsURL != "https://ghe.example.com/settings/apps" {
		t.Errorf("github_app_settings_url = %q, want the App's own host", body.SettingsURL)
	}

	ctx := context.Background()
	if app, _ := s.githubApps.GetForOrgSystem(ctx, runmode.LocalDefaultOrgID); app != nil {
		t.Errorf("app survived disconnect: %+v", app)
	}
	if insts, _ := s.githubApps.ListInstallationsForOrgSystem(ctx, runmode.LocalDefaultOrgID); len(insts) != 0 {
		t.Errorf("installations survived disconnect: %+v", insts)
	}
	if v := getSecret(t, s, "github_app_1_pem"); v != "" {
		t.Errorf("app pem secret survived teardown: %q", v)
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("a PAT appeared from nowhere: %q", v)
	}
	set, err := s.orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if set.GitHubCredentialClass != domain.GitHubCredentialClassPAT {
		t.Errorf("class = %q, want %q (the rowless default)", set.GitHubCredentialClass, domain.GitHubCredentialClassPAT)
	}
	if set.GitHubBaseURL != "" {
		t.Errorf("host = %q after disconnect, want cleared — nothing remains that uses it", set.GitHubBaseURL)
	}

	if got := strings.Join(invalidated, ","); got != "1,2" {
		t.Errorf("token invalidations = %q, want both installations", got)
	}
	select {
	case org := <-kicked:
		if org != runmode.LocalDefaultOrgID {
			t.Errorf("change kick for org %q, want %q", org, runmode.LocalDefaultOrgID)
		}
	case <-time.After(2 * time.Second):
		t.Error("the GitHub-changed kick never fired — polling stays due under a credential that is gone")
	}

	rows, _, err := sqlitestore.New(s.db).AccessChangeLog.ListByOrg(ctx, runmode.LocalDefaultOrgID, domain.AccessChangeListOpts{Limit: 10})
	if err != nil {
		t.Fatalf("list access changes: %v", err)
	}
	var removals []string
	for _, row := range rows {
		if row.Action == domain.AccessActionCredentialRemoved {
			removals = append(removals, row.DetailJSON)
		}
	}
	if len(removals) != 1 {
		t.Fatalf("%d credential_removed rows, want 1: %v", len(removals), removals)
	}
	for _, want := range []string{`"kind":"github_app"`, `"name":"tf-bot"`, `ghe.example.com`} {
		if !strings.Contains(removals[0], want) {
			t.Errorf("access change %s lacks %s — the removal is named by host and slug", removals[0], want)
		}
	}
}

// TestGitHubAppTransitions_NoApp404: the App row is the resource every
// transition acts on, so an org with none is a 404.
func TestGitHubAppTransitions_NoApp404(t *testing.T) {
	for _, tc := range []struct {
		name, route string
		body        any
	}{
		{"disconnect", "/github/app/disconnect", nil},
		{"cutover", "/github/app/cutover", nil},
		{"switch to PAT", "/github/pat/switch-to", map[string]string{"pat": "ghp_valid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			setOrgGitHubBase(t, s, newGitHubAccessStub(t, ghAccessStub{login: "octocat"}).URL)

			rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+tc.route, tc.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s with no app = %d, want 404; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestGitHubAppDisconnect_Staged409: a staged App is the discard's to remove,
// so disconnect refuses and leaves the registration, its key and the PAT.
func TestGitHubAppDisconnect_Staged409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, false) // staged, behind a live PAT
	seedInstallation(t, s, 1, "acme")
	if err := s.secrets.Put(context.Background(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
		t.Fatalf("seed pat: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/disconnect", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disconnect of a staged app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("staged app was torn down on a 409 disconnect")
	}
	if v := getSecret(t, s, "github_app_1_pem"); v == "" {
		t.Error("the staged App's private key was destroyed on a 409 disconnect")
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q after a 409 disconnect, want it untouched (ghp_live)", v)
	}
}

// TestGitHubAppDisconnect_AppReplacedUnderTheGuard: a switch-to-pat and a new
// staged registration land between the advisory read and the lock, so the
// disconnect must refuse rather than tear down the staged replacement.
func TestGitHubAppDisconnect_AppReplacedUnderTheGuard(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)
	seedInstallation(t, s, 1, "acme")

	var apps db.GitHubAppsStore
	hook := &ghAppsRaceHook{afterGet: func() {
		ctx := context.Background()
		if err := apps.DeleteForOrg(ctx, runmode.LocalDefaultOrgID); err != nil {
			t.Errorf("concurrent switch-to-pat: delete app: %v", err)
		}
		if err := s.secrets.Put(ctx, runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT, "ghp_live", ""); err != nil {
			t.Errorf("concurrent switch-to-pat: save pat: %v", err)
		}
		seedLocalApp(t, s, false) // a fresh registration, staged behind the token
	}}
	apps = hookGitHubApps(s, hook)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/disconnect", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("disconnect of an App replaced under the guard = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil {
		t.Fatal("the staged replacement was torn down by a disconnect that read the previous App as live")
	}
	if app.Active {
		t.Errorf("app.Active = true, want the fixture's staged replacement")
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q, want the fixture's token untouched", v)
	}
}

// --- cutover -------------------------------------------------------------

func TestGitHubAppCutover_AlreadyActive409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
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

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cutover with no installations = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil || app.Active {
		t.Errorf("app should still be staged after a 409 cutover, got %+v", app)
	}
}

// TestGitHubAppCutover_DeletedApp422: a staged App deleted on GitHub is a 422
// naming it, with the reason recorded on the row, not a retryable-looking 502.
func TestGitHubAppCutover_DeletedApp422(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{appDeleted: true})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, false)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cutover of a deleted app = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no longer exists on GitHub") {
		t.Errorf("body=%s; want the deleted App named", rec.Body.String())
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || app.Active {
		t.Fatalf("app should still be staged after a refused cutover, got %+v", app)
	}
	if app.UnusableReason != domain.GitHubAppMissing {
		t.Errorf("unusable_reason = %q, want %q recorded by the backfill", app.UnusableReason, domain.GitHubAppMissing)
	}
}

// TestGitHubAppCutover_RegistrationVanishesUnderTheGuard: a discard commits
// while the cutover is reconciling with GitHub, so the re-read under the lock
// must refuse, and the PAT the org still runs on must survive.
func TestGitHubAppCutover_RegistrationVanishesUnderTheGuard(t *testing.T) {
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

	// Hooked on the installation read rather than the App read, so the discard
	// lands after the backfill: mutating earlier would leave nothing for the
	// backfill to reconcile and the handler would refuse for the ordinary
	// no-installations reason instead of the one under test.
	var apps db.GitHubAppsStore
	hook := &ghAppsRaceHook{afterList: func() {
		if err := apps.DeleteForOrg(context.Background(), runmode.LocalDefaultOrgID); err != nil {
			t.Errorf("concurrent discard: delete app: %v", err)
		}
	}}
	apps = hookGitHubApps(s, hook)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cutover of a registration discarded under the guard = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// The whole point: the refused cutover must not have taken the credential
	// the org is still running on down with it.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q, want ghp_live — the cutover destroyed the org's only remaining credential", v)
	}
}

// TestGitHubAppCutover_SetActiveReportsNoRow: if SetActive inside the lock
// reports it flipped nothing, the cutover fails before its PAT delete runs,
// which would otherwise strand the org with no credential.
func TestGitHubAppCutover_SetActiveReportsNoRow(t *testing.T) {
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

	s.tx = setActiveReturnsNilTx{inner: s.tx}

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("cutover with SetActive reporting no row = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	// The whole point: a SetActive that reports nothing flipped must not let
	// the unconditional PAT delete right after it run anyway — that exact
	// sequence is what would strand the org with no GitHub credential at all.
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "ghp_live" {
		t.Errorf("PAT = %q, want ghp_live — SetActive reporting no row still let the PAT delete run", v)
	}
	app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID)
	if app == nil || app.Active {
		t.Errorf("app = %+v, want the staged (Active=false) row left untouched", app)
	}
}

// --- switch to PAT -------------------------------------------------------

// TestGitHubAccessSwitchToPAT_InvalidPAT rejects an unvalidatable token and
// changes nothing.
func TestGitHubAccessSwitchToPAT_InvalidPAT(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: ""}) // /user → 401
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, true)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/switch-to", map[string]string{"pat": "ghp_bad"})
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

// TestGitHubAccessSwitchToPAT_RegistrationVanishesUnderTheGuard: a discard lands
// while the PAT is being validated, so the switch must answer 404 and store no
// PAT rather than report removing an App it did not remove.
func TestGitHubAccessSwitchToPAT_RegistrationVanishesUnderTheGuard(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	setOrgGitHubBase(t, s, stub.URL)
	seedLocalApp(t, s, false) // staged — a switch-to-pat is valid from here too
	seedInstallation(t, s, 1, "acme")

	var apps db.GitHubAppsStore
	hook := &ghAppsRaceHook{afterGet: func() {
		if err := apps.DeleteForOrg(context.Background(), runmode.LocalDefaultOrgID); err != nil {
			t.Errorf("concurrent discard: delete app: %v", err)
		}
	}}
	apps = hookGitHubApps(s, hook)

	rec := doJSON(t, s, http.MethodPost,
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/switch-to", map[string]string{"pat": "ghp_valid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("switch-to-pat for a registration discarded under the guard = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if v := getSecret(t, s, integrations.KeyGitHubPAT); v != "" {
		t.Errorf("PAT = %q, want nothing stored — the switch reported 404 and must not have committed", v)
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

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/preflight", map[string]string{"pat": "ghp_x"})
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

	rec := doJSON(t, s, http.MethodGet, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover-preflight", nil)
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

// TestBuildAccessDiff_MatchesTrackedReposByGitHubID: both preflights match a
// tracked repository by GitHub id (a rename stays reachable, a reused name is
// dark), by name only when the row has no id, and name each dark registry row.
func TestBuildAccessDiff_MatchesTrackedReposByGitHubID(t *testing.T) {
	tracked := []domain.TrackedRepoTeams{
		{RepositoryID: "row-renamed", ExternalID: "10", Owner: "acme", Repo: "old", Teams: []string{"core"}},
		{RepositoryID: "row-reused", ExternalID: "20", Owner: "acme", Repo: "api", Teams: []string{"core"}},
		{RepositoryID: "row-no-id", Owner: "Acme", Repo: "Web", Teams: []string{"core"}},
	}
	listing := []ghclient.UserRepo{
		{ID: 10, FullName: "acme/new"},
		{ID: 21, FullName: "acme/api"},
		{ID: 30, FullName: "acme/web"},
	}
	diff := buildAccessDiff(tracked, reachableRepoSetOf(listing))
	if diff.Tracked != 3 || diff.Reachable != 2 {
		t.Errorf("tracked=%d reachable=%d, want 3/2", diff.Tracked, diff.Reachable)
	}
	if len(diff.DarkRepos) != 1 {
		t.Fatalf("dark_repos = %+v, want the reused name's row alone", diff.DarkRepos)
	}
	if got := diff.DarkRepos[0]; got.RepositoryID != "row-reused" || got.GitHubID != "20" || got.Repo != "acme/api" {
		t.Errorf("dark repo = %+v, want row-reused (github id 20, acme/api)", got)
	}
}

// --- App XOR PAT guard ---------------------------------------------------

// TestGitHubPATPut_WithApp409 rejects binding a PAT while an App is registered;
// the switch flow is the only path between the two.
func TestGitHubPATPut_WithApp409(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := bindOrgGitHubPAT(t, s, "https://github.com", "ghp_new")
	if rec.Code != http.StatusConflict {
		t.Fatalf("pat bind with app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestOrgSettingsSave_CarriesNoCredential: the bulk settings save has no token
// field, so strict decoding rejects one and nothing reaches the vault.
func TestOrgSettingsSave_CarriesNoCredential(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedLocalApp(t, s, true)

	rec := patchOrgSettings(t, s, map[string]any{"github_pat": "ghp_new"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("settings save = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := s.secrets.Get(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyGitHubPAT)
	if stored != "" {
		t.Errorf("settings save stored a PAT (%q) — it must not be a vault write path", stored)
	}
}

// --- the bound identity Settings shows next to the token ------------------

// TestOrgSettingsGet_ReportsBoundPATLogin: github_pat_login follows the live
// token, appearing on a bind, re-pointing on a rotation and clearing on unbind,
// which leaves the org in the PAT class.
func TestOrgSettingsGet_ReportsBoundPATLogin(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	if got := orgPATLogin(t, s); got != "" {
		t.Errorf("github_pat_login = %q with nothing bound, want empty", got)
	}

	gh := githubTokenStub(t, map[string]string{"ghp_first": "acme-bot", "ghp_second": "acme-bot-2"})
	if rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_first"); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "acme-bot" {
		t.Errorf("github_pat_login = %q after bind, want acme-bot", got)
	}

	// Rotation: same route, same host, a token that authenticates as someone
	// else.
	if rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{"pat": "ghp_second"}); rec.Code != http.StatusOK {
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
	// The class names the credential system, not whether it holds a credential.
	assertCredentialClass(t, s, domain.GitHubCredentialClassPAT, "disconnecting the PAT")
}

// TestGitHubPATBindAndUnbind_RestartGitHubReadinessOnly: a PAT bind and unbind
// each kick the GitHub poller and clear GitHub poll readiness, and leave Jira's
// poller and readiness alone.
func TestGitHubPATBindAndUnbind_RestartGitHubReadinessOnly(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	gh := githubUserStub(t, "acme-bot")
	commitGitHubHost(t, s, gh.URL)
	kicks := recordChangeKicks(s)

	for _, step := range []struct {
		name   string
		method string
		body   any
	}{
		{"bind", http.MethodPut, map[string]any{"pat": "ghp_x"}},
		{"unbind", http.MethodDelete, nil},
	} {
		markPollsComplete(t, s, "github", "jira")
		if rec := doJSON(t, s, step.method, patRoute(), step.body); rec.Code != http.StatusOK {
			t.Fatalf("pat %s = %d, body=%s", step.name, rec.Code, rec.Body.String())
		}
		if !fired(kicks.github) {
			t.Errorf("pat %s did not kick the GitHub poller", step.name)
		}
		if fired(kicks.jira) {
			t.Errorf("pat %s kicked the Jira poller", step.name)
		}
		if pollReady(t, s, "github") {
			t.Errorf("GitHub readiness survived a pat %s", step.name)
		}
		if !pollReady(t, s, "jira") {
			t.Errorf("a pat %s cleared Jira readiness", step.name)
		}
	}
}

// orgPATLogin reads github_pat_login off the org settings GET.
func orgPATLogin(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
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
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/github/pat"
}

// commitGitHubHost saves the org's GitHub base URL through the settings route,
// the one door that owns that column.
func commitGitHubHost(t *testing.T, s *Server, baseURL string) {
	t.Helper()
	if rec := patchOrgSettings(t, s, map[string]any{"github_base_url": baseURL}); rec.Code != http.StatusOK {
		t.Fatalf("commit github host %q = %d: %s", baseURL, rec.Code, rec.Body.String())
	}
}

// bindOrgGitHubPAT commits baseURL as the org's host and then binds the token,
// which validates against the committed host; the route takes no host itself.
func bindOrgGitHubPAT(t *testing.T, s *Server, baseURL, pat string) *httptest.ResponseRecorder {
	t.Helper()
	commitGitHubHost(t, s, baseURL)
	return doJSON(t, s, http.MethodPut, patRoute(), map[string]any{"pat": pat})
}

// --- the host the bind validates against ---------------------------------

// TestGitHubPATPut_ValidatesAgainstTheCommittedHost: the bind probes the org's
// saved GitHub URL, read fresh each call, and writes no settings column, so the
// settings version the setup wizard holds across the bind does not move.
func TestGitHubPATPut_ValidatesAgainstTheCommittedHost(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	first, firstHits := recordingGitHubStub(t, "first-bot")
	second, secondHits := recordingGitHubStub(t, "second-bot")

	commitGitHubHost(t, s, first.URL)
	versionAfterCommit := orgSettingsVersion(t, s)

	if rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{"pat": "ghp_one"}); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "first-bot" {
		t.Errorf("bound login = %q, want the committed host's account", got)
	}
	if *firstHits == 0 {
		t.Error("the committed host was never probed")
	}
	if n := *secondHits; n != 0 {
		t.Errorf("an uncommitted host was probed %d time(s)", n)
	}

	// The bind stores the credential, not the host — github_base_url still
	// holds exactly what the settings route put there, at the same version.
	if got := orgGitHubHost(t, s); got != first.URL {
		t.Errorf("github_base_url = %q, want the committed %q untouched by the bind", got, first.URL)
	}
	if got := orgSettingsVersion(t, s); got != versionAfterCommit {
		t.Errorf("settings version moved %d → %d; the bind writes no settings column", versionAfterCommit, got)
	}

	// Move the org to the other deployment: the token comes off first,
	// since the URL does not move while a credential is connected, and the
	// next bind follows the new setting.
	if rec := doJSON(t, s, http.MethodDelete, patRoute(), nil); rec.Code != http.StatusOK {
		t.Fatalf("pat unbind = %d, body=%s", rec.Code, rec.Body.String())
	}
	before := *firstHits
	commitGitHubHost(t, s, second.URL)
	if rec := doJSON(t, s, http.MethodPut, patRoute(), map[string]any{"pat": "ghp_two"}); rec.Code != http.StatusOK {
		t.Fatalf("pat rebind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "second-bot" {
		t.Errorf("rebound login = %q, want the newly committed host's account", got)
	}
	if *secondHits == 0 {
		t.Error("the newly committed host was never probed")
	}
	if *firstHits != before {
		t.Errorf("the previous host was probed again (%d → %d) — the bind followed the credential's host, not the org's", before, *firstHits)
	}
}

// githubTokenStub is githubUserStub answering with the login each token maps
// to, so a test can rotate the credential on one host.
func githubTokenStub(t *testing.T, logins map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, ok := logins[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": login})
		case "/api/v3/user/emails":
			writeGitHubPrimaryEmail(w, login+"@example.com")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingGitHubStub is githubUserStub plus a request counter, so a test can
// assert which of two hosts the backend actually reached.
func recordingGitHubStub(t *testing.T, login string) (*httptest.Server, *int) {
	t.Helper()
	var (
		mu   sync.Mutex
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v3/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": login})
		case "/api/v3/user/emails":
			writeGitHubPrimaryEmail(w, login+"@example.com")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// orgGitHubHost reads the org's saved GitHub base URL back off the settings
// resource — the same value the setup wizard and Settings render.
func orgGitHubHost(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET org settings = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		GitHubBaseURL string `json:"github_base_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out.GitHubBaseURL
}
