package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/auth/sessions"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ---------- JWKS test rig (mirrors internal/auth/verify/verifier_test.go) ----------

const (
	testIssuer   = "https://tf.test/auth/v1"
	testAudience = "authenticated"
)

type testKey struct {
	kid  string
	priv *rsa.PrivateKey
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	return testKey{kid: uuid.NewString(), priv: priv}
}

func (k testKey) publicJWK() map[string]any {
	pub := &k.priv.PublicKey
	return map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": k.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (k testKey) mintJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = k.kid
	signed, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

type jwksMux struct {
	mu   sync.Mutex
	keys []testKey
}

func (m *jwksMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	jwks := make([]map[string]any, 0, len(m.keys))
	for _, k := range m.keys {
		jwks = append(jwks, k.publicJWK())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
}

// validClaimsFor returns a JWT claim map for the given user id.
func validClaimsFor(userID uuid.UUID) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   userID.String(),
		"email": userID.String() + "@test",
		"iss":   testIssuer,
		"aud":   testAudience,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "github",
			"providers": []any{"github"},
		},
		"user_metadata": map[string]any{
			"user_name":  "test-user-" + userID.String()[:8],
			"avatar_url": "https://avatar.example/" + userID.String()[:8],
			"full_name":  "Test User",
		},
	}
}

// ---------- test rig: server + JWKS + cleanup ----------

type authRig struct {
	t       *testing.T
	h       *pgtest.Harness
	srv     *Server
	jwks    *httptest.Server
	jwksMux *jwksMux
	signKey testKey

	// gotrueLogoutCalls records the access tokens passed to the
	// gotrueLogout closure during the test. Lets logout tests assert
	// "upstream was invoked with the right JWT" without standing up a
	// real gotrue stub.
	gotrueLogoutCalls []string
}

// newAuthRig boots the pg testcontainer, JWKS test server, verifier,
// session store, and Server. The test process owns the JWKS keypair so
// it can mint tokens that the live Verifier accepts.
//
// Multi mode, because everything the rig stands up is multi-only machinery —
// GoTrue-signed JWTs, org memberships, RLS. Left at the default, the mode would
// say local while the database is Postgres, and the authorization gates would
// take their N=1 short-circuit against real multi-tenant rows: a cross-org test
// then passes by not asking the question.
func newAuthRig(t *testing.T) *authRig {
	t.Helper()

	runmode.SetForTest(t, runmode.ModeMulti)
	h := pgtest.Shared(t)
	h.Reset(t)

	signKey := newTestKey(t)
	mux := &jwksMux{keys: []testKey{signKey}}
	jwks := httptest.NewServer(mux)
	t.Cleanup(jwks.Close)

	v, err := verify.NewVerifier(context.Background(), jwks.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Two independent random keys: one for session AES-GCM, one for
	// the OAuth state cookie HMAC. Production loads these from
	// TF_SESSION_ENCRYPTION_KEY + TF_COOKIE_SECRET respectively.
	var encKey, cookieSecret [32]byte
	if _, err := rand.Read(encKey[:]); err != nil {
		t.Fatalf("seed enc key: %v", err)
	}
	if _, err := rand.Read(cookieSecret[:]); err != nil {
		t.Fatalf("seed cookie secret: %v", err)
	}
	store := sessions.NewStore(h.AdminDB, sessions.Key(encKey))

	// Wire real pgstore-backed stores. Auth-only tests don't exercise
	// these (handlers like /api/me touch sessions, not pg stores), but
	// the cross-org HTTP tests in cross_org_http_test.go need real
	// stores to prove the handler→WithTx→RLS chain actually filters
	// by session orgID end-to-end. Use the production pool split:
	// admin for system reads, app (tf_app) for request-equivalent
	// paths under RLS.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	s := New(h.AdminDB, stores)

	// Per-test context bound to t.Cleanup so the reaper goroutine
	// spawned inside SetAuthDeps exits with the test rather than
	// leaking across test runs.
	rigCtx, rigCancel := context.WithCancel(context.Background())
	t.Cleanup(rigCancel)
	if err := s.SetAuthDeps(rigCtx, v, store, apitokens.NewStore(h.AdminDB), "http://gotrue.unused", "http://tf.test", cookieSecret); err != nil {
		t.Fatalf("SetAuthDeps: %v", err)
	}

	rig := &authRig{
		t:       t,
		h:       h,
		srv:     s,
		jwks:    jwks,
		jwksMux: mux,
		signKey: signKey,
	}

	// Default gotrueLogout stub: record the access token and succeed.
	// Tests that want to assert "upstream logout was called" inspect
	// rig.gotrueLogoutCalls; tests that want to simulate a failure
	// override authDeps.gotrueLogout themselves.
	s.authDeps.gotrueLogout = func(ctx context.Context, accessToken string) error {
		rig.gotrueLogoutCalls = append(rig.gotrueLogoutCalls, accessToken)
		return nil
	}

	return rig
}

// seedUser inserts auth.users + public.users for a fresh UUID and
// returns it. Mirrors pgtest.seedUser (package-private) — when a third
// caller materializes, this lifts to pgtest as an exported helper.
func (r *authRig) seedUser() uuid.UUID {
	r.t.Helper()
	var idStr string
	if err := r.h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		r.t.Fatalf("gen uuid: %v", err)
	}
	r.h.SeedAuthUser(r.t, idStr, idStr+"@test")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, idStr, "tester"); err != nil {
		r.t.Fatalf("seed public.users: %v", err)
	}
	// Link the GoTrue auth identity to this principal so a simulated login
	// (driveCallback) resolves back to this user instead of minting a fresh
	// principal. In tests the auth.users id and principal id are the same value.
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $1, 'github', $2, true)`, idStr, idStr+"@test"); err != nil {
		r.t.Fatalf("seed user identity: %v", err)
	}
	return uuid.MustParse(idStr)
}

// seedOrg inserts an org owned by ownerID and returns its UUID + the
// id of its default team. Org_memberships gets a 'owner' row for the
// owner; team memberships get 'admin'.
func (r *authRig) seedOrg(ownerID uuid.UUID, slug string) (orgID, teamID uuid.UUID) {
	r.t.Helper()
	var oID, tID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1, $1, $2) RETURNING id::text
	`, slug, ownerID).Scan(&oID); err != nil {
		r.t.Fatalf("insert org: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'default') RETURNING id::text
	`, oID).Scan(&tID); err != nil {
		r.t.Fatalf("insert team: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
		ownerID, oID); err != nil {
		r.t.Fatalf("insert org_membership: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
		ownerID, tID); err != nil {
		r.t.Fatalf("insert membership: %v", err)
	}
	// On github.com, as pgtest.SeedOrg's orgs are: an org reaches these routes
	// past a setup gate that requires a GitHub URL.
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_event_sources (org_id, kind, base_url) VALUES ($1, 'github', 'https://github.com')`,
		oID); err != nil {
		r.t.Fatalf("insert github url: %v", err)
	}
	return uuid.MustParse(oID), uuid.MustParse(tID)
}

// signStateCookie returns a state cookie value usable in callback
// requests. Mirrors handleOAuthStart's signing path without forcing
// the test to drive the full redirect.
func (r *authRig) signStateCookie(returnTo, csrf, codeVerifier string) string {
	r.t.Helper()
	state := stateClaims{
		ReturnTo: returnTo, CSRF: csrf, CodeVerifier: codeVerifier,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	dc := r.srv.deployCfg
	signed, err := state.sign(dc.hmacKey)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}
	return signed
}

// driveCallback completes the PKCE handshake server-side: stubs the
// gotrue exchange closure to hand back a JWT minted by the test rig
// for userID, then drives the /api/auth/callback handler with a code
// query param. Returns the resulting response (including the sid
// Set-Cookie).
//
// gotrueLogoutCalls captures any subsequent upstream-logout invocation
// for assertion in logout tests.
func (r *authRig) driveCallback(userID uuid.UUID) (resp *http.Response, accessToken string) {
	r.t.Helper()
	return r.driveCallbackClaims(validClaimsFor(userID))
}

// driveCallbackClaims drives the OAuth callback with arbitrary JWT claims, so a
// test can vary email / email_verified and exercise principal resolution +
// verified-email linking through the full verifier path.
func (r *authRig) driveCallbackClaims(claims jwt.MapClaims) (resp *http.Response, accessToken string) {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, claims)
	refresh := "refresh-" + uuid.NewString()

	// Stub the exchange closure to return our minted tokens regardless
	// of the auth_code value. The TF handler still verifies the JWT
	// via the live Verifier, so an unsigned/wrong-issuer token would
	// still 4xx — exactly what we want.
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, refresh, time.Now().Add(time.Hour).Unix(), nil
	}

	csrf := "test-csrf-" + uuid.NewString()
	stateVal := r.signStateCookie("/dashboard", csrf, "test-pkce-verifier")

	q := url.Values{}
	q.Set("state", csrf)
	q.Set("code", "fake-auth-code-"+uuid.NewString())
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateVal})

	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result(), token
}

// driveSSOCallbackClaims is driveCallbackClaims for an SSO sign-in: TF's
// signed state carries providerID, which is what makes the callback treat the
// login as SSO. This package registers no login extension, so the no-op's
// TrustsSSOEmail answers no and every address driven here is recorded
// untrusted — ee/sso's answer for a domain not verified for the connection.
func (r *authRig) driveSSOCallbackClaims(claims jwt.MapClaims, providerID string) *http.Response {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, claims)
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, "refresh-" + uuid.NewString(), time.Now().Add(time.Hour).Unix(), nil
	}
	csrf := "test-csrf-" + uuid.NewString()
	state := stateClaims{
		ReturnTo: "/dashboard", CSRF: csrf, CodeVerifier: "test-pkce-verifier", ProviderID: providerID,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(r.srv.deployCfg.hmacKey)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}
	q := url.Values{}
	q.Set("state", csrf)
	q.Set("code", "fake-auth-code-"+uuid.NewString())
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// sidFromResp pulls the sid cookie from a callback response, fatal on miss.
func (r *authRig) sidFromResp(resp *http.Response) string {
	r.t.Helper()
	want := r.srv.sidCookieName()
	for _, c := range resp.Cookies() {
		if c.Name == want {
			return c.Value
		}
	}
	r.t.Fatalf("response had no %s cookie (status=%d, set-cookie=%v)",
		want, resp.StatusCode, resp.Header["Set-Cookie"])
	return ""
}

// requestWithSid fires a request to s.mux with the sid cookie set
// AND a same-origin Origin header (so the CSRF Origin check on
// mutating endpoints passes — GETs ignore Origin anyway).
func (r *authRig) requestWithSid(method, path, sid string) *http.Response {
	r.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	// Set Origin to the configured publicURL so withCSRFOriginCheck
	// treats the request as same-origin. httptest.NewRequest sets no
	// Origin by default, which the middleware also allows (no Origin
	// = not a browser cross-site request), but explicit is clearer.
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// ---------- tests: session flow ----------

// TestAuthFlow_Me_LinearIdentityFromActiveOrgWorkspace: multi-mode /api/me
// reports the user's Linear binding in the active org's workspace only: none
// while the org has no workspace, never one from another workspace, and none
// once the user leaves the org the session still points at.
func TestAuthFlow_Me_LinearIdentityFromActiveOrgWorkspace(t *testing.T) {
	r := newAuthRig(t)

	userID := r.seedUser()
	orgID, teamID := r.seedOrg(userID, "linear-ws-org")
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO user_linear_identities (user_id, workspace_id, linear_user_id, display_name, source, verified_at)
		 VALUES ($1, 'ws-elsewhere', 'lin-elsewhere', 'Elsewhere', 'api_key', now() + interval '1 hour'),
		        ($1, 'ws-org', 'lin-me', 'Me In Linear', 'api_key', now())`, userID)

	resp, _ := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	readMe := func() map[string]any {
		t.Helper()
		meResp := r.requestWithSid("GET", "/api/me", sid)
		defer meResp.Body.Close()
		if meResp.StatusCode != http.StatusOK {
			t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
		}
		var me map[string]any
		if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
			t.Fatalf("decode /api/me: %v", err)
		}
		return me
	}

	// The org has no Linear workspace yet, so neither binding is reported: a
	// Linear user id names nobody outside the workspace that issued it.
	me := readMe()
	if me["active_org_id"] == nil {
		t.Fatalf("active_org_id absent; the Linear read under test needs an active org: %v", me)
	}
	for _, field := range []string{"linear_user_id", "linear_display_name"} {
		if v, ok := me[field]; ok {
			t.Errorf("%s = %v, want absent (the active org has no Linear workspace)", field, v)
		}
	}

	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_settings (org_id, linear_workspace_id, linear_workspace_url_key)
		 VALUES ($1, 'ws-org', 'acme')
		 ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id,
		                                    linear_workspace_url_key = EXCLUDED.linear_workspace_url_key`, orgID)
	me = readMe()
	if me["active_org_id"] != orgID.String() {
		t.Fatalf("active_org_id = %v, want %s", me["active_org_id"], orgID)
	}
	if me["linear_user_id"] != "lin-me" || me["linear_display_name"] != "Me In Linear" {
		t.Errorf("linear pair = (%v, %v), want (lin-me, Me In Linear)", me["linear_user_id"], me["linear_display_name"])
	}

	// The org must keep an owner, so hand it to someone else first.
	pgtest.AddOrgMember(t, r.h, r.seedUser().String(), orgID.String(), teamID.String(), "owner", "admin")
	pgtest.MustExec(t, r.h.AdminDB, `DELETE FROM org_memberships WHERE user_id = $1 AND org_id = $2`, userID, orgID)
	me = readMe()
	for _, field := range []string{"active_org_id", "linear_user_id", "linear_display_name"} {
		if v, ok := me[field]; ok && v != "" {
			t.Errorf("after leaving the org: %s = %v, want absent", field, v)
		}
	}
}

// A brand-new GitHub login with no profile name gets its login handle as
// display_name, not NULL. It skips seedUser's pre-linked identity so the callback
// takes the new-principal insert path.
func TestAuthFlow_LoginToMe_DisplayNameFallsBackToGitHubLogin(t *testing.T) {
	r := newAuthRig(t)

	var idStr string
	if err := r.h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	r.h.SeedAuthUser(t, idStr, idStr+"@test")

	login := "octocat-" + idStr[:8]
	claims := jwt.MapClaims{
		"sub":   idStr,
		"email": idStr + "@test",
		"iss":   testIssuer,
		"aud":   testAudience,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "github",
			"providers": []any{"github"},
		},
		"user_metadata": map[string]any{
			"user_name":  login,
			"avatar_url": "https://avatar.example/" + idStr[:8],
		},
	}

	resp, _ := r.driveCallbackClaims(claims)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	meResp := r.requestWithSid("GET", "/api/me", sid)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	var me struct {
		DisplayName    string `json:"display_name"`
		GitHubUsername string `json:"github_username"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.DisplayName != login {
		t.Errorf("me.display_name = %q, want %q (fallback to GitHub login)", me.DisplayName, login)
	}
	if me.GitHubUsername != login {
		t.Errorf("me.github_username = %q, want %q", me.GitHubUsername, login)
	}
}

// A user with no memberships gets /api/me with active_org_id omitted, not empty,
// and teams present as [].
func TestAuthFlow_Me_NoMemberships_OmitsActiveOrgID(t *testing.T) {
	r := newAuthRig(t)
	// Signup provisions nothing, so a fresh user with no seedOrg lands
	// with active_org_id NULL — the zero-membership scenario this exercises.
	userID := r.seedUser()
	// Deliberately no seedOrg — session lands with active_org_id NULL.

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	meResp := r.requestWithSid("GET", "/api/me", sid)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	raw, err := io.ReadAll(meResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Decode into a map so we can distinguish "field absent" from
	// "field present and empty" — that's exactly the omitempty contract
	// the ticket pins.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if _, present := m["active_org_id"]; present {
		t.Errorf("active_org_id present in /api/me with zero memberships: %s", raw)
	}
	if m["id"] != userID.String() {
		t.Errorf("id = %v, want %s", m["id"], userID)
	}
	// teams takes the opposite contract to active_org_id: [] is an answer
	// ("no teams"), so the key is always there, exactly like orgs.
	teams, present := m["teams"]
	if !present {
		t.Errorf("teams absent from /api/me: %s", raw)
	} else if arr, ok := teams.([]any); !ok || len(arr) != 0 {
		t.Errorf("teams = %v, want []", teams)
	}
}

// A session whose active org the user has since left gets /api/me with
// active_org_id omitted and that org absent from orgs, so a client cannot route
// to an org the caller no longer belongs to.
func TestAuthFlow_Me_StaleActiveOrgMembership_OmitsActiveOrgID(t *testing.T) {
	r := newAuthRig(t)

	// Org owned by a different user so the schema CHECK ("each org must
	// retain at least one owner") doesn't block the revoke below. Our
	// caller joins as a non-owner member; revoking their membership
	// leaves the owner intact.
	ownerID := r.seedUser()
	orgID, _ := r.seedOrg(ownerID, "real-owner-org")

	userID := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		userID, orgID,
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Revoke the member's tie. The org row stays (owner intact) — only
	// this user's tie to it goes away. session.active_org_id keeps
	// pointing at orgID.
	if _, err := r.h.AdminDB.Exec(
		`DELETE FROM public.org_memberships WHERE user_id = $1 AND org_id = $2`,
		userID, orgID,
	); err != nil {
		t.Fatalf("delete membership: %v", err)
	}

	meResp := r.requestWithSid("GET", "/api/me", sid)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	raw, err := io.ReadAll(meResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if v, present := m["active_org_id"]; present {
		t.Errorf("active_org_id surfaced for stale membership: %v (body=%s)", v, raw)
	}
	// Sanity: orgs[] is filtered through the membership query too, so
	// the no-longer-member org is absent there as well.
	orgs, _ := m["orgs"].([]any)
	if len(orgs) != 0 {
		t.Errorf("orgs[] non-empty after revoke: %v", orgs)
	}
}

// A multi-mode user whose id equals the local sentinel gets their real /api/me
// from the database, never the synthesized Local response: the synthesis is
// gated on authDeps being nil, not on the subject.
func TestAuthFlow_Me_SentinelSubjectInMultiMode_HitsDBPath(t *testing.T) {
	r := newAuthRig(t)

	// Seed a user whose ID IS the sentinel UUID.
	sentinelID := uuid.MustParse(runmode.LocalDefaultUserID)
	r.h.SeedAuthUser(t, sentinelID.String(), sentinelID.String()+"@test")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		sentinelID.String(), "sentinel-collision",
	); err != nil {
		t.Fatalf("seed sentinel public.users: %v", err)
	}
	// Link the login identity to this principal so the callback resolves back to
	// it rather than minting a fresh principal.
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $1, 'github', $2, true)`, sentinelID.String(), sentinelID.String()+"@test"); err != nil {
		t.Fatalf("seed sentinel identity: %v", err)
	}
	orgID, _ := r.seedOrg(sentinelID, "real-org-for-sentinel")

	resp, _ := r.driveCallback(sentinelID)
	sid := r.sidFromResp(resp)

	meResp := r.requestWithSid("GET", "/api/me", sid)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	var body struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		ActiveOrgID string `json:"active_org_id"`
		Orgs        []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != sentinelID.String() {
		t.Errorf("id = %q, want %q", body.ID, sentinelID)
	}
	if body.Email != sentinelID.String()+"@test" {
		t.Errorf("email = %q, want the principal's identity email", body.Email)
	}
	// The hallmark of the synthesis path is DisplayName="Local" + the
	// sentinel org. Either of those leaking through here means we
	// short-circuited a real multi-mode session.
	if body.DisplayName == "Local" {
		t.Error("DisplayName = \"Local\" — local-mode synthesis ran in multi mode")
	}
	if body.ActiveOrgID != orgID.String() {
		t.Errorf("active_org_id = %q, want real org %q (synthesized response would point at LocalDefaultOrgID)",
			body.ActiveOrgID, orgID)
	}
	if len(body.Orgs) != 1 || body.Orgs[0].ID != orgID.String() {
		t.Errorf("orgs = %+v, want one real org %s", body.Orgs, orgID)
	}
	if body.Orgs[0].Name == "Local" {
		t.Error("orgs[0].name = \"Local\" — sentinel org leaked into multi-mode response")
	}
	if body.Orgs[0].Role != "owner" {
		t.Errorf("orgs[0].role = %q, want owner", body.Orgs[0].Role)
	}
}

// TestAuthFlow_LogoutFlipsRevoked: login records login_success; logout calls
// GoTrue logout with the session's JWT, revokes the session (later requests
// 401, the row stays with revoked_at set) and records a logout event.
func TestAuthFlow_LogoutFlipsRevoked(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	resp, jwt := r.driveCallback(userID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	login := r.oneAuthEvent(domain.AuthEventLoginSuccess)
	if login.userID != userID.String() || login.orgID != orgID.String() || login.sessionID != sid {
		t.Errorf("login_success (user, org, session) = (%q, %q, %q), want (%q, %q, %q)",
			login.userID, login.orgID, login.sessionID, userID, orgID, sid)
	}
	if login.detail["method"] != "github" || login.detail["sso"] != false {
		t.Errorf("login_success detail=%v, want method=github sso=false", login.detail)
	}

	// Sanity: /api/me works before logout.
	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusOK {
		t.Fatalf("pre-logout /api/me = %d, want 200", got)
	}

	logoutResp := r.requestWithSid("POST", "/api/auth/logout", sid)
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204", logoutResp.StatusCode)
	}

	// Upstream logout runs with the access token, so an exfiltrated JWT cannot
	// keep refreshing after the user logs out.
	if len(r.gotrueLogoutCalls) != 1 || r.gotrueLogoutCalls[0] != jwt {
		t.Errorf("upstream logout calls = %d, want 1 carrying the session's JWT", len(r.gotrueLogoutCalls))
	}

	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("post-logout /api/me = %d, want 401", got)
	}

	logout := r.oneAuthEvent(domain.AuthEventLogout)
	if logout.userID != userID.String() || logout.sessionID != sid || logout.orgID != orgID.String() {
		t.Errorf("logout (user, session, org) = (%q, %q, %q), want (%q, %q, %q): the revoked session's active org",
			logout.userID, logout.sessionID, logout.orgID, userID, sid, orgID)
	}

	// Row persists with revoked_at NOT NULL.
	sidUUID := uuid.MustParse(sid)
	var revokedAt *time.Time
	if err := r.h.AdminDB.QueryRow(
		`SELECT revoked_at FROM public.sessions WHERE id = $1`, sidUUID,
	).Scan(&revokedAt); err != nil {
		t.Fatalf("post-logout select: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("logout did not set revoked_at")
	}
}

// A session whose expires_at has passed is a 401 even while its JWT is valid.
func TestAuthFlow_ForceExpiryReturns401(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Backdate expires_at past now. Must also push created_at into the
	// past to satisfy CHECK (expires_at > created_at).
	sidUUID := uuid.MustParse(sid)
	if _, err := r.h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '2 hours',
		       jwt_expires_at = now() - interval '1 hour 30 minutes',
		       expires_at     = now() - interval '1 minute'
		 WHERE id = $1`, sidUUID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("expired /api/me = %d, want 401", got)
	}
}

// RequireOrgMember answers a member's org 200, another org 404 (not 403, which
// would disclose that it exists), and a malformed org id 404. The probe route is
// mounted the way every org-scoped route is.
func TestAuthFlow_OrgMiddleware_CrossOrg404AndMember200(t *testing.T) {
	r := newAuthRig(t)

	r.srv.mux.Handle("GET /api/orgs/{org_id}/probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if _, _, ok := r.srv.az.RequireOrgMember(w, req); !ok {
				return
			}
			w.WriteHeader(http.StatusOK)
		})))

	userA := r.seedUser()
	orgA, _ := r.seedOrg(userA, "alice-org")

	userB := r.seedUser()
	orgB, _ := r.seedOrg(userB, "bob-org")

	// Sign User A in.
	resp, _ := r.driveCallback(userA)
	sid := r.sidFromResp(resp)

	// User A → /api/orgs/{orgA}/probe = 200
	gotA := r.requestWithSid("GET", "/api/orgs/"+orgA.String()+"/probe", sid).StatusCode
	if gotA != http.StatusOK {
		t.Errorf("User A → orgA: got %d, want 200", gotA)
	}

	// User A → /api/orgs/{orgB}/probe = 404
	gotB := r.requestWithSid("GET", "/api/orgs/"+orgB.String()+"/probe", sid).StatusCode
	if gotB != http.StatusNotFound {
		t.Errorf("User A → orgB: got %d, want 404 (not 403 — don't leak existence)", gotB)
	}

	// Also verify the path-malformed case 404s (not 400).
	gotBad := r.requestWithSid("GET", "/api/orgs/not-a-uuid/probe", sid).StatusCode
	if gotBad != http.StatusNotFound {
		t.Errorf("malformed org_id: got %d, want 404", gotBad)
	}

	_ = userB
}

func TestAuthFlow_NoSidCookie_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.requestWithSid("GET", "/api/me", "").StatusCode
	if got != http.StatusUnauthorized {
		t.Errorf("no cookie /api/me = %d, want 401", got)
	}
}

func TestAuthFlow_GarbageSidCookie_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.requestWithSid("GET", "/api/me", "not-a-uuid").StatusCode
	if got != http.StatusUnauthorized {
		t.Errorf("garbage cookie /api/me = %d, want 401", got)
	}
}

// Concurrent requests on a session in its refresh window call GoTrue refresh
// once and all succeed. Without serialization, refresh-token rotation fails the
// second refresh and the user gets a spurious 401.
func TestAuthFlow_ConcurrentRefresh_SerializesAndDedupes(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	// Mint a JWT whose exp is inside the refresh window so the
	// middleware decides it needs to refresh. We can't use
	// driveCallback because that sets a 1-hour exp; we need to
	// bypass the helper and mint by hand.
	jwtBefore := r.signKey.mintJWT(t, validClaimsFor(userID))

	// Create the session directly via the store with a near-expiry
	// JWT exp. Bypasses the callback handler entirely so we control
	// the timing precisely.
	encStore := r.srv.authDeps.sessions
	sess, err := encStore.CreateSystem(context.Background(), userID, userID, "", jwtBefore, "refresh-token-original",
		time.Now().Add(30*time.Second).UTC(),  // JWT expires in 30s → needsRefresh = true
		time.Now().Add(30*24*time.Hour).UTC(), // session valid for 30d
		"test-ua", "127.0.0.1", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// Stub gotrueRefresh: count invocations, block on a release channel
	// until the test gives the signal.
	var (
		refreshCalls atomic.Int32
		release      = make(chan struct{})
	)
	newJWT := r.signKey.mintJWT(t, validClaimsFor(userID))
	r.srv.authDeps.gotrueRefresh = func(ctx context.Context, refresh string) (string, string, int64, error) {
		refreshCalls.Add(1)
		<-release
		return newJWT, "refresh-token-rotated", time.Now().Add(1 * time.Hour).Unix(), nil
	}

	// Launch N concurrent requests.
	const n = 5
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			results <- r.requestWithSid("GET", "/api/me", sess.ID.String()).StatusCode
		}()
	}

	// Wait until the first refresh call has landed. After that, the
	// remaining n-1 goroutines are either waiting on the per-session
	// mutex or have re-fetched and skipped (race-dependent — both are
	// correct).
	deadline := time.Now().Add(3 * time.Second)
	for refreshCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if refreshCalls.Load() == 0 {
		t.Fatal("first refresh never started — middleware didn't enter the refresh branch")
	}

	// Release the refresh — first call completes, lock unlocks, queued
	// goroutines acquire-and-re-fetch-and-skip.
	close(release)

	// Collect all results.
	statuses := make([]int, 0, n)
	for i := 0; i < n; i++ {
		select {
		case s := <-results:
			statuses = append(statuses, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d/%d timed out after refresh release", i+1, n)
		}
	}

	// Every request should have succeeded (no spurious 401s).
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("request %d: status=%d, want 200 (refresh-race caused spurious 401)", i, s)
		}
	}

	// gotrueRefresh called exactly once across all N requests. The
	// remaining n-1 saw the fresh DB row and skipped.
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls = %d, want 1 (lock didn't dedupe concurrent refreshes)", got)
	}
}

// A shared refresh runs on its own context: the first caller cancelling its
// request does not cancel the in-flight GoTrue call the other waiters share.
func TestAuthFlow_RefreshIgnoresCallerCtxCancellation(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	jwtBefore := r.signKey.mintJWT(t, validClaimsFor(userID))
	encStore := r.srv.authDeps.sessions
	sess, err := encStore.CreateSystem(context.Background(), userID, userID, "", jwtBefore, "refresh-original",
		time.Now().Add(30*time.Second).UTC(),
		time.Now().Add(30*24*time.Hour).UTC(),
		"test-ua", "127.0.0.1", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	newJWT := r.signKey.mintJWT(t, validClaimsFor(userID))
	inFlight := make(chan struct{})
	release := make(chan struct{})

	// Stub gotrueRefresh to: signal "started", then block on either
	// release (the test's signal to finish) OR fnCtx.Done(). If the
	// detach is broken, fnCtx == caller's ctx and the test's cancel
	// races ahead of release — we'd return ctx.Err() and the assertion
	// below fails.
	r.srv.authDeps.gotrueRefresh = func(fnCtx context.Context, refresh string) (string, string, int64, error) {
		close(inFlight)
		select {
		case <-release:
			return newJWT, "refresh-rotated", time.Now().Add(time.Hour).Unix(), nil
		case <-fnCtx.Done():
			return "", "", 0, fnCtx.Err()
		}
	}

	// Caller A: cancellable ctx. Will cancel mid-refresh.
	aCtx, aCancel := context.WithCancel(context.Background())
	aSess := *sess
	aDone := make(chan error, 1)
	go func() {
		aDone <- r.srv.refreshSessionInline(aCtx, &aSess)
	}()

	// Wait until fn is in-flight.
	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("fn never started — refreshSessionInline didn't reach gotrueRefresh")
	}

	// Cancel A's context. With detach, fnCtx is untouched.
	aCancel()

	// Give the cancellation 100ms to (incorrectly) propagate. If detach
	// is broken, fn returns ctx.Canceled here, before release fires.
	select {
	case err := <-aDone:
		t.Fatalf("refresh returned before release (err=%v) — caller ctx cancellation propagated into fn (detach broken)", err)
	case <-time.After(100 * time.Millisecond):
		// Good: fn is still blocked on release, ignoring A's cancellation.
	}

	// Release: fn completes cleanly, A picks up the result.
	close(release)
	select {
	case err := <-aDone:
		if err != nil {
			t.Errorf("after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never returned after release")
	}

	// Sanity: aSess got the rotated JWT.
	if aSess.JWT != newJWT {
		t.Errorf("aSess.JWT not updated to rotated value")
	}
}

// Upstream logout failure does NOT block local revoke. The session
// still ends from the client's perspective; we lose only the
// belt-and-suspenders upstream invalidation.
func TestAuthFlow_Logout_UpstreamFailureStillLocallyRevokes(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Override the stub to fail.
	r.srv.authDeps.gotrueLogout = func(ctx context.Context, accessToken string) error {
		return errSimulated
	}

	logoutResp := r.requestWithSid("POST", "/api/auth/logout", sid)
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204 (upstream failure must not block)", logoutResp.StatusCode)
	}

	// Local revocation still happened.
	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("post-(failed-upstream)-logout /api/me = %d, want 401", got)
	}
}

// CSRF Origin check rejects cross-origin POSTs to mutating endpoints.
// SameSite=Lax alone permits top-level cross-site POSTs; the Origin
// match closes that gap.
func TestAuthFlow_Logout_CSRFOriginCheck(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")
	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Forge a cross-origin POST: same cookie + path, but Origin
	// header points at an attacker site.
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin logout = %d, want 403", rec.Code)
	}

	// The session must still be live (logout was blocked).
	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusOK {
		t.Errorf("post-blocked-CSRF /api/me = %d, want 200 (session intact)", got)
	}
}

// errSimulated is a sentinel used by tests that need to assert
// failure-path handling without exporting a public error symbol.
var errSimulated = errorString("simulated failure")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestAuthFlow_Callback_RejectsBadState(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	// Wrong CSRF in query vs cookie. The PKCE code is unused — the
	// CSRF check fires before the exchange call, so the request is
	// rejected without contacting (the stubbed) gotrue.
	stateCookie := r.signStateCookie("/dashboard", "the-real-csrf", "verifier")
	q := url.Values{}
	q.Set("state", "different-csrf")
	q.Set("code", "any-code")
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("state mismatch status = %d, want 400", rec.Code)
	}

	// One login_failure naming the branch and the method, which the parsed
	// state carries; no identity is known yet, so no user.
	ev := r.oneAuthEvent(domain.AuthEventLoginFailure)
	if ev.detail["reason"] != "state_mismatch" || ev.detail["method"] != "github" {
		t.Errorf("login_failure detail=%v, want reason=state_mismatch method=github", ev.detail)
	}
	if ev.userID != "" {
		t.Errorf("login_failure carried a user=%q, want none (pre-identity)", ev.userID)
	}
}

// ---------- standalone tests: cookies, return_to, client IP, GoTrue client ----------

// Cookie names follow the deployment scheme: HTTPS uses the __Host-
// prefix (browser-enforced Secure + Path=/ + no Domain), HTTP keeps
// plain sid so local dev/tests work without TLS.
func TestSidCookieName_DependsOnPublicURL(t *testing.T) {
	// We can't easily re-run newAuthRig with a different publicURL
	// without paying another pgtest boot cost. Instead, construct a
	// minimal Server + deployCfg by hand and probe sidCookieName.
	cases := []struct {
		publicURL string
		want      string
	}{
		{"https://triagefactory.acme.com", "__Host-sid"},
		{"http://localhost:3000", "sid"},
		{"http://tf.test", "sid"},
	}
	for _, tc := range cases {
		s := &Server{
			deployCfg: &deployConfig{
				publicURL:     tc.publicURL,
				secureCookies: strings.HasPrefix(tc.publicURL, "https://"),
			},
		}
		if got := s.sidCookieName(); got != tc.want {
			t.Errorf("publicURL=%s: sidCookieName=%q, want %q", tc.publicURL, got, tc.want)
		}
	}

	// No deployCfg → plain name.
	bare := &Server{}
	if got := bare.sidCookieName(); got != "sid" {
		t.Errorf("nil deployCfg: sidCookieName=%q, want sid", got)
	}
}

// An HTTPS deployment marks auth cookies Secure even for a request that reaches
// Go over plain HTTP, as behind a TLS-terminating proxy.
func TestCookieSecure_FromPublicURL(t *testing.T) {
	s := &Server{deployCfg: &deployConfig{secureCookies: true}}
	r := httptest.NewRequest("GET", "http://internal/auth", nil)
	if !s.cookieSecure(r) {
		t.Errorf("https publicURL: cookieSecure(plain-http req) = false, want true")
	}
}

func TestStateCookie_Roundtrip(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sc := stateClaims{ReturnTo: "/dashboard", CSRF: "abc", ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	signed, err := sc.sign(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := parseStateCookie(signed, key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ReturnTo != "/dashboard" || got.CSRF != "abc" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestStateCookie_WrongKey(t *testing.T) {
	var k1, k2 [32]byte
	_, _ = rand.Read(k1[:])
	_, _ = rand.Read(k2[:])
	sc := stateClaims{ReturnTo: "/", CSRF: "x", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	signed, _ := sc.sign(k1)
	if _, err := parseStateCookie(signed, k2); err == nil {
		t.Fatal("parse with wrong key succeeded")
	}
}

func TestStateCookie_Expired(t *testing.T) {
	var key [32]byte
	_, _ = rand.Read(key[:])
	sc := stateClaims{ReturnTo: "/", CSRF: "x", ExpiresAt: time.Now().Add(-1 * time.Minute).Unix()}
	signed, _ := sc.sign(key)
	if _, err := parseStateCookie(signed, key); err == nil {
		t.Fatal("parse of expired cookie succeeded")
	}
}

func TestNormalizeReturnTo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/dashboard", "/dashboard"},
		{"/orgs/123/tasks", "/orgs/123/tasks"},
		{"//evil.com/path", "/"},     // protocol-relative
		{"https://evil.com/", "/"},   // absolute URL
		{"javascript:alert(1)", "/"}, // scheme-relative
		{"path-without-slash", "/"},
		// WHATWG URL parsing treats `\` as `/` for special schemes,
		// so `/\evil.com` resolves cross-origin. Reject explicit `\`
		// and percent-encoded `%5C` variants.
		{`/\evil.com`, "/"},
		{`/\\evil.com`, "/"},
		{`/foo\bar`, "/"},
		{`/%5Cevil.com`, "/"},
		{`/%5c%5cevil.com`, "/"}, // lowercase hex
		// Browsers strip tabs and newlines before resolving, which turns
		// each of these into `//evil.com`.
		{"/\t/evil.com", "/"},
		{"/\n/evil.com", "/"},
		{"/\r/evil.com", "/"},
	}
	for _, tc := range cases {
		if got := normalizeReturnTo(tc.in); got != tc.want {
			t.Errorf("normalizeReturnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// clientIP returns a value Postgres inet accepts (bracketed v6 unwrapped) and
// honors X-Forwarded-For only behind a configured trusted proxy, walking it right
// to left so a forged left-hand entry is never reached.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name        string
		trustedCIDR string // TF_TRUSTED_PROXY_CIDR
		capture     bool   // TF_CAPTURE_CLIENT_IP
		remoteAddr  string
		xff         string
		want        string
	}{
		// --- secure default: no trusted-proxy allowlist, XFF always ignored ---
		{"default ipv4 with port", "", true, "192.0.2.1:54321", "", "192.0.2.1"},
		{"default ipv6 with port (bracketed)", "", true, "[2001:db8::1]:54321", "", "2001:db8::1"},
		{"default ipv6 loopback", "", true, "[::1]:8080", "", "::1"},
		{"default ipv4 no port (degenerate)", "", true, "192.0.2.1", "", "192.0.2.1"},
		// Spoof attempt against a directly-exposed deployment: XFF is forged,
		// must be ignored, the real peer wins.
		{"default ignores spoofed xff", "", true, "192.0.2.1:54321", "9.9.9.9", "192.0.2.1"},
		{"default ignores spoofed xff chain", "", true, "192.0.2.1:54321", "9.9.9.9, 8.8.8.8", "192.0.2.1"},

		// --- behind a trusted proxy: walk XFF right→left ---
		{"proxy single client", "10.0.0.0/8", true, "10.0.0.1:5000", "203.0.113.5", "203.0.113.5"},
		// Internal hop (health check / internal call) appended after the
		// client — skipped, client recovered.
		{"proxy multi-hop skips trusted", "10.0.0.0/8", true, "10.0.0.1:5000", "203.0.113.5, 10.0.0.9", "203.0.113.5"},
		// Spoof through the proxy: the forged value sits LEFT of the address
		// the proxy appended, so the right→left walk never reaches it.
		{"proxy ignores pre-seeded spoof", "10.0.0.0/8", true, "10.0.0.1:5000", "9.9.9.9, 203.0.113.5", "203.0.113.5"},
		// All XFF entries are trusted hops → fall back to the peer.
		{"proxy all-trusted falls back to peer", "10.0.0.0/8", true, "10.0.0.1:5000", "10.0.0.5, 10.0.0.9", "10.0.0.1"},
		// Peer is trusted but no XFF → peer (degraded but safe).
		{"proxy no xff falls back to peer", "10.0.0.0/8", true, "10.0.0.1:5000", "", "10.0.0.1"},
		// Allowlist set, but the peer is NOT in it (direct exposure): XFF
		// still ignored — only a trusted peer unlocks the header.
		{"untrusted peer ignores xff", "10.0.0.0/8", true, "192.0.2.1:54321", "203.0.113.5", "192.0.2.1"},
		// IPv6 client behind a trusted proxy.
		{"proxy ipv6 client", "10.0.0.0/8", true, "10.0.0.1:5000", "2001:db8::abc", "2001:db8::abc"},
		// Some proxies append "ip:port" in XFF — the port is stripped.
		{"proxy strips xff port", "10.0.0.0/8", true, "10.0.0.1:5000", "203.0.113.5:9999", "203.0.113.5"},
		// IPv6 trusted proxy + IPv6 CIDR.
		{"proxy ipv6 peer ipv6 cidr", "2001:db8::/32", true, "[2001:db8::1]:5000", "203.0.113.5", "203.0.113.5"},
		// Bare-IP allowlist entry (promoted to /32).
		{"proxy bare-ip allowlist", "10.0.0.1", true, "10.0.0.1:5000", "203.0.113.5", "203.0.113.5"},
		// Dual-stack LB: the kernel hands Go an IPv4-mapped IPv6 peer
		// (::ffff:10.0.0.1); an operator's plain IPv4 CIDR must still match it.
		{"ipv4-mapped peer matches v4 cidr", "10.0.0.0/8", true, "[::ffff:10.0.0.1]:5000", "203.0.113.5", "203.0.113.5"},

		// --- capture opt-out → "" (stored as NULL) ---
		{"capture off returns empty", "10.0.0.0/8", false, "10.0.0.1:5000", "203.0.113.5", ""},
		{"capture off direct returns empty", "", false, "192.0.2.1:54321", "", ""},
		{"capture off via none sentinel", "none", true, "10.0.0.1:5000", "203.0.113.5", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetClientIPPolicyForTest(t, tc.trustedCIDR, tc.capture)
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP(remote=%q, xff=%q, trusted=%q, capture=%v) = %q, want %q",
					tc.remoteAddr, tc.xff, tc.trustedCIDR, tc.capture, got, tc.want)
			}
		})
	}
}

// clientIP does constant work on an attacker-sized X-Forwarded-For: it scans
// right to left with a hop cap, so a megabyte of commas cannot amplify a
// pre-auth request.
func TestClientIP_LargeXFFHeaderBounded(t *testing.T) {
	runmode.SetClientIPPolicyForTest(t, "10.0.0.0/8", true)

	t.Run("padding left of the appended client is never walked", func(t *testing.T) {
		// Attacker pre-seeds a megabyte of commas; the trusted proxy appends
		// the real (untrusted) peer on the right. Right→left finds it first.
		xff := strings.Repeat(",", 1<<20) + "203.0.113.5"
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", xff)
		if got := clientIP(req); got != "203.0.113.5" {
			t.Fatalf("clientIP = %q, want 203.0.113.5 (right→left finds the appended client)", got)
		}
	})

	t.Run("all-empty header falls back to the trusted peer", func(t *testing.T) {
		xff := strings.Repeat(",", 1<<20)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", xff)
		if got := clientIP(req); got != "10.0.0.1" {
			t.Fatalf("clientIP = %q, want 10.0.0.1 (no client in header → trusted peer)", got)
		}
	})

	t.Run("walk stops at the hop cap before reaching a far-left client", func(t *testing.T) {
		// Client at the far left, behind exactly maxXFFHops trusted hops. The
		// right→left walk spends its whole hop budget skipping trusted entries
		// and never reaches the client, so it falls back to the trusted peer.
		// Without the cap the client would win — so this pins the bound.
		xff := "203.0.113.5" + strings.Repeat(", 10.0.0.1", maxXFFHops)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.2:5000" // trusted (10/8), distinct from the hops
		req.Header.Set("X-Forwarded-For", xff)
		if got := clientIP(req); got != "10.0.0.2" {
			t.Fatalf("clientIP = %q, want 10.0.0.2 (cap reached before the far-left client → peer)", got)
		}
	})
}

// gotrueHTTPClient has a timeout between zero and two minutes, so a hung GoTrue
// cannot block the token exchange or logout indefinitely.
func TestGoTrueHTTPClient_HasTimeout(t *testing.T) {
	if gotrueHTTPClient.Timeout <= 0 {
		t.Fatalf("gotrueHTTPClient.Timeout = %v, want > 0", gotrueHTTPClient.Timeout)
	}
	// Upper-bound sanity: token exchanges complete sub-second in normal
	// operation. A timeout above ~2 minutes is almost certainly a typo
	// (units mix-up). If we ever legitimately need it higher, bump this
	// bound deliberately.
	if gotrueHTTPClient.Timeout > 2*time.Minute {
		t.Errorf("gotrueHTTPClient.Timeout = %v, suspicious (>2m); confirm units", gotrueHTTPClient.Timeout)
	}
}

// TestGoTrueHTTPClient_EnforcesTimeout proves the bound is actually
// honored by swapping in a short-deadline client against a deliberately
// slow httptest server. Validates wiring, not the specific 30s default.
func TestGoTrueHTTPClient_EnforcesTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the override timeout below; the client
		// should cancel before this returns.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	orig := gotrueHTTPClient
	gotrueHTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { gotrueHTTPClient = orig }()

	req, _ := http.NewRequest(http.MethodGet, slow.URL, nil)
	start := time.Now()
	_, err := gotrueHTTPClient.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Generous upper bound — 50ms timeout + scheduling slack. Anything
	// near the server's 500ms means the timeout didn't fire.
	if elapsed > 300*time.Millisecond {
		t.Errorf("request took %v, expected timeout near 50ms (timeout not enforced)", elapsed)
	}
}

// The PKCE exchange and refresh requests send a JSON body with Content-Type
// application/json, which GoTrue's /token handler requires for every grant type.
func TestGoTrueTokenRequests_AreJSON(t *testing.T) {
	type captured struct {
		path        string
		contentType string
		body        map[string]string
	}
	var (
		mu  sync.Mutex
		got []captured
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, captured{
			path:        r.URL.Path + "?" + r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600}`))
	}))
	defer upstream.Close()

	srv := &Server{}
	cfg := &authConfig{gotrueURL: upstream.URL}

	if _, _, _, err := srv.gotrueExchangeFunc(cfg)(context.Background(), "code-x", "verifier-y"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, _, _, err := srv.gotrueRefreshFunc(cfg)(context.Background(), "refresh-z"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(got))
	}
	for _, c := range got {
		if c.contentType != "application/json" {
			t.Errorf("Content-Type=%q want application/json (path=%s)", c.contentType, c.path)
		}
		if len(c.body) == 0 {
			t.Errorf("body did not parse as JSON or was empty (path=%s)", c.path)
		}
	}
	if got[0].body["auth_code"] != "code-x" || got[0].body["code_verifier"] != "verifier-y" {
		t.Errorf("pkce body fields: %v", got[0].body)
	}
	if got[1].body["refresh_token"] != "refresh-z" {
		t.Errorf("refresh body fields: %v", got[1].body)
	}
}

// meTeam is the shape of one row in /api/me's teams list. Declared once so the
// membership tests below decode the same contract the frontend's AuthTeam does.
type meTeam struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OrgID string `json:"org_id"`
	Role  string `json:"role"`
}

// fetchMeTeams drives GET /api/me with sid and returns its teams list.
func (r *authRig) fetchMeTeams(sid string) []meTeam {
	r.t.Helper()
	resp := r.requestWithSid("GET", "/api/me", sid)
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("/api/me status=%d, want 200", resp.StatusCode)
	}
	var body struct {
		Teams []meTeam `json:"teams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		r.t.Fatalf("decode /api/me: %v", err)
	}
	return body.Teams
}

// seedTeam inserts a team in orgID, optionally enrolling userID at role, and
// returns its id. role == "" enrolls nobody — the org-visible team the caller
// never joined.
func (r *authRig) seedTeam(orgID uuid.UUID, name, role string, userID uuid.UUID) uuid.UUID {
	r.t.Helper()
	var tID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO teams (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id::text
	`, orgID, name).Scan(&tID); err != nil {
		r.t.Fatalf("insert team %s: %v", name, err)
	}
	if role != "" {
		if _, err := r.h.AdminDB.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, $3)`,
			userID, tID, role); err != nil {
			r.t.Fatalf("insert membership on %s: %v", name, err)
		}
	}
	return uuid.MustParse(tID)
}

// /api/me's teams are the caller's own memberships across every org, each with
// its org_id and the role held (viewer included), excluding archived teams and
// teams the caller can see but never joined.
func TestAuthFlow_Me_TeamMemberships(t *testing.T) {
	r := newAuthRig(t)

	userID := r.seedUser()
	orgA, teamA := r.seedOrg(userID, "org-a")
	orgB, teamB := r.seedOrg(userID, "org-b")

	viewerTeam := r.seedTeam(orgA, "reviewers", "viewer", userID)
	archivedTeam := r.seedTeam(orgA, "sunset", "admin", userID)
	if _, err := r.h.AdminDB.Exec(
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, archivedTeam); err != nil {
		t.Fatalf("archive team: %v", err)
	}
	// Visible to the caller (org access is what teams_select gates on) but
	// never joined — no membership row, so no role to report.
	unjoinedTeam := r.seedTeam(orgA, "platform", "", userID)

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	teams := r.fetchMeTeams(sid)

	byID := map[string]meTeam{}
	for _, tm := range teams {
		byID[tm.ID] = tm
	}
	for _, want := range []struct {
		id    uuid.UUID
		orgID uuid.UUID
		role  string
		name  string
	}{
		{teamA, orgA, "admin", "default"},
		{teamB, orgB, "admin", "default"},
		{viewerTeam, orgA, "viewer", "reviewers"},
	} {
		got, ok := byID[want.id.String()]
		if !ok {
			t.Fatalf("team %s (%s) missing from /api/me teams: %+v", want.name, want.id, teams)
		}
		if got.OrgID != want.orgID.String() {
			t.Errorf("team %s org_id = %q, want %q", want.name, got.OrgID, want.orgID)
		}
		if got.Role != want.role {
			t.Errorf("team %s role = %q, want %q", want.name, got.Role, want.role)
		}
		if got.Name != want.name {
			t.Errorf("team %s name = %q, want %q", want.id, got.Name, want.name)
		}
	}
	if _, present := byID[archivedTeam.String()]; present {
		t.Errorf("archived team %s present in /api/me teams: %+v", archivedTeam, teams)
	}
	if _, present := byID[unjoinedTeam.String()]; present {
		t.Errorf("never-joined team %s present in /api/me teams: %+v", unjoinedTeam, teams)
	}
	if len(teams) != 3 {
		t.Errorf("teams = %+v, want exactly the 3 membership rows", teams)
	}
}

// TestAuthFlow_Me_NoActiveOrg_StillCarriesTeams: with no active org,
// POST /api/teams/list answers 409 NO_ACTIVE_ORG, but /api/me is keyed on the
// identity alone and still lists the caller's teams.
func TestAuthFlow_Me_NoActiveOrg_StillCarriesTeams(t *testing.T) {
	r := newAuthRig(t)

	userID := r.seedUser()
	orgID, teamID := r.seedOrg(userID, "cursorless-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// The OAuth callback seeds the cursor from the earliest membership; clear
	// it to get the shape a caller with no session state has.
	if _, err := r.h.AdminDB.Exec(`UPDATE sessions SET active_org_id = NULL`); err != nil {
		t.Fatalf("clear active org: %v", err)
	}

	listResp := r.requestWithSid("POST", "/api/teams/list", sid)
	if listResp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /api/teams/list status=%d, want 409 (the gap this field closes)", listResp.StatusCode)
	}
	var conflict struct {
		Errors []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&conflict); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if len(conflict.Errors) != 1 || conflict.Errors[0].Reason != httpx.ReasonNoActiveOrg || conflict.Errors[0].Message == "" {
		t.Errorf("409 errors = %+v, want one NO_ACTIVE_ORG item with a message saying how to pick an org", conflict.Errors)
	}

	teams := r.fetchMeTeams(sid)
	if len(teams) != 1 || teams[0].ID != teamID.String() {
		t.Fatalf("teams = %+v, want the one membership on %s", teams, teamID)
	}
	if teams[0].OrgID != orgID.String() || teams[0].Role != "admin" {
		t.Errorf("teams[0] = %+v, want org %s role admin", teams[0], orgID)
	}
}

// The login-claim mirror is keyed by the claim's GitHub id: a renamed login under
// the same id keeps its recorded email, and a login another id now signs in with
// takes that sign-in's email.
func TestAuthFlow_LoginClaim_DecidesTheAccountByGitHubID(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()

	signIn := func(login, githubID string, emailVerified bool) *http.Response {
		t.Helper()
		claims := validClaimsFor(userID)
		meta := claims["user_metadata"].(map[string]any)
		meta["user_name"] = login
		meta["provider_id"] = githubID
		claims["email_verified"] = emailVerified
		resp, _ := r.driveCallbackClaims(claims)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("callback as %s (%s) status=%d, want 302", login, githubID, resp.StatusCode)
		}
		return resp
	}
	read := func() [3]string {
		t.Helper()
		var got [3]string
		if err := r.h.AdminDB.QueryRow(`
			SELECT login, COALESCE(github_user_id, ''), COALESCE(email, '')
			  FROM user_github_identities
			 WHERE user_id = $1 AND source = 'login_claim'
		`, userID).Scan(&got[0], &got[1], &got[2]); err != nil {
			t.Fatalf("read identity: %v", err)
		}
		return got
	}

	signIn("octo-old", "583231", true)
	signIn("octo-new", "583231", false)
	if got, want := read(), [3]string{"octo-new", "583231", userID.String() + "@test"}; got != want {
		t.Errorf("(login, id, email) after a rename = %q, want %q", got, want)
	}

	resp := signIn("octo-new", "999", false)
	if got, want := read(), [3]string{"octo-new", "999", ""}; got != want {
		t.Errorf("(login, id, email) after another account took the login = %q, want %q", got, want)
	}

	// /api/me serves the account's id beside its login: the value the
	// predicate editor's "match me" writes.
	meResp := r.requestWithSid("GET", "/api/me", r.sidFromResp(resp))
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	var me struct {
		GitHubUsername string `json:"github_username"`
		GitHubUserID   string `json:"github_user_id"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.GitHubUsername != "octo-new" || me.GitHubUserID != "999" {
		t.Errorf("/api/me github = (%q, %q), want (octo-new, 999)", me.GitHubUsername, me.GitHubUserID)
	}
}

// The login claim is recorded under the deployment's GitHub host: github.com
// when unset, or the configured GitHub Enterprise Server.
func TestAuthFlow_LoginClaim_KeysUnderTheDeploymentHost(t *testing.T) {
	for _, tc := range []struct {
		name, setDefault, wantHost string
	}{
		{"default unset", "", ghbase.GitHubCom},
		{"enterprise server", "https://ghe.example.com", "https://ghe.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghbase.SetDefaultBaseURLForTest(t, tc.setDefault)
			r := newAuthRig(t)

			userID := r.seedUser()
			resp, _ := r.driveCallback(userID)
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("callback status=%d, want 302", resp.StatusCode)
			}

			login := "test-user-" + userID.String()[:8]
			var hosts []string
			rows, err := r.h.AdminDB.Query(`
				SELECT github_base_url FROM user_github_identities
				 WHERE login = $1 AND source = 'login_claim'
			`, login)
			if err != nil {
				t.Fatalf("query user_github_identities: %v", err)
			}
			defer rows.Close()
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err != nil {
					t.Fatalf("scan: %v", err)
				}
				hosts = append(hosts, h)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if len(hosts) != 1 || hosts[0] != tc.wantHost {
				t.Errorf("login-claim identity hosts = %v; want exactly [%q]", hosts, tc.wantHost)
			}
		})
	}
}
