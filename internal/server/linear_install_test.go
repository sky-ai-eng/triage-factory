package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// linearOAuthFake is Linear's token and revoke endpoints. "good-code" exchanges
// for the install's pair; a refresh token in refuse answers invalid_grant;
// any other refresh rotates. Every revoke is recorded.
type linearOAuthFake struct {
	URL string

	mu      sync.Mutex
	revoked []string
	refuse  map[string]bool
	grants  []string
	// onExchange runs before a code exchange is answered, outside the fake's
	// lock: a test's way to change something while a ceremony is mid-flight.
	onExchange func()
}

func newLinearOAuthFake(t *testing.T) *linearOAuthFake {
	t.Helper()
	f := &linearOAuthFake{refuse: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		hook := f.onExchange
		f.mu.Unlock()
		if hook != nil && r.URL.Path == "/token" && r.PostForm.Get("grant_type") == "authorization_code" {
			hook()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/revoke":
			f.revoked = append(f.revoked, r.PostForm.Get("token"))
			w.WriteHeader(http.StatusOK)
		case "/token":
			grant := r.PostForm.Get("grant_type")
			f.grants = append(f.grants, grant)
			switch {
			case grant == "authorization_code" && r.PostForm.Get("code") == "good-code":
				_, _ = w.Write([]byte(`{"access_token":"acc-install","refresh_token":"ref-install","expires_in":86399}`))
			case grant == "refresh_token" && !f.refuse[r.PostForm.Get("refresh_token")]:
				_, _ = w.Write([]byte(`{"access_token":"acc-refreshed","refresh_token":"ref-refreshed","expires_in":86399}`))
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *linearOAuthFake) Revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func (f *linearOAuthFake) Grants() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.grants...)
}

// linearInstallRig is the access rig plus a deployment identity, a fake OAuth
// endpoint and a registered app of the org's own, with the whoami fake
// answering as the app user.
type linearInstallRig struct {
	*linearAccessRig
	oauth *linearOAuthFake
	key   [32]byte
}

func newLinearInstallRig(t *testing.T) *linearInstallRig {
	t.Helper()
	base := newLinearAccessRig(t)
	base.fake.set("app")
	r := &linearInstallRig{linearAccessRig: base, oauth: newLinearOAuthFake(t), key: [32]byte{7}}
	r.s.SetDeployConfig("http://localhost:3000", r.key)
	r.s.linearOAuthMinter = linearoauth.NewMinterWithEndpoints(r.oauth.URL+"/token", r.oauth.URL+"/revoke")
	r.s.linearResolver = linear.NewResolverWithInstall(r.stores.Secrets, r.stores.Orgs,
		linearoauth.NewTokenCache(r.s.linearOAuthMinter, r.s.linearOAuthApps, r.stores.Secrets, r.stores.LinearInstalls, r.s.linearCredentialLock))
	r.registerApp(t, "lin-client-1")
	return r
}

func linearAppPath() string { return "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/app" }

func linearInstallStartPath() string {
	return "/api/orgs/" + runmode.LocalDefaultOrgID + "/linear/install/start"
}

func (r *linearInstallRig) registerApp(t *testing.T, clientID string) linearAppStatusResponse {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodPost, linearAppPath(), map[string]any{"client_id": clientID, "client_secret": "lin-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST linear app: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearAppStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// get sends a GET carrying cookies through the full mux.
func (r *linearInstallRig) get(t *testing.T, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.s.mux.ServeHTTP(rec, req)
	return rec
}

// start runs the start leg and returns the state cookie and the nonce Linear
// would echo back.
func (r *linearInstallRig) start(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := r.get(t, linearInstallStartPath())
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if loc.Host != "linear.app" || loc.Path != "/oauth/authorize" {
		t.Fatalf("start redirected to %s, want Linear's authorize page", loc)
	}
	q := loc.Query()
	if q.Get("actor") != "app" || q.Get("client_id") != "lin-client-1" ||
		q.Get("redirect_uri") != "http://localhost:3000/api/linear/install/callback" {
		t.Errorf("authorize query = %v, want actor=app for the org's app and the static callback", q)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == linearInstallStateCookieName {
			if c.Path != linearInstallStatePath || !c.HttpOnly || c.MaxAge != 600 {
				t.Errorf("state cookie = %+v, want HttpOnly, path %s, ten minutes", c, linearInstallStatePath)
			}
			return c, q.Get("state")
		}
	}
	t.Fatal("start set no state cookie")
	return nil, ""
}

func callbackPath(params url.Values) string {
	return linearInstallCallbackPath + "?" + params.Encode()
}

// expectRedirect asserts a callback redirected with key=value on its query and
// returns the location.
func expectRedirect(t *testing.T, rec *httptest.ResponseRecorder, key, value string) *url.URL {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if got := loc.Query().Get(key); got != value {
		t.Errorf("redirect %s: %s = %q, want %q", loc, key, got, value)
	}
	return loc
}

// install runs the whole ceremony to a successful callback.
func (r *linearInstallRig) install(t *testing.T) {
	t.Helper()
	cookie, state := r.start(t)
	rec := r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear", "installed")
	r.expectKick(t)
}

func (r *linearInstallRig) installRow(t *testing.T) *domain.OrgLinearInstall {
	t.Helper()
	inst, err := r.stores.LinearInstalls.GetForOrgSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read install: %v", err)
	}
	return inst
}

// assertNothingWritten pins the refusal contract: no credential, no workspace,
// no install row live, no audit row.
func (r *linearInstallRig) assertNothingWritten(t *testing.T) {
	t.Helper()
	for _, k := range integrations.LinearKeys() {
		if v := r.secret(t, k); v != "" {
			t.Errorf("%s = %q, want nothing written", k, v)
		}
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "" {
		t.Errorf("workspace id = %q, want none", set.LinearWorkspaceID)
	}
	if inst := r.installRow(t); inst != nil && inst.Live() {
		t.Errorf("install row = %+v, want none live", inst)
	}
	if rows := r.credentialRows(t); len(rows) != 0 {
		t.Errorf("linear credential rows = %+v, want none", rows)
	}
}

func TestLinearInstall_HappyPath(t *testing.T) {
	r := newLinearInstallRig(t)
	r.install(t)

	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != string(linear.AuthMethodAppInstall) {
		t.Errorf("marker = %q, want app_install", v)
	}
	env, err := linear.ParseInstallCredential(r.secret(t, integrations.KeyLinearAppInstall))
	if err != nil {
		t.Fatalf("install envelope: %v", err)
	}
	if env.WorkspaceID != "org-acme" || env.AppUserID != "lu-ada" || env.RefreshToken != "ref-install" || env.ClientID != "lin-client-1" {
		t.Errorf("envelope = %+v, want the workspace, app user, refresh token and client", env)
	}
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "" {
		t.Errorf("api key = %q, want none", v)
	}
	if set := r.orgSettings(t); set.LinearWorkspaceID != "org-acme" || set.LinearWorkspaceURLKey != "acme" {
		t.Errorf("workspace columns = (%q, %q), want (org-acme, acme)", set.LinearWorkspaceID, set.LinearWorkspaceURLKey)
	}
	inst := r.installRow(t)
	if inst == nil || !inst.Live() || inst.WorkspaceID != "org-acme" || inst.WorkspaceURLKey != "acme" ||
		inst.AppUserID != "lu-ada" || inst.AppClientID != "lin-client-1" || inst.InstalledByUserID != runmode.LocalDefaultUserID {
		t.Errorf("install row = %+v", inst)
	}
	if env.InstallID == "" || env.InstallID != inst.InstallID {
		t.Errorf("envelope install_id %q, want the row's %q", env.InstallID, inst.InstallID)
	}

	rows := r.credentialRows(t)
	if len(rows) != 1 || rows[0].Action != domain.AccessActionCredentialSet {
		t.Fatalf("linear credential rows = %+v, want one credential_set", rows)
	}
	var detail map[string]string
	_ = json.Unmarshal([]byte(rows[0].DetailJSON), &detail)
	if detail["shape"] != "app_install" || detail["workspace"] != "acme" || detail["app_user_id"] != "lu-ada" {
		t.Errorf("audit detail = %s, want the install's shape, workspace and app user", rows[0].DetailJSON)
	}

	got := r.access(t)
	if !got.Connected || got.AuthMethod != "app_install" || got.WorkspaceURLKey != "acme" ||
		got.BoundAs == nil || got.BoundAs.DisplayName != "Ada" || !got.ConnectAvailable || got.LastError != "" {
		t.Errorf("access = %+v, want connected as the app user", got)
	}
	if st := readOrgSources(t, r.s)[eventsource.KindLinear]; st != string(eventsource.StateAvailable) {
		t.Errorf("linear source = %q after an install, want available", st)
	}
	if revoked := r.oauth.Revoked(); len(revoked) != 0 {
		t.Errorf("revoked %v on a clean install, want nothing", revoked)
	}

	// The install is the org's credential: ForSystem mints from it.
	if _, err := r.s.linearResolver.ResolveSystemCredential(t.Context(), runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("resolve installed credential: %v", err)
	}
	if env, _ := linear.ParseInstallCredential(r.secret(t, integrations.KeyLinearAppInstall)); env.RefreshToken != "ref-refreshed" {
		t.Errorf("stored refresh token = %q after a refresh, want the rotated one", env.RefreshToken)
	}
}

// TestLinearInstall_ReplacesAPIKey: an org on an API key installs, and the
// key is gone in the same write that stores the install.
func TestLinearInstall_ReplacesAPIKey(t *testing.T) {
	r := newLinearInstallRig(t)
	r.fake.set("person")
	r.bind(t, "lin_api_ada")
	r.fake.set("app")

	r.install(t)
	if v := r.secret(t, integrations.KeyLinearAPIKey); v != "" {
		t.Errorf("api key = %q after the install, want it replaced", v)
	}
	if v := r.secret(t, integrations.KeyLinearAuthMethod); v != string(linear.AuthMethodAppInstall) {
		t.Errorf("marker = %q, want app_install", v)
	}
}

// TestLinearInstall_HumanViewerRefusedAndRevoked: a token that acts as a person
// means actor=app was stripped on the way through the browser. Nothing is
// stored, and both halves of the pair are revoked.
func TestLinearInstall_HumanViewerRefusedAndRevoked(t *testing.T) {
	r := newLinearInstallRig(t)
	r.fake.set("person")
	cookie, state := r.start(t)

	rec := r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear_error", "install_failed")
	r.assertNothingWritten(t)
	if got := r.oauth.Revoked(); len(got) != 2 || got[0] != "acc-install" || got[1] != "ref-install" {
		t.Errorf("revoked %v, want the access and refresh token", got)
	}
	r.expectNoKick(t)
}

// TestLinearInstall_StateRefusals: every callback that cannot prove it is the
// ceremony this caller started refuses before talking to Linear.
func TestLinearInstall_StateRefusals(t *testing.T) {
	cases := map[string]func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder{
		"no cookie": func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder {
			_, state := r.start(t)
			return r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}))
		},
		"state mismatch": func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder {
			cookie, _ := r.start(t)
			return r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {"forged"}}), cookie)
		},
		"expired cookie": func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder {
			signed, err := connectState{
				OrgID: runmode.LocalDefaultOrgID, UserID: runmode.LocalDefaultUserID, CSRF: "nonce",
				ExpiresAt: time.Now().Add(-time.Minute).Unix(),
			}.sign(r.key)
			if err != nil {
				t.Fatal(err)
			}
			return r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {"nonce"}}),
				&http.Cookie{Name: linearInstallStateCookieName, Value: signed})
		},
		"forged cookie": func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder {
			signed, _ := connectState{
				OrgID: runmode.LocalDefaultOrgID, UserID: runmode.LocalDefaultUserID, CSRF: "nonce",
				ExpiresAt: time.Now().Add(time.Minute).Unix(),
			}.sign([32]byte{9})
			return r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {"nonce"}}),
				&http.Cookie{Name: linearInstallStateCookieName, Value: signed})
		},
		"another user's cookie": func(t *testing.T, r *linearInstallRig) *httptest.ResponseRecorder {
			signed, _ := connectState{
				OrgID: runmode.LocalDefaultOrgID, UserID: "someone-else", CSRF: "nonce",
				ExpiresAt: time.Now().Add(time.Minute).Unix(),
			}.sign(r.key)
			return r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {"nonce"}}),
				&http.Cookie{Name: linearInstallStateCookieName, Value: signed})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			r := newLinearInstallRig(t)
			rec := run(t, r)
			expectRedirect(t, rec, "linear_error", "state")
			r.assertNothingWritten(t)
			if grants := r.oauth.Grants(); len(grants) != 0 {
				t.Errorf("exchanged a code (%v) on a refused state", grants)
			}
			cleared := false
			for _, c := range rec.Result().Cookies() {
				cleared = cleared || (c.Name == linearInstallStateCookieName && c.MaxAge < 0)
			}
			if !cleared {
				t.Error("the callback did not clear the state cookie")
			}
		})
	}
}

func TestLinearInstall_Denied(t *testing.T) {
	r := newLinearInstallRig(t)
	cookie, state := r.start(t)
	rec := r.get(t, callbackPath(url.Values{"error": {"access_denied"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear_error", "denied")
	r.assertNothingWritten(t)
}

func TestLinearInstall_FailedExchange(t *testing.T) {
	r := newLinearInstallRig(t)
	cookie, state := r.start(t)
	rec := r.get(t, callbackPath(url.Values{"code": {"bad-code"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear_error", "install_failed")
	r.assertNothingWritten(t)
}

func TestLinearInstall_NoApp(t *testing.T) {
	r := newLinearInstallRig(t)
	if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE app: %d: %s", rec.Code, rec.Body.String())
	}
	rec := r.get(t, linearInstallStartPath()+"?return_to=/setup")
	loc := expectRedirect(t, rec, "linear_error", "no_app")
	if loc.Path != "/setup" {
		t.Errorf("redirected to %s, want back to the return_to", loc.Path)
	}
}

// TestLinearInstall_WorkspaceTaken: a workspace another org holds is refused,
// the minted pair revoked, and nothing written for this org.
func TestLinearInstall_WorkspaceTaken(t *testing.T) {
	r := newLinearInstallRig(t)
	const otherOrg = "22222222-2222-2222-2222-222222222222"
	if _, err := r.s.db.Exec(`INSERT INTO orgs (id, slug, name) VALUES (?, 'other', 'Other')`, otherOrg); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := r.stores.LinearInstalls.UpsertSystem(t.Context(), domain.OrgLinearInstall{
		OrgID: otherOrg, InstallID: "inst-other", WorkspaceID: "org-acme", WorkspaceURLKey: "acme", AppUserID: "lu-other",
		AppClientID: "other-client", InstalledAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed other org's install: %v", err)
	}

	cookie, state := r.start(t)
	rec := r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
	expectRedirect(t, rec, "linear_error", "workspace_taken")
	r.assertNothingWritten(t)
	if inst := r.installRow(t); inst != nil {
		t.Errorf("install row = %+v, want none for this org", inst)
	}
	if got := r.oauth.Revoked(); len(got) != 2 {
		t.Errorf("revoked %v, want the minted pair", got)
	}
	r.expectNoKick(t)
}

// TestLinearInstall_Disconnect: the disconnect revokes the install's refresh
// token, marks its row removed, and clears the credential, after which the
// source reads unconfigured.
func TestLinearInstall_Disconnect(t *testing.T) {
	r := newLinearInstallRig(t)
	r.install(t)

	rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	r.expectKick(t)

	if got := r.oauth.Revoked(); len(got) != 1 || got[0] != "ref-install" {
		t.Errorf("revoked %v, want the install's refresh token", got)
	}
	inst := r.installRow(t)
	if inst == nil || inst.Live() || inst.RemovedReason != domain.LinearInstallRemovedDisconnected {
		t.Errorf("install row = %+v, want removed as disconnected", inst)
	}
	for _, k := range integrations.LinearKeys() {
		if v := r.secret(t, k); v != "" {
			t.Errorf("%s = %q after the disconnect, want cleared", k, v)
		}
	}
	if got := r.access(t); got.Connected || got.LastError != "" {
		t.Errorf("access = %+v, want disconnected with no error", got)
	}
	if st := readOrgSources(t, r.s)[eventsource.KindLinear]; st != string(eventsource.StateUnconfigured) {
		t.Errorf("linear source = %q after the disconnect, want unconfigured", st)
	}
}

// TestLinearInstall_AppChangedMidCeremonyIsRefused: the code is exchanged
// against the app that resolved when the callback started. An app replaced or
// removed before the install is stored would leave it unable to refresh, so
// the store refuses, revokes the pair, and writes nothing.
func TestLinearInstall_AppChangedMidCeremonyIsRefused(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, r *linearInstallRig){
		"replaced": func(t *testing.T, r *linearInstallRig) { r.registerApp(t, "lin-client-2") },
		"removed": func(t *testing.T, r *linearInstallRig) {
			if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
				t.Errorf("DELETE app mid-ceremony: %d: %s", rec.Code, rec.Body.String())
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLinearInstallRig(t)
			cookie, state := r.start(t)
			r.oauth.mu.Lock()
			r.oauth.onExchange = func() { change(t, r) }
			r.oauth.mu.Unlock()

			rec := r.get(t, callbackPath(url.Values{"code": {"good-code"}, "state": {state}}), cookie)
			expectRedirect(t, rec, "linear_error", "install_failed")
			r.assertNothingWritten(t)
			if inst := r.installRow(t); inst != nil {
				t.Errorf("install row = %+v, want none", inst)
			}
			if got := r.oauth.Revoked(); len(got) != 2 {
				t.Errorf("revoked %v, want the minted pair", got)
			}
		})
	}
}

// TestLinearInstall_DisconnectReleasesALeftoverRow: a live install row with no
// credential behind it — a release that failed after its disconnect committed
// — is released by the next disconnect, so the retry frees the workspace.
func TestLinearInstall_DisconnectReleasesALeftoverRow(t *testing.T) {
	r := newLinearInstallRig(t)
	if _, err := r.stores.LinearInstalls.UpsertSystem(t.Context(), domain.OrgLinearInstall{
		OrgID: runmode.LocalDefaultOrgID, InstallID: "inst-left", WorkspaceID: "org-acme", WorkspaceURLKey: "acme",
		AppUserID: "lu-ada", AppClientID: "lin-client-1", InstalledAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed leftover install: %v", err)
	}

	if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d: %s", rec.Code, rec.Body.String())
	}
	if inst := r.installRow(t); inst == nil || inst.Live() || inst.RemovedReason != domain.LinearInstallRemovedDisconnected {
		t.Errorf("leftover install = %+v, want released as disconnected", inst)
	}
	r.expectNoKick(t)
}

// TestLinearInstall_RefreshRefusedMarksRemoved: Linear refusing the install's
// refresh token is the app removed in Linear. The install is marked revoked,
// the org reads disconnected with last_error, and a key may then be bound.
func TestLinearInstall_RefreshRefusedMarksRemoved(t *testing.T) {
	r := newLinearInstallRig(t)
	r.install(t)
	r.oauth.mu.Lock()
	r.oauth.refuse["ref-install"] = true
	r.oauth.mu.Unlock()

	_, err := r.s.linearResolver.ForSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("ForSystem err = %v, want the revoked install", err)
	}
	inst := r.installRow(t)
	if inst == nil || inst.RemovedReason != domain.LinearInstallRemovedRevoked {
		t.Errorf("install row = %+v, want removed as install_revoked", inst)
	}
	if v := r.secret(t, integrations.KeyLinearAppInstall); v != "" {
		t.Error("the refused envelope is still stored")
	}
	got := r.access(t)
	if got.Connected || got.LastError != "install_revoked" {
		t.Errorf("access = %+v, want disconnected with last_error install_revoked", got)
	}
	if st := readOrgSources(t, r.s)[eventsource.KindLinear]; st != string(eventsource.StateUnconfigured) {
		t.Errorf("linear source = %q, want unconfigured", st)
	}

	// Nothing is left to revoke, so a key binds over the dead install.
	r.fake.set("person")
	if status := r.bind(t, "lin_api_ada"); !status.Connected || status.AuthMethod != "api_key" {
		t.Errorf("bind over a revoked install = %+v, want the key connected", status)
	}
}

func TestLinearApp_StatusAndCreateLink(t *testing.T) {
	r := newLinearInstallRig(t)
	rec := doJSON(t, r.s, http.MethodGet, linearAppPath(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d: %s", rec.Code, rec.Body.String())
	}
	var got linearAppStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.App == nil || got.App.ClientID != "lin-client-1" || !got.InstallAvailable || got.UsingDeploymentDefault {
		t.Errorf("status = %+v, want the org's own app", got)
	}
	wantURIs := []string{"http://localhost:3000/api/linear/install/callback", "http://localhost:3000/api/linear/connect/callback"}
	if len(got.RedirectURIs) != 2 || got.RedirectURIs[0] != wantURIs[0] || got.RedirectURIs[1] != wantURIs[1] {
		t.Errorf("redirect_uris = %v, want %v", got.RedirectURIs, wantURIs)
	}
	u, err := url.Parse(got.CreateURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "linear.app" || q.Get("distribution") != "private" || q.Get("oauth.client_name") != "Triage Factory" ||
		q.Get("oauth.client_uri") != "http://localhost:3000" || q.Get("oauth.grant_types") != "authorization_code" {
		t.Errorf("create_url = %s", got.CreateURL)
	}
	if uris := q["oauth.redirect_uris"]; len(uris) != 2 || uris[0] != wantURIs[0] || uris[1] != wantURIs[1] {
		t.Errorf("create_url redirect uris = %v, want %v", uris, wantURIs)
	}
	if n := len([]rune(q.Get("developer.name"))); n < 2 || n > 80 {
		t.Errorf("developer.name = %q, want 2–80 characters", q.Get("developer.name"))
	}
}

func TestLinearDeveloperName(t *testing.T) {
	long := strings.Repeat("é", 100)
	for in, want := range map[string]string{
		"Acme":   "Acme",
		"  x  ":  linearDeveloperNameFallback,
		"":       linearDeveloperNameFallback,
		long:     strings.Repeat("é", 80),
		" Ac me": "Ac me",
	} {
		if got := linearDeveloperName(in); got != want {
			t.Errorf("linearDeveloperName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinearApp_ImportValidatesEveryField(t *testing.T) {
	r := newLinearInstallRig(t)
	rec := doJSON(t, r.s, http.MethodPost, linearAppPath(), map[string]any{"client_id": " ", "client_secret": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	items := decodeErrorItems(t, rec)
	if len(items) != 2 || items[0].Field != "client_id" || items[1].Field != "client_secret" {
		t.Errorf("errors = %+v, want both fields named", items)
	}
}

// TestLinearApp_ChangesThatStrandTheInstallAreRefused: while an install minted
// by the app is live, the app cannot be deleted or replaced by another one;
// the secret of the same app can still be rotated.
func TestLinearApp_ChangesThatStrandTheInstallAreRefused(t *testing.T) {
	r := newLinearInstallRig(t)
	r.install(t)

	rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil)
	if rec.Code != http.StatusConflict || firstError(t, rec).Reason != "CONFLICT" {
		t.Fatalf("DELETE with a live install = %d %s, want 409 CONFLICT", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, r.s, http.MethodPost, linearAppPath(), map[string]any{"client_id": "lin-client-2", "client_secret": "s"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST of another app with a live install = %d, want 409", rec.Code)
	}
	if v := r.secret(t, linearOAuthClientSecretKey); v != "lin-secret" {
		t.Errorf("client secret = %q, want the original kept", v)
	}
	r.registerApp(t, "lin-client-1") // same app, rotated secret

	if rec := doJSON(t, r.s, http.MethodDelete, linearCredentialPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d", rec.Code)
	}
	r.expectKick(t)
	if rec := doJSON(t, r.s, http.MethodDelete, linearAppPath(), nil); rec.Code != http.StatusOK {
		t.Fatalf("DELETE after the disconnect = %d: %s", rec.Code, rec.Body.String())
	}
	if v := r.secret(t, linearOAuthClientSecretKey); v != "" {
		t.Errorf("client secret = %q after the delete, want cleared", v)
	}
}

// TestLinearInstall_MultiMode drives the ceremony on Postgres: the role gates
// on both legs, the Bearer refusal on the static callback, the install written
// across the admin pool and the claims transaction, and a second org refused
// the workspace the first one holds.
func TestLinearInstall_MultiMode(t *testing.T) {
	rig := newAuthRig(t)
	s := rig.srv
	whoami := newLinearWhoamiFake(t)
	whoami.set("app")
	s.validateLinear = func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error) {
		cfg.Endpoint = whoami.URL
		return auth.ValidateLinear(ctx, cfg)
	}
	oauth := newLinearOAuthFake(t)
	s.linearOAuthMinter = linearoauth.NewMinterWithEndpoints(oauth.URL+"/token", oauth.URL+"/revoke")

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "alice-org")
	sidA := rig.signIn(alice)
	carol := rig.seedUser()
	if _, err := rig.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		carol.String(), orgA.String()); err != nil {
		t.Fatalf("seed carol membership: %v", err)
	}
	sidC := rig.signIn(carol)
	bob := rig.seedUser()
	orgB, _ := rig.seedOrg(bob, "bob-org")
	sidB := rig.signIn(bob)

	appPath := func(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/linear/app" }
	for _, reg := range []struct {
		org uuid.UUID
		sid string
	}{{orgA, sidA}, {orgB, sidB}} {
		if rec := rig.tokensJSON(http.MethodPost, appPath(reg.org), map[string]any{"client_id": "lin-client-1", "client_secret": "s"}, reg.sid, ""); rec.Code != http.StatusOK {
			t.Fatalf("register app: %d: %s", rec.Code, rec.Body.String())
		}
	}
	if rec := rig.tokensJSON(http.MethodPost, appPath(orgA), map[string]any{"client_id": "x", "client_secret": "y"}, sidC, ""); rec.Code != http.StatusForbidden {
		t.Errorf("member registering an app = %d, want 403", rec.Code)
	}

	get := func(path, sid, bearer string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if sid != "" {
			req.AddCookie(&http.Cookie{Name: s.sidCookieName(), Value: sid})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec
	}
	start := func(org uuid.UUID, sid string) (*http.Cookie, string) {
		rec := get("/api/orgs/"+org.String()+"/linear/install/start", sid, "", nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("start: %d: %s", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		for _, c := range rec.Result().Cookies() {
			if c.Name == linearInstallStateCookieName {
				return c, loc.Query().Get("state")
			}
		}
		t.Fatal("no state cookie")
		return nil, ""
	}
	callback := func(state string) string {
		return callbackPath(url.Values{"code": {"good-code"}, "state": {state}})
	}

	if rec := get("/api/orgs/"+orgA.String()+"/linear/install/start", sidC, "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member starting an install = %d, want 403", rec.Code)
	}

	// A Bearer token is refused on the static callback, even the starting
	// admin's own.
	_, bearer := rig.mintToken(alice, orgA, "ci")
	cookie, state := start(orgA, sidA)
	if rec := get(callback(state), "", bearer, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("callback over a Bearer token = %d, want 401", rec.Code)
	}
	// Another member of the org cannot spend the admin's cookie.
	cookie, state = start(orgA, sidA)
	expectRedirect(t, get(callback(state), sidC, "", cookie), "linear_error", "state")
	if len(oauth.Grants()) != 0 {
		t.Fatalf("a refused callback exchanged a code: %v", oauth.Grants())
	}

	cookie, state = start(orgA, sidA)
	expectRedirect(t, get(callback(state), sidA, "", cookie), "linear", "installed")

	stores := pgstore.New(rig.h.AdminDB, rig.h.AppDB, pgtest.SecretKey)
	inst, err := stores.LinearInstalls.GetForOrgSystem(t.Context(), orgA.String())
	if err != nil || inst == nil || !inst.Live() || inst.WorkspaceID != "org-acme" || inst.InstalledByUserID != alice.String() {
		t.Fatalf("org A install = (%+v, %v), want live for org-acme", inst, err)
	}
	if v, _ := stores.Secrets.GetSystem(t.Context(), orgA.String(), integrations.KeyLinearAuthMethod); v != string(linear.AuthMethodAppInstall) {
		t.Errorf("org A marker = %q, want app_install", v)
	}
	if v, _ := stores.Secrets.GetSystem(t.Context(), orgA.String(), integrations.KeyLinearAppInstall); v == "" {
		t.Error("org A has no install envelope")
	}
	if rec := rig.tokensJSON(http.MethodGet, "/api/orgs/"+orgA.String()+"/linear/access", nil, sidC, ""); rec.Code != http.StatusOK ||
		!decodeLinearAccess(t, rec).Connected {
		t.Errorf("member read of org A's access = %d %s, want connected", rec.Code, rec.Body.String())
	}

	// Org B installing the same workspace is refused, and writes nothing.
	revokedBefore := len(oauth.Revoked())
	cookie, state = start(orgB, sidB)
	expectRedirect(t, get(callback(state), sidB, "", cookie), "linear_error", "workspace_taken")
	if inst, _ := stores.LinearInstalls.GetForOrgSystem(t.Context(), orgB.String()); inst != nil {
		t.Errorf("org B install = %+v, want none", inst)
	}
	if v, _ := stores.Secrets.GetSystem(t.Context(), orgB.String(), integrations.KeyLinearAuthMethod); v != "" {
		t.Errorf("org B marker = %q, want none", v)
	}
	if got := len(oauth.Revoked()) - revokedBefore; got != 2 {
		t.Errorf("revoked %d tokens on workspace_taken, want the minted pair", got)
	}

	// Org A disconnects, which releases the workspace to org B.
	if rec := rig.tokensJSON(http.MethodDelete, "/api/orgs/"+orgA.String()+"/linear/access/credential", nil, sidA, ""); rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d: %s", rec.Code, rec.Body.String())
	}

	// The store waits on org B's Linear credential lock however it is held:
	// here by another session, the way another pod's handler or token cache
	// would hold it.
	releaseOther, err := linearoauth.NewCredentialLock(rig.h.AdminDB).Lock(t.Context(), orgB.String())
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer releaseOther()
	cookie, state = start(orgB, sidB)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- get(callback(state), sidB, "", cookie) }()
	select {
	case rec := <-done:
		t.Fatalf("the callback completed (%d) while another session held the org's lock", rec.Code)
	case <-time.After(300 * time.Millisecond):
	}
	releaseOther()
	select {
	case rec := <-done:
		expectRedirect(t, rec, "linear", "installed")
	case <-time.After(10 * time.Second):
		t.Fatal("the callback did not complete after the lock was released")
	}
}

func TestLinearInstallReturn(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	for _, tc := range []struct {
		org, returnTo, want string
	}{
		{"o-1", "", "/orgs/o-1/settings?linear_error=state"},
		{"o-1", "/setup?step=2", "/setup?linear_error=state&step=2"},
		{"o-1", "/orgs/o-1/org?tab=settings", "/orgs/o-1/org?linear_error=state&tab=settings"},
		{"o-1", "/orgs/o-1/org?linear_error=denied&tab=settings", "/orgs/o-1/org?linear_error=state&tab=settings"},
		{"o-1", "https://evil.example/x", "/orgs/o-1/settings?linear_error=state"},
		{"", "", "/?linear_error=state"},
	} {
		if got := linearInstallReturn(tc.org, tc.returnTo, "linear_error", "state"); got != tc.want {
			t.Errorf("linearInstallReturn(%q, %q) = %q, want %q", tc.org, tc.returnTo, got, tc.want)
		}
	}
}
