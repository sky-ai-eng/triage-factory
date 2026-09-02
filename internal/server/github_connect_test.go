package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// TestGitHubConnect_NilDeployConfig_Returns404 pins that the Connect
// endpoints 404 when deployCfg is nil (server not fully booted) — the same
// guard the register flow carries.
func TestGitHubConnect_NilDeployConfig_Returns404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	// Don't call SetDeployConfig — deployCfg stays nil.

	org := runmode.LocalDefaultOrgID
	for _, path := range []string{
		"/api/orgs/" + org + "/github/connect/start",
		"/api/orgs/" + org + "/github/connect/callback?code=x&state=y",
	} {
		rec := doJSON(t, s, "GET", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", path, rec.Code)
		}
	}
}

// TestGitHubConnect_StateToken_RoundTrip verifies the HMAC-signed connect
// state signs + parses, and that expired / tampered / malformed tokens are
// rejected — the CSRF + session-binding carrier for the OAuth dance.
func TestGitHubConnect_StateToken_RoundTrip(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	st := connectState{
		OrgID:     "00000000-0000-0000-0000-000000000099",
		UserID:    "11111111-1111-1111-1111-111111111111",
		CSRF:      "nonce",
		ReturnTo:  "/orgs/x/triage",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parsed, err := parseConnectState(signed, key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.OrgID != st.OrgID || parsed.UserID != st.UserID || parsed.CSRF != st.CSRF || parsed.ReturnTo != st.ReturnTo {
		t.Errorf("round-trip mismatch: got %+v want %+v", parsed, st)
	}

	t.Run("expired", func(t *testing.T) {
		exp := st
		exp.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		tok, _ := exp.sign(key)
		if _, err := parseConnectState(tok, key); err == nil {
			t.Error("expected error for expired token")
		}
	})
	t.Run("wrong_key", func(t *testing.T) {
		var other [32]byte
		if _, err := rand.Read(other[:]); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := parseConnectState(signed, other); err == nil {
			t.Error("expected error for wrong key")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		// "nodot" has no separator (structurally malformed); "not.a.token"
		// splits but its payload isn't valid base64 — both must be rejected.
		for _, bad := range []string{"nodot", "not.a.token"} {
			if _, err := parseConnectState(bad, key); err == nil {
				t.Errorf("expected error for malformed token %q", bad)
			}
		}
	})
}

// TestConnectErrCode pins the error-class mapping the gate UI keys off: a
// network-level failure is the distinct "host unreachable" infra state;
// anything else is a generic connect failure.
func TestConnectErrCode(t *testing.T) {
	if got := connectErrCode(fmt.Errorf("wrap: %w", auth.ErrGitHubHostUnreachable)); got != "host_unreachable" {
		t.Errorf("unreachable error mapped to %q, want host_unreachable", got)
	}
	if got := connectErrCode(errors.New("bad code")); got != "connect_failed" {
		t.Errorf("generic error mapped to %q, want connect_failed", got)
	}
}

// TestResolveGitHubHost pins that the identity key is path-preserving — it
// must agree with the raw orgSet.GitHubBaseURL key the PAT writer and the
// factory/me/settings readers use, so a base URL with a path component keys
// identically across all of them. A bare-origin derivation (gitHubWebOrigin)
// would strip the path, splitting Connect's writes from the other sites'
// reads. Empty resolves to the deployment's default GitHub — github.com with
// the variable unset, as here, which is also the host the login_claim row keys
// under; malformed bases are rejected.
func TestResolveGitHubHost(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://github.com", "https://github.com", true},
		{"", "https://github.com", true},                    // empty → the deployment default (github.com here)
		{"https://github.com/", "https://github.com", true}, // trailing slash trimmed
		{"https://git.corp.example.com", "https://git.corp.example.com", true},
		{"https://git.corp.example.com/enterprise", "https://git.corp.example.com/enterprise", true}, // path PRESERVED
		{"ftp://github.com", "", false},
		{"not-a-url", "", false},
	}
	for _, tc := range cases {
		got, ok := resolveGitHubHost(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("resolveGitHubHost(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestGitHubConnect_ManifestIncludesConnectCallback pins that a freshly-built
// App manifest registers the Connect callback in callback_urls — without it,
// GitHub would reject the user-to-server redirect_uri at consent time. The
// register redirect_url stays the manifest-conversion callback.
func TestGitHubConnect_ManifestIncludesConnectCallback(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", key)

	org := runmode.LocalDefaultOrgID
	_, manifestJSON, _, err := s.buildManifestAndState(context.Background(), org, runmode.LocalDefaultUserID, "user", "testuser", "settings")
	if err != nil {
		t.Fatalf("buildManifestAndState: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(manifestJSON), &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}

	rawCBs, ok := m["callback_urls"].([]any)
	if !ok {
		t.Fatalf("callback_urls missing or not a list: %s", manifestJSON)
	}
	cbs := make(map[string]bool, len(rawCBs))
	for _, c := range rawCBs {
		cbs[c.(string)] = true
	}
	wantConnect := "http://localhost:3000/api/orgs/" + org + "/github/connect/callback"
	wantRegister := "http://localhost:3000/api/orgs/" + org + "/github/app/register/callback"
	if !cbs[wantConnect] {
		t.Errorf("callback_urls missing the Connect callback %q: %v", wantConnect, rawCBs)
	}
	if !cbs[wantRegister] {
		t.Errorf("callback_urls missing the register callback %q: %v", wantRegister, rawCBs)
	}
	if got, _ := m["redirect_url"].(string); got != wantRegister {
		t.Errorf("redirect_url = %q, want the register callback %q", got, wantRegister)
	}
}

// ---- multi-mode (Postgres-backed) ----

// seedGitHubApp registers an org_github_apps row and stores its client_secret
// in the org's Vault, mirroring the state a completed register flow leaves —
// the precondition for Connect (which reuses the App's client_id/secret).
func seedGitHubApp(t *testing.T, rig *authRig, orgID, userID, clientID, clientSecret string) {
	t.Helper()
	const ref = "github_app_999_client_secret"
	if err := rig.srv.tx.WithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
		if _, err := tx.GitHubApps.CreateForOrg(context.Background(), domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              "999",
			Slug:               "tf-connect-test",
			ClientID:           clientID,
			ClientSecretRef:    ref,
			PEMRef:             "github_app_999_pem",
			RegisteredByUserID: userID,
		}); err != nil {
			return err
		}
		// The class registration writes alongside the row, in the same
		// transaction — a fixture that skips it models an unreachable state.
		if _, err := tx.Orgs.SetGitHubCredentialClass(context.Background(), orgID, domain.GitHubCredentialClassBYOApp); err != nil {
			return err
		}
		return tx.Secrets.Put(context.Background(), orgID, ref, clientSecret, "test client secret")
	}); err != nil {
		t.Fatalf("seed github app: %v", err)
	}
}

func seedOrgGitHubHost(t *testing.T, rig *authRig, orgID, host string) {
	t.Helper()
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url)
		VALUES ($1, 'github', $2)
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = $2
	`, orgID, host); err != nil {
		t.Fatalf("seed org_event_sources: %v", err)
	}
}

// TestGitHubConnect_StartRedirectsToHost proves Connect targets the org's
// actual host (a GHES host here), NOT github.com — the whole reason the
// social-login path can't be the capture mechanism. Asserts the authorize
// URL carries the App's client_id + a CSRF state, and a state cookie is set.
func TestGitHubConnect_StartRedirectsToHost(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "ghes-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	const ghesHost = "https://git.corp.example.com"
	seedOrgGitHubHost(t, rig, orgA.String(), ghesHost)
	seedGitHubApp(t, rig, orgA.String(), alice.String(), "Iv1.connect_client", "secret")

	startResp := rig.requestWithSid("GET",
		"/api/orgs/"+orgA.String()+"/github/connect/start?return_to=/orgs/"+orgA.String()+"/triage", sidA)
	if startResp.StatusCode != http.StatusFound {
		t.Fatalf("start status=%d, want 302", startResp.StatusCode)
	}
	loc := startResp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect %q: %v", loc, err)
	}
	if u.Scheme+"://"+u.Host != ghesHost {
		t.Errorf("redirect host = %q, want the GHES host %q", u.Scheme+"://"+u.Host, ghesHost)
	}
	if u.Path != "/login/oauth/authorize" {
		t.Errorf("redirect path = %q, want /login/oauth/authorize", u.Path)
	}
	if got := u.Query().Get("client_id"); got != "Iv1.connect_client" {
		t.Errorf("client_id = %q, want the App's", got)
	}
	if u.Query().Get("state") == "" {
		t.Error("authorize URL missing CSRF state")
	}
	if got := u.Query().Get("redirect_uri"); !strings.HasSuffix(got, "/github/connect/callback") {
		t.Errorf("redirect_uri = %q, want the connect callback", got)
	}

	var stateCookieSet bool
	for _, c := range startResp.Cookies() {
		if c.Name == connectStateCookieName && c.Value != "" {
			stateCookieSet = true
		}
	}
	if !stateCookieSet {
		t.Errorf("start did not set the %s cookie", connectStateCookieName)
	}
}

// TestGitHubConnect_StartNoApp_RedirectsToGate verifies that an org without a
// registered App can't Connect (no client_id to reuse) and is bounced to the
// gate page with no_app — distinguishable copy, not a dead end.
func TestGitHubConnect_StartNoApp_RedirectsToGate(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "no-app-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	startResp := rig.requestWithSid("GET", "/api/orgs/"+orgA.String()+"/github/connect/start", sidA)
	if startResp.StatusCode != http.StatusFound {
		t.Fatalf("start status=%d, want 302", startResp.StatusCode)
	}
	loc := startResp.Header.Get("Location")
	if !strings.Contains(loc, "/connect-github") || !strings.Contains(loc, "error=no_app") {
		t.Errorf("redirect = %q, want the gate page with error=no_app", loc)
	}
}

// TestGitHubConnect_CallbackBindsIdentity drives the full callback against a
// stubbed GitHub host: code → ghu token → GET /user → login. Asserts the
// user_github_identities row lands with source='connect_oauth' against the
// GHES host, and the browser is redirected to the original return_to.
func TestGitHubConnect_CallbackBindsIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "bind-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	var gotTokenReq, gotUserReq bool
	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			gotTokenReq = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"ghu_test_token","token_type":"bearer"}`)
		case "/api/v3/user":
			gotUserReq = true
			if auth := r.Header.Get("Authorization"); auth != "Bearer ghu_test_token" {
				t.Errorf("whoami Authorization = %q, want the ghu_ token", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"login":"corp-octocat"}`)
		case "/api/v3/user/emails":
			writeGitHubPrimaryEmail(w, "corp-octocat@example.com")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghStub.Close)

	seedOrgGitHubHost(t, rig, orgA.String(), ghStub.URL)
	seedGitHubApp(t, rig, orgA.String(), alice.String(), "Iv1.bind_client", "bind_secret")

	csrf := "csrf-nonce-bind"
	st := connectState{
		OrgID:     orgA.String(),
		UserID:    alice.String(),
		CSRF:      csrf,
		ReturnTo:  "/orgs/" + orgA.String() + "/triage",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(rig.srv.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("sign state: %v", err)
	}

	req := httptest.NewRequest("GET",
		"/api/orgs/"+orgA.String()+"/github/connect/callback?code=valid_code&state="+csrf, nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
	req.AddCookie(&http.Cookie{Name: connectStateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s, want 302", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != st.ReturnTo {
		t.Errorf("redirect = %q, want return_to %q", loc, st.ReturnTo)
	}
	if !gotTokenReq || !gotUserReq {
		t.Errorf("expected both token-exchange and whoami calls (token=%v user=%v)", gotTokenReq, gotUserReq)
	}

	// driveCallback (the GitHub login) already wrote a login_claim row on
	// github.com; Connect adds a SEPARATE binding for the org's (stub) host.
	// Scope the readback to the host Connect wrote.
	var login, email, source, host string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT login, email, source, github_base_url FROM user_github_identities
		 WHERE user_id = $1 AND github_base_url = $2
	`, alice.String(), ghStub.URL).Scan(&login, &email, &source, &host); err != nil {
		t.Fatalf("read user_github_identities: %v", err)
	}
	if login != "corp-octocat" {
		t.Errorf("login = %q, want corp-octocat", login)
	}
	if email != "corp-octocat@example.com" {
		t.Errorf("email = %q, want corp-octocat@example.com", email)
	}
	if source != "connect_oauth" {
		t.Errorf("source = %q, want connect_oauth", source)
	}
	if host != ghStub.URL {
		t.Errorf("github_base_url = %q, want the stub host %q", host, ghStub.URL)
	}

	// The login_claim binding from driveCallback (github.com) must coexist —
	// Connect adds a per-host row, it doesn't clobber the login binding.
	var claimRows int
	if err := rig.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM user_github_identities
		 WHERE user_id = $1 AND github_base_url = 'https://github.com' AND source = 'login_claim'
	`, alice.String()).Scan(&claimRows); err != nil {
		t.Fatalf("count login_claim rows: %v", err)
	}
	if claimRows != 1 {
		t.Errorf("login_claim github.com row count = %d, want 1 (Connect must not disturb it)", claimRows)
	}
}

func TestGitHubLoginClaimCapturesVerifiedEmail(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)
	userID := rig.seedUser()
	claims := validClaimsFor(userID)
	claims["email_verified"] = true

	resp, _ := rig.driveCallbackClaims(claims)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}

	var email sql.NullString
	if err := rig.h.AdminDB.QueryRow(`
		SELECT email FROM user_github_identities
		 WHERE user_id = $1 AND github_base_url = 'https://github.com' AND source = 'login_claim'
	`, userID.String()).Scan(&email); err != nil {
		t.Fatalf("read login-claim email: %v", err)
	}
	if want := userID.String() + "@test"; email.String != want {
		t.Errorf("email = %q, want %q", email.String, want)
	}
}

// TestGitHubConnect_CallbackHostUnreachable proves the infra state is
// distinguished: when the host can't be reached, the callback redirects to
// the gate with error=host_unreachable (not connect_failed), and writes no
// identity row.
func TestGitHubConnect_CallbackHostUnreachable(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "unreachable-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	seedOrgGitHubHost(t, rig, orgA.String(), deadURL)
	seedGitHubApp(t, rig, orgA.String(), alice.String(), "Iv1.dead_client", "dead_secret")

	csrf := "csrf-dead"
	st := connectState{
		OrgID: orgA.String(), UserID: alice.String(), CSRF: csrf,
		ReturnTo: "/orgs/" + orgA.String(), ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, _ := st.sign(rig.srv.deployCfg.hmacKey)

	req := httptest.NewRequest("GET",
		"/api/orgs/"+orgA.String()+"/github/connect/callback?code=c&state="+csrf, nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
	req.AddCookie(&http.Cookie{Name: connectStateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=host_unreachable") {
		t.Errorf("redirect = %q, want error=host_unreachable", loc)
	}

	// The login (driveCallback) leaves a login_claim row on github.com; the
	// assertion is specifically that Connect wrote nothing for the org's
	// (dead) host.
	var n int
	if err := rig.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2`,
		alice.String(), deadURL).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("identity rows for the unreachable host = %d, want 0", n)
	}
}

// TestGitHubConnect_CallbackCSRFMismatch verifies a state/cookie mismatch is
// rejected to the gate (no binding) — the OAuth CSRF defense.
func TestGitHubConnect_CallbackCSRFMismatch(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "csrf-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)

	seedOrgGitHubHost(t, rig, orgA.String(), "https://github.com")
	seedGitHubApp(t, rig, orgA.String(), alice.String(), "Iv1.csrf_client", "secret")

	st := connectState{
		OrgID: orgA.String(), UserID: alice.String(), CSRF: "real-nonce",
		ReturnTo: "/", ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, _ := st.sign(rig.srv.deployCfg.hmacKey)

	// Query carries a different state than the signed cookie.
	req := httptest.NewRequest("GET",
		"/api/orgs/"+orgA.String()+"/github/connect/callback?code=c&state=ATTACKER-NONCE", nil)
	req.AddCookie(&http.Cookie{Name: rig.srv.sidCookieName(), Value: sidA})
	req.AddCookie(&http.Cookie{Name: connectStateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	rig.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=state") {
		t.Errorf("redirect = %q, want error=state", loc)
	}
	// login_claim from the login flow may exist; assert Connect itself bound
	// nothing (no connect_oauth row).
	var n int
	_ = rig.h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM user_github_identities WHERE user_id = $1 AND source = 'connect_oauth'`,
		alice.String()).Scan(&n)
	if n != 0 {
		t.Errorf("connect_oauth rows = %d, want 0 on CSRF mismatch", n)
	}
}

// TestGitHubIdentityStatus exercises the onboarding gate's status read: a
// bound user reports connected=true with the login; an unbound user reports
// connected=false. connect_available reflects whether an App is registered.
func TestGitHubIdentityStatus(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "status-org")
	resp, _ := rig.driveCallback(alice)
	sidA := rig.sidFromResp(resp)
	// Use a GHES host so the user is genuinely unconnected for this org:
	// driveCallback's login_claim binds github.com, which must NOT satisfy the
	// gate for a different host — the core GHES-onboarding scenario.
	const ghesHost = "https://ghe.example.com"
	seedOrgGitHubHost(t, rig, orgA.String(), ghesHost)

	get := func() githubIdentityStatusResponse {
		r := rig.requestWithSid("GET", "/api/orgs/"+orgA.String()+"/github/identity", sidA)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status endpoint status=%d", r.StatusCode)
		}
		var out githubIdentityStatusResponse
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if out := get(); out.Connected {
		t.Errorf("fresh user reported connected=true: %+v", out)
	}

	// Bind via the store under the resolved origin (what the gate reads).
	if err := rig.srv.tx.WithTx(context.Background(), orgA.String(), alice.String(), func(tx db.TxStores) error {
		return tx.Users.UpsertGitHubIdentity(context.Background(), alice.String(), ghesHost, "octocat", "", "", "connect_oauth")
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	out := get()
	if !out.Connected || out.Login != "octocat" {
		t.Errorf("after bind, got %+v, want connected=true login=octocat", out)
	}
	if out.Host != ghesHost {
		t.Errorf("host = %q, want %q", out.Host, ghesHost)
	}
	if out.ConnectAvailable {
		t.Errorf("connect_available=true with no App registered: %+v", out)
	}
}

// TestGitHubConnect_IdentityPersistsAcrossLoginProvider closes a
// regression by construction: a connect_oauth binding must survive a
// subsequent login under a non-GitHub provider (no user_name claim), where
// the login-claim writer deliberately writes nothing.
func TestGitHubConnect_IdentityPersistsAcrossLoginProvider(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)

	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "persist-org")
	seedOrgGitHubHost(t, rig, orgA.String(), "https://github.com")

	if err := rig.srv.tx.WithTx(context.Background(), orgA.String(), alice.String(), func(tx db.TxStores) error {
		return tx.Users.UpsertGitHubIdentity(context.Background(), alice.String(), "https://github.com", "corp-login", "", "", "connect_oauth")
	}); err != nil {
		t.Fatalf("seed connect identity: %v", err)
	}

	// A DISTINCT Entra SAML login identity for the same human, linked to alice's
	// principal (her github login identity already occupies auth_user_id=alice).
	samlID := uuid.New()
	rig.h.SeedAuthUser(t, samlID.String(), "alice@corp.example")
	if _, err := rig.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'saml', 'alice@corp.example', true)`, samlID, alice); err != nil {
		t.Fatalf("seed saml identity: %v", err)
	}

	// Re-login under that non-GitHub provider (Entra SAML): isSSO=true resolves to
	// alice's principal but must never touch the github identity row —
	// preserving the connect_oauth binding captured above.
	claims := &verify.Claims{
		Subject:       samlID.String(),
		Email:         "alice@corp.example",
		EmailVerified: true,                                         // SAML assertion emails are verified by definition
		UserMetadata:  map[string]any{"full_name": "Alice Example"}, // no user_name
	}
	got, err := resolveOrCreatePrincipal(context.Background(), rig.h.AdminDB, samlID, claims, true)
	if err != nil {
		t.Fatalf("resolveOrCreatePrincipal: %v", err)
	}
	if got != alice {
		t.Fatalf("SAML re-login resolved to principal %v, want alice %v", got, alice)
	}

	var login, source string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT login, source FROM user_github_identities WHERE user_id = $1 AND github_base_url = 'https://github.com'
	`, alice.String()).Scan(&login, &source); err != nil {
		t.Fatalf("read identity after re-login: %v", err)
	}
	if login != "corp-login" || source != "connect_oauth" {
		t.Errorf("after non-GitHub re-login got (login=%q, source=%q), want the connect_oauth binding intact", login, source)
	}
}

// ---- PAT_2 capture-and-discard endpoint (local-mode SQLite) ----

// seedLocalOrgGitHubHost sets the local sentinel org's github_base_url via the
// store (the value the PAT-capture handler resolves + keys identity under).
func seedLocalOrgGitHubHost(t *testing.T, s *Server, host string) {
	t.Helper()
	ctx := context.Background()
	if err := s.tx.WithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			return err
		}
		set.GitHubBaseURL = host
		_, err = tx.Orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, set)
		return err
	}); err != nil {
		t.Fatalf("seed org github host: %v", err)
	}
}

// TestGitHubIdentityPAT_BindsIdentityAndDiscardsToken drives the capture-and-
// discard path against a stubbed GHES host: the supplied PAT validates (GET
// /user), the login is bound with source='pat' keyed on the org host, and the
// token is NOT written into the org credential store — PAT_2 leaves no stored
// credential, byte-for-byte the Connect end state.
func TestGitHubIdentityPAT_BindsIdentityAndDiscardsToken(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — the sandbox has no dbus backend
	s := newTestServer(t)
	ctx := context.Background()

	var gotAuth string
	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/emails" {
			writeGitHubPrimaryEmail(w, "octo-pat@example.com")
			return
		}
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"octo-pat"}`)
	}))
	t.Cleanup(ghStub.Close)
	seedLocalOrgGitHubHost(t, s, ghStub.URL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "ghp_secret_token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var out githubIdentityCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Login != "octo-pat" {
		t.Errorf("login = %q, want octo-pat", out.Login)
	}
	if out.Host != ghStub.URL {
		t.Errorf("host = %q, want %q", out.Host, ghStub.URL)
	}
	if gotAuth != "Bearer ghp_secret_token" {
		t.Errorf("whoami Authorization = %q, want the supplied PAT", gotAuth)
	}

	login, err := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, ghStub.URL)
	if err != nil {
		t.Fatalf("GetGitHubLogin: %v", err)
	}
	if login != "octo-pat" {
		t.Errorf("stored login = %q, want octo-pat", login)
	}
	var source string
	if err := s.db.QueryRowContext(ctx,
		`SELECT source FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
		runmode.LocalDefaultUserID, ghStub.URL).Scan(&source); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if source != "pat" {
		t.Errorf("source = %q, want pat", source)
	}

	// The token is discarded — never written into the org credential store (the
	// only secret store in play), proving PAT_2 retains no usable credential.
	creds, _ := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.GitHubPAT != "" {
		t.Errorf("org GitHub PAT = %q, want empty (the user PAT must not be stored)", creds.GitHubPAT)
	}
}

// TestGitHubIdentityPAT_BadToken_Returns422 pins the "your action" failure
// shape: a token the host rejects is a 422 (not the infra 502), and no identity
// row is written.
func TestGitHubIdentityPAT_BadToken_Returns422(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(ghStub.Close)
	seedLocalOrgGitHubHost(t, s, ghStub.URL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "ghp_bad"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, ghStub.URL); login != "" {
		t.Errorf("identity row written on a bad token: login=%q", login)
	}
}

// TestGitHubIdentityPAT_EmptyToken_Returns400 pins that an empty PAT is
// rejected before any host round-trip.
func TestGitHubIdentityPAT_EmptyToken_Returns400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit() // in-memory keychain — consistent with the sibling PAT tests
	s := newTestServer(t)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

// TestGitHubIdentityPAT_HostUnreachable_Returns502 pins the infra failure shape:
// when TF can't reach the host, the response is a 502 (distinct from a bad
// token) and no identity row is written.
func TestGitHubIdentityPAT_HostUnreachable_Returns502(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	ctx := context.Background()

	// Stand a stub up to claim a port, then close it so dials are refused.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	seedLocalOrgGitHubHost(t, s, deadURL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "ghp_whatever"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	if login, _ := s.users.GetGitHubLogin(ctx, runmode.LocalDefaultUserID, deadURL); login != "" {
		t.Errorf("identity row written despite unreachable host: %q", login)
	}
}

// TestGitHubIdentityPAT_CapturesNumericUserID pins the second of the two
// writers of user_github_identities.github_user_id (the Connect OAuth callback
// is the other): the whoami that proves the token also reports which account it
// is, and both halves of that answer are persisted. The login is what the rest
// of the product matches on today; the id is what stays true if the user
// renames.
func TestGitHubIdentityPAT_CapturesNumericUserID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := context.Background()

	ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/user/emails" {
			writeGitHubPrimaryEmail(w, "octo-pat@example.com")
			return
		}
		if r.URL.Path != "/api/v3/user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":583231,"login":"octo-pat"}`)
	}))
	t.Cleanup(ghStub.Close)
	seedLocalOrgGitHubHost(t, s, ghStub.URL)

	rec := doJSON(t, s, "POST",
		"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
		map[string]any{"pat": "ghp_secret_token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var githubUserID, email sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT github_user_id, email FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
		runmode.LocalDefaultUserID, ghStub.URL).Scan(&githubUserID, &email); err != nil {
		t.Fatalf("read GitHub identity: %v", err)
	}
	if githubUserID.String != "583231" {
		t.Errorf("github_user_id = %q, want %q", githubUserID.String, "583231")
	}
	if email.String != "octo-pat@example.com" {
		t.Errorf("email = %q, want %q", email.String, "octo-pat@example.com")
	}
}
