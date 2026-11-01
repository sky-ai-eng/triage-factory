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

// TestGitHubConnect_StateToken_RoundTrip: the HMAC-signed connect state, the
// OAuth flow's CSRF and session binding, round-trips, and expired, wrongly keyed
// and malformed tokens are rejected.
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

// TestResolveGitHubHost: the identity key keeps a base URL's path, so it agrees
// with every other reader of the org's GitHub URL; an empty base is no host and
// malformed ones are rejected.
func TestResolveGitHubHost(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://github.com", "https://github.com", true},
		{"", "", false}, // no GitHub URL, no host
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

// ---- multi-mode (Postgres-backed) ----

// seedGitHubApp leaves what a completed registration does: the org's App row,
// its class, and its client secret in the vault, which Connect reuses.
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

// TestGitHubConnect_StartRedirectsToHost: Connect for an org's own App sends
// the browser to the org's host (GHES here), not github.com, with the App's
// client_id and a CSRF state, and sets the state cookie.
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

// TestGitHubConnect_StartNoApp_RedirectsToGate: an org with no App has no
// client to Connect through and is sent to the gate page with error=no_app.
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

// TestGitHubConnect_CallbackBindsIdentity: the own-App callback exchanges the
// code, reads the account, writes a connect_oauth identity on the org's host
// beside the login's github.com row, and returns to return_to.
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

// TestGitHubLoginClaimCapturesVerifiedEmail: a GitHub sign-in with a verified
// email records it on the login_claim identity.
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

// TestGitHubConnect_CallbackHostUnreachable: an unreachable host sends the
// browser to the gate with error=host_unreachable, not connect_failed, and
// writes no identity.
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

// TestGitHubIdentityStatus: a GHES org's member is not connected by their
// github.com login, is connected once bound on the org's host, and is offered
// no Connect with no App registered.
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

// TestGitHubConnect_LoginMirrorYieldsToUserBinding: the sign-in mirror refreshes
// a github.com binding it wrote, but leaves one the user made by Connect or a
// PAT, or the next login would undo it.
func TestGitHubConnect_LoginMirrorYieldsToUserBinding(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	rig := newAuthRig(t)
	alice := rig.seedUser()
	orgA, _ := rig.seedOrg(alice, "mirror-org")
	seedOrgGitHubHost(t, rig, orgA.String(), "https://github.com")

	readBinding := func() (login, source string) {
		t.Helper()
		if err := rig.h.AdminDB.QueryRow(`
			SELECT login, source FROM user_github_identities
			 WHERE user_id = $1 AND github_base_url = 'https://github.com'
		`, alice.String()).Scan(&login, &source); err != nil {
			t.Fatalf("read binding: %v", err)
		}
		return login, source
	}
	signInAs := func(handle string) {
		t.Helper()
		claims := validClaimsFor(alice)
		claims["user_metadata"].(map[string]any)["user_name"] = handle
		if resp, _ := rig.driveCallbackClaims(claims); resp.StatusCode != http.StatusFound {
			t.Fatalf("callback status=%d, want 302", resp.StatusCode)
		}
	}

	// A mirror-written row follows the login: the handle it mirrors changes,
	// the row changes.
	signInAs("alice-personal")
	if login, source := readBinding(); login != "alice-personal" || source != "login_claim" {
		t.Fatalf("after first login: %s/%s, want alice-personal/login_claim", login, source)
	}
	signInAs("alice-renamed")
	if login, source := readBinding(); login != "alice-renamed" || source != "login_claim" {
		t.Errorf("after handle change: %s/%s, want the mirror refreshed to alice-renamed/login_claim", login, source)
	}

	// The user then binds a different account for this host. That row is
	// theirs, and the next sign-in leaves it exactly as they set it.
	if err := rig.srv.tx.WithTx(context.Background(), orgA.String(), alice.String(), func(tx db.TxStores) error {
		return tx.Users.UpsertGitHubIdentity(context.Background(), alice.String(), "https://github.com", "alice-corp", "", "", "connect_oauth")
	}); err != nil {
		t.Fatalf("bind by connect: %v", err)
	}
	signInAs("alice-renamed")
	if login, source := readBinding(); login != "alice-corp" || source != "connect_oauth" {
		t.Errorf("after login over a connect_oauth binding: %s/%s, want alice-corp/connect_oauth untouched", login, source)
	}
	if err := rig.srv.tx.WithTx(context.Background(), orgA.String(), alice.String(), func(tx db.TxStores) error {
		return tx.Users.UpsertGitHubIdentity(context.Background(), alice.String(), "https://github.com", "alice-pat", "", "", "pat")
	}); err != nil {
		t.Fatalf("bind by pat: %v", err)
	}
	signInAs("alice-renamed")
	if login, source := readBinding(); login != "alice-pat" || source != "pat" {
		t.Errorf("after login over a pat binding: %s/%s, want alice-pat/pat untouched", login, source)
	}
}

// TestGitHubConnect_IdentityPersistsAcrossLoginProvider: a connect_oauth binding
// survives a later sign-in through a non-GitHub provider, whose claims carry no
// user_name for the mirror to write.
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

	// Re-login under that non-GitHub provider (Entra SAML): a provider id
	// resolves to alice's principal but must never touch the github identity
	// row — preserving the connect_oauth binding captured above.
	claims := &verify.Claims{
		Subject:       samlID.String(),
		Email:         "alice@corp.example",
		EmailVerified: true,
		UserMetadata:  map[string]any{"full_name": "Alice Example"}, // no user_name
	}
	const providerID = "11111111-2222-3333-4444-555555555555"
	// Trusted, as it would be on a domain verified for the connection's org.
	got, err := resolveOrCreatePrincipal(context.Background(), rig.h.AdminDB, samlID, claims, true, providerID)
	if err != nil {
		t.Fatalf("resolveOrCreatePrincipal: %v", err)
	}
	if got != alice {
		t.Fatalf("SAML re-login resolved to principal %v, want alice %v", got, alice)
	}

	// The saml row was seeded without a provider id; the login refresh stamps
	// it, so the identities read can name the IdP behind it.
	var stamped string
	if err := rig.h.AdminDB.QueryRow(`
		SELECT COALESCE(sso_provider_id, '') FROM user_identities WHERE auth_user_id = $1
	`, samlID).Scan(&stamped); err != nil {
		t.Fatalf("read saml identity after re-login: %v", err)
	}
	if stamped != providerID {
		t.Errorf("sso_provider_id after re-login = %q, want %q", stamped, providerID)
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

// ---- identity PAT capture (local mode) ----

// seedLocalOrgGitHubHost sets the local org's github_base_url, the host the
// PAT capture keys the identity under, keeping its other settings.
func seedLocalOrgGitHubHost(t *testing.T, s *Server, host string) {
	t.Helper()
	ctx := context.Background()
	if err := s.tx.WithTx(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			return err
		}
		set.GitHubBaseURL = host
		_, err = tx.Orgs.UpdateSettingsVersioned(ctx, runmode.LocalDefaultOrgID, set, set.Version)
		return err
	}); err != nil {
		t.Fatalf("seed org github host: %v", err)
	}
}

// TestGitHubIdentityPAT_BindsIdentityAndDiscardsToken: the pasted PAT proves
// the account, whose login, numeric id and email are bound with source='pat' on
// the org's host, and the token itself is stored nowhere.
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
	var githubUserID, email sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT source, github_user_id, email FROM user_github_identities WHERE user_id = ? AND github_base_url = ?`,
		runmode.LocalDefaultUserID, ghStub.URL).Scan(&source, &githubUserID, &email); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if source != "pat" {
		t.Errorf("source = %q, want pat", source)
	}
	if githubUserID.String != "583231" {
		t.Errorf("github_user_id = %q, want %q", githubUserID.String, "583231")
	}
	if email.String != "octo-pat@example.com" {
		t.Errorf("email = %q, want %q", email.String, "octo-pat@example.com")
	}

	// The org credential store is the only secret store in play, and the user
	// PAT is not in it.
	creds, _ := integrations.Load(ctx, s.secrets, runmode.LocalDefaultOrgID)
	if creds.GitHubPAT != "" {
		t.Errorf("org GitHub PAT = %q, want empty (the user PAT must not be stored)", creds.GitHubPAT)
	}
}

// TestGitHubIdentityPAT_Refusals: an empty token is a 400 before any host call,
// a token the host rejects is the caller's 422, an unreachable host is a 502,
// and none of them writes an identity.
func TestGitHubIdentityPAT_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   func(t *testing.T) string
		pat    string
		status int
	}{
		{"empty token", func(t *testing.T) string { return "" }, "   ", http.StatusBadRequest},
		{"token rejected", func(t *testing.T) string {
			ghStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			}))
			t.Cleanup(ghStub.Close)
			return ghStub.URL
		}, "ghp_bad", http.StatusUnprocessableEntity},
		{"host unreachable", func(t *testing.T) string {
			// Claim a port, then close it so dials are refused.
			dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			dead.Close()
			return dead.URL
		}, "ghp_whatever", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			keyring.MockInit()
			s := newTestServer(t)
			host := tc.host(t)
			if host != "" {
				seedLocalOrgGitHubHost(t, s, host)
			}

			rec := doJSON(t, s, "POST",
				"/api/orgs/"+runmode.LocalDefaultOrgID+"/github/identity/pat",
				map[string]any{"pat": tc.pat})
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if host == "" {
				return
			}
			if login, _ := s.users.GetGitHubLogin(context.Background(), runmode.LocalDefaultUserID, host); login != "" {
				t.Errorf("a refused capture wrote an identity: login=%q", login)
			}
		})
	}
}
