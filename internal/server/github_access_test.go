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

// seedBYOAppCredentialClass records that the org is in the BYO-App credential
// system, which is what registering or importing an App writes in the same
// transaction as the registration row.
//
// A fixture that inserts an org_github_apps row directly has to write this too,
// or it is modelling a state the product cannot reach — an org with an App and
// a class saying it has none. The handlers dispatch on the class, so without it
// such a fixture is served as a PAT org.
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
	}); err != nil {
		t.Fatalf("seed installation %s: %v", login, err)
	}
}

func setOrgGitHubBase(t *testing.T, s *Server, base string) {
	t.Helper()
	if _, err := s.orgs.UpdateSettings(context.Background(), runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: base}); err != nil {
		t.Fatalf("set org github base: %v", err)
	}
}

// ghAppsRaceHook wraps the App store and fires a one-shot hook right after one
// of the two reads the credential transitions guard on returns. It stands in
// for a concurrent transition committing inside the window a handler's pre-lock
// read is stale across — the window that is real because two of the three
// handlers put a GitHub round-trip in it, and that no amount of test
// concurrency could pin down deterministically.
//
// The hook runs after the wrapped read has already produced its answer, so the
// handler receives the PRE-mutation row and then proceeds into a world where it
// no longer holds. Only the first call is hooked; the re-read inside the
// critical section sees the mutation, which is the whole point.
//
// This wraps s.githubApps, which is NOT the store a handler's own
// tx.GitHubApps calls run against inside s.tx.WithTx — WithTx builds a fresh
// TxStores straight off the *sql.Tx, so a hook here only reaches the
// pre-transaction reads (GetForOrgSystem, ListInstallationsForOrgSystem), never
// a write made through tx. See setActiveReturnsNilTx below for the shape that
// reaches inside the transaction.
type ghAppsRaceHook struct {
	db.GitHubAppsStore
	afterGet  func() // fires once, after the first GetForOrgSystem
	afterList func() // fires once, after the first ListInstallationsForOrgSystem
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

func (g *ghAppsRaceHook) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	insts, err := g.GitHubAppsStore.ListInstallationsForOrgSystem(ctx, orgID)
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

// setActiveReturnsNilTx wraps a TxRunner and swaps a setActiveNilStore into
// every transaction it opens, leaving the rest of the stores real — the same
// injection shape failingSecretsTx uses for tx.Secrets. This is what actually
// reaches tx.GitHubApps.SetActive inside s.tx.WithTx, unlike ghAppsRaceHook
// above: nothing in this codebase can make the registration vanish between
// the cutover's authoritative GetForOrgSystem and its SetActive while the
// lock is held (every writer of org_github_apps serializes on it), so there's
// no real interleaving to race for. What this proves instead is that the
// handler itself holds if SetActive ever reports it flipped nothing — the
// same guarantee GetForOrgSystem's nil-on-absent contract gives the read
// side, now enforced on the write side too.
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

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE github/app = %d, want 200; body=%s", rec.Code, rec.Body.String())
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

	rec := doJSON(t, s, http.MethodDelete, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE active github/app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if app, _ := s.githubApps.GetForOrgSystem(context.Background(), runmode.LocalDefaultOrgID); app == nil {
		t.Error("active app was deleted on a 409 discard")
	}
}

// TestGitHubAppDiscard_AppGoesLiveUnderTheGuard is the discard half of the
// serialization the three transitions share: a cutover commits between the
// discard's read and its teardown, so the staged App the 409 guard cleared is
// live by the time the teardown would run.
//
// The fixture commits BOTH halves of the cutover's transaction — the active
// flip and the PAT delete — because it is the second half that sets the stake.
// Once the PAT is gone the App is the org's only credential, so a discard that
// proceeds on the stale "staged" answer isn't tearing down an abandoned switch,
// it is taking the workspace's GitHub access with it.
//
// The guard has to be re-evaluated under the lock for the refusal to happen at
// all; against the unserialized handler the stale answer stands and the
// teardown commits.
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

// --- cutover -------------------------------------------------------------

func TestGitHubAppCutover_NoApp404(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cutover with no app = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

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

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/app/cutover", nil)
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

// TestGitHubAppCutover_RegistrationVanishesUnderTheGuard is the interleaving
// that costs the org every credential it has. A discard commits while the
// cutover is inside BackfillInstallationsFromAPI — seconds, not microseconds —
// so by the time the cutover writes, the registration it verified is gone.
// SetActive is an unchecked UPDATE and flips nothing; the PAT delete is
// unconditional and lands. The org ends with no App, no PAT, and an audit row
// saying the App became its credential.
//
// The lock is what makes the re-read possible, but the re-read is what refuses:
// the PAT surviving is the assertion that matters.
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

// TestGitHubAppCutover_SetActiveReportsNoRow is the branch
// TestGitHubAppCutover_RegistrationVanishesUnderTheGuard doesn't reach: that
// test's discard lands before the lock, so the authoritative GetForOrgSystem
// inside it already sees app == nil and refuses at the earlier guard — it
// never gets as far as SetActive. Once the lock is held, nothing in this file
// can make the registration vanish between that read and the cutover's
// SetActive, so there is no real interleaving left to race for; this forces
// the store's answer directly instead, to prove the handler holds on its own
// if SetActive were ever to report it flipped nothing — the same
// unchecked-UPDATE hole the file's header describes, now closed on the write
// side rather than only guarded on the read side.
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

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/switch-to", map[string]string{"pat": "ghp_valid"})
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

func TestGitHubAccessSwitchToPAT_NoApp404(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	stub := newGitHubAccessStub(t, ghAccessStub{login: "octocat"})
	setOrgGitHubBase(t, s, stub.URL)

	rec := doJSON(t, s, http.MethodPost, "/api/orgs/"+runmode.LocalDefaultOrgID+"/github/pat/switch-to", map[string]string{"pat": "ghp_valid"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("switch-to-pat with no app = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGitHubAccessSwitchToPAT_RegistrationVanishesUnderTheGuard runs a discard
// through the window auth.ValidateGitHub opens between this handler's 404 guard
// and its teardown. What survives the window is not just the guard's answer but
// the row itself — the refs teardownAppSecrets deletes ride on it, so a stale
// pass tears down secrets by the identity of a registration that no longer
// exists and reports a 200 for an App it did not remove.
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

	rec := bindOrgGitHubPAT(t, s, "https://github.com", "ghp_new")
	if rec.Code != http.StatusConflict {
		t.Fatalf("pat bind with app = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestOrgSettingsSave_CarriesNoCredential proves the bulk settings save can't
// reach the vault: a body carrying a token is REJECTED (the field is not part
// of the schema, and strict decoding says so instead of ignoring it) and
// stores nothing. This is the structural version of the old XOR guard on this
// route — there is no longer a code path from here to a credential write to
// guard.
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
	if rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_first"); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := orgPATLogin(t, s); got != "acme-bot" {
		t.Errorf("github_pat_login = %q after bind, want acme-bot", got)
	}

	// Rotation: same route, a token that authenticates as someone else.
	rotated := githubUserStub(t, "acme-bot-2")
	if rec := bindOrgGitHubPAT(t, s, rotated.URL, "ghp_second"); rec.Code != http.StatusOK {
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

// TestOrgSettingsGet_EnvOverlaidPATIsSettled covers the local-mode env overlay,
// where TRIAGE_FACTORY_GITHUB_BOT_PAT supplies the token TF actually
// authenticates with and every write to the vault is invisible to the next read.
//
// Two things have to give. The recorded login describes the last token bound
// through a route, which is NOT the token in use, so naming it on a surface
// whose job is "here's the account you're replacing" would point the operator
// at the wrong account. And the replacement itself can't be honored at all —
// hence the flag the UI reads to render the credential as settled instead of
// offering a control that would report success and change nothing.
func TestOrgSettingsGet_EnvOverlaidPATIsSettled(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	// A real bind first, so there IS a recorded login to (wrongly) show.
	gh := githubUserStub(t, "acme-bot")
	if rec := bindOrgGitHubPAT(t, s, gh.URL, "ghp_bound"); rec.Code != http.StatusOK {
		t.Fatalf("pat bind = %d, body=%s", rec.Code, rec.Body.String())
	}
	if view := orgCredentialView(t, s); view.Login != "acme-bot" || view.PATEnvProvided {
		t.Fatalf("before the overlay: %+v, want login acme-bot and env_provided false", view)
	}

	// The operator starts the server with a bot token in the environment. It now
	// outranks the bound one on every read.
	t.Setenv("TRIAGE_FACTORY_GITHUB_BOT_PAT", "ghp_from_env")

	view := orgCredentialView(t, s)
	if !view.PATEnvProvided {
		t.Errorf("github_pat_env_provided = false with the env var set, want true")
	}
	if !view.HasPAT {
		t.Errorf("has_github_pat = false, want true — the env token IS the live credential")
	}
	if view.Login != "" {
		t.Errorf("github_pat_login = %q under the env overlay, want empty — the recorded login "+
			"belongs to the shadowed token, not the one in use", view.Login)
	}
}

// TestOrgSettingsGet_EnvOverlaidJiraIsSettled is the Jira half. Either env half
// is enough: the resolver reads the host from the same overlaid secret, so an
// env-supplied URL makes a rebind partly ineffective even when the token isn't
// shadowed.
func TestOrgSettingsGet_EnvOverlaidJiraIsSettled(t *testing.T) {
	keyring.MockInit()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	if orgCredentialView(t, s).JiraEnvProvided {
		t.Fatalf("jira_credential_env_provided = true with no env vars set")
	}

	t.Setenv("TRIAGE_FACTORY_JIRA_URL", "https://jira.example.com")
	if !orgCredentialView(t, s).JiraEnvProvided {
		t.Errorf("jira_credential_env_provided = false with the URL env var set, want true")
	}
}

// orgCredentialView is the org settings GET's credential-facing fields — what
// Settings reads to decide between reporting a credential and offering to
// replace it.
type orgCredentialViewJSON struct {
	HasPAT          bool   `json:"has_github_pat"`
	Login           string `json:"github_pat_login"`
	PATEnvProvided  bool   `json:"github_pat_env_provided"`
	JiraEnvProvided bool   `json:"jira_credential_env_provided"`
}

func orgCredentialView(t *testing.T, s *Server) orgCredentialViewJSON {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, orgSettingsPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/org = %d: %s", rec.Code, rec.Body.String())
	}
	var out orgCredentialViewJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode org settings: %v", err)
	}
	return out
}

// orgPATLogin reads github_pat_login off the org settings GET.
func orgPATLogin(t *testing.T, s *Server) string {
	t.Helper()
	return orgCredentialView(t, s).Login
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

// bindOrgGitHubPAT binds a token the way the product does: commit the host as
// config, then bind the credential, which validates against the committed
// value. A test standing up a fake GitHub host has to commit it for the same
// reason a user does — the route takes no host of its own.
func bindOrgGitHubPAT(t *testing.T, s *Server, baseURL, pat string) *httptest.ResponseRecorder {
	t.Helper()
	commitGitHubHost(t, s, baseURL)
	return doJSON(t, s, http.MethodPut, patRoute(), map[string]any{"pat": pat})
}

// --- the host the bind validates against ---------------------------------

// TestGitHubPATPut_ValidatesAgainstTheCommittedHost pins where the bind gets
// its host: the org's saved GitHub URL, read fresh on every call. Two live
// stubs stand in for two GitHub deployments, and the committed host moves
// between the two binds — so a bind that followed anything stickier than the
// current setting (a cached value, the credential's own copy of the host) would
// probe the wrong server and resolve the wrong account.
//
// It also pins the two things that follow from the host being config rather
// than credential: the bind writes no host, and the settings row's concurrency
// token doesn't move — the setup wizard commits the URL a step earlier and
// holds that token across the bind.
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

	// Move the workspace to the other deployment and rotate. The bind must
	// follow the setting, not the host the previous credential was bound to.
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
