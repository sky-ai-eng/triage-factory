package sso_test

// Black-box integration harness for the Enterprise Edition SSO surface.
//
// Unlike the white-box authRig in internal/server (which reaches into Server's
// unexported auth guts), this rig drives a REAL *server.Server purely through
// its public surface: server.New + SetAuthDeps pointed at a fake GoTrue, then
// HTTP over server.Handler(). It never touches a core internal — the SSO
// feature is exercised exactly as a browser would, against the same middleware
// chain production serves.
//
// Two facts make that possible without poking internals:
//   - SetAuthDeps builds the GoTrue token-exchange (and the /sso start) from a
//     URL, so a fake GoTrue at that URL runs the real code path.
//   - The server issues its own signed state cookie at the OAuth start route, so
//     a login is driven by following the cookies + redirect the server sets,
//     never by hand-signing state.
//
// The SSO extension mounts itself: testing package sso links ee/sso, whose
// init() registers the route/login extension + store factories; the entitlement
// provider below licenses the `sso` feature for this test binary (mirroring a
// deployment with a valid TF_LICENSE). Both are process-global, set before any
// server is built. Individual tests that need an UNLICENSED org swap the
// provider for the duration of the subtest via t.Cleanup to restore it.

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
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// ---------- entitlement: license the SSO feature for this test binary ----------

func init() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSSO)) }

// ---------- constants ----------

const (
	testIssuer   = "https://tf.test/auth/v1"
	testAudience = "authenticated"

	// testPublicURL is HTTP, so the server's sid cookie is the plain "sid"
	// (not the HTTPS-only __Host-sid). The rig owns this value, so the cookie
	// names are deterministic.
	testPublicURL = "http://tf.test"
	sidCookieName = "sid"

	// ssoTestServiceToken is the static service-role bearer the rig configures
	// via TF_GOTRUE_SERVICE_ROLE_TOKEN; the fake GoTrue checks TF forwards it
	// verbatim. Opaque — TF treats the token as opaque and just presents it.
	ssoTestServiceToken = "test-service-role-token-deadbeefdeadbeefdeadbeefdeadbeef"
	envServiceRoleToken = "TF_GOTRUE_SERVICE_ROLE_TOKEN"
)

// ---------- JWKS test rig (the rig owns the signing key GoTrue would hold) ----------

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

func (m *jwksMux) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	jwks := make([]map[string]any, 0, len(m.keys))
	for _, k := range m.keys {
		jwks = append(jwks, k.publicJWK())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
}

// validClaimsFor returns a GitHub-login JWT claim map for the given user id.
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

// ---------- fake GoTrue: token exchange + SP-initiated /sso + admin providers ----------

// ssoFakeGoTrue stands in for GoTrue. It serves the three endpoints TF's auth
// code path hits: /token (the PKCE exchange — returns a rig-minted JWT), /sso
// (the SP-initiated SAML start — captures the csrf TF embeds in redirect_to and
// answers 303), and /admin/sso/providers (the admin API connection-create uses).
type ssoFakeGoTrue struct {
	srv       *httptest.Server
	wantToken string

	mu          sync.Mutex
	postCount   int
	lastBody    map[string]any
	nextAccess  string
	nextRefresh string
	failTok     bool // when set, /token answers non-200 (a rejected exchange)
	ssoCount    int
	lastSSOBody map[string]string
	ssoLocation string // the IdP redirect /sso answers with (303 Location)
}

func newSSOFakeGoTrue(t *testing.T, wantToken string) *ssoFakeGoTrue {
	t.Helper()
	f := &ssoFakeGoTrue{wantToken: wantToken, ssoLocation: "https://idp.example/saml/redirect"}
	mux := http.NewServeMux()

	// Admin SSO provider create. GoTrue authenticates the admin call by the
	// service-role token; assert TF presented exactly the configured token.
	mux.HandleFunc("/admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.postCount++
		f.lastBody = body
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"id": uuid.NewString()})
	})

	// PKCE token exchange (and refresh). Returns the access token the rig
	// configured for the next login (a JWT the live Verifier accepts).
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		access, refresh, fail := f.nextAccess, f.nextRefresh, f.failTok
		f.mu.Unlock()
		if fail {
			// A rejected assertion / bad signature: GoTrue 4xx/5xx the exchange.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if access == "" {
			// No token staged → the exchange has nothing to hand back; surface
			// it as an upstream error rather than minting an empty session.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if refresh == "" {
			refresh = "refresh-" + uuid.NewString()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"expires_in":    3600,
		})
	})

	// SP-initiated SAML start. Capture the full body (provider_id, redirect_to
	// carrying the csrf, code_challenge…) so the rig can complete the matching
	// callback and assert what TF posted, then answer 303 with the IdP redirect —
	// gotrueSSOFunc requires a 303 + Location.
	mux.HandleFunc("/sso", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.ssoCount++
		f.lastSSOBody = body
		loc := f.ssoLocation
		f.mu.Unlock()
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusSeeOther)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// stageToken arms the next /token exchange to return this access token.
func (f *ssoFakeGoTrue) stageToken(access string) {
	f.mu.Lock()
	f.nextAccess = access
	f.nextRefresh = "refresh-" + uuid.NewString()
	f.mu.Unlock()
}

// failNextToken arms the next /token exchange to fail (a rejected assertion).
func (f *ssoFakeGoTrue) failNextToken() {
	f.mu.Lock()
	f.failTok = true
	f.mu.Unlock()
}

// ssoCSRF returns the csrf TF embedded in the last /sso redirect_to.
func (f *ssoFakeGoTrue) ssoCSRF() string {
	f.mu.Lock()
	rt := f.lastSSOBody["redirect_to"]
	f.mu.Unlock()
	u, err := url.Parse(rt)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// ssoCalls returns how many times TF posted GoTrue's /sso.
func (f *ssoFakeGoTrue) ssoCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ssoCount
}

// lastSSO returns the body of the last /sso POST (provider_id, redirect_to, …).
func (f *ssoFakeGoTrue) lastSSO() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSSOBody
}

func (f *ssoFakeGoTrue) posts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCount
}

func (f *ssoFakeGoTrue) lastPostBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// ---------- the rig: a real server driven over its public Handler() ----------

type authRig struct {
	t       *testing.T
	h       *pgtest.Harness
	srv     *server.Server
	jwks    *httptest.Server
	jwksMux *jwksMux
	signKey testKey
	fake    *ssoFakeGoTrue
}

// newAuthRig boots the pg testcontainer, a JWKS server (the rig owns the keypair
// that signs access tokens), a fake GoTrue, and a real *server.Server with
// SetAuthDeps pointed at the fake. Multi-mode + the service-role env are set so
// the full SSO surface is live.
func newAuthRig(t *testing.T) *authRig { return newAuthRigWith(t, testPublicURL) }

// newAuthRigWith is newAuthRig with a caller-chosen deployment public URL — used
// by the "public URL unconfigured → 500" test, which builds with "".
func newAuthRigWith(t *testing.T, publicURL string) *authRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	signKey := newTestKey(t)
	jmux := &jwksMux{keys: []testKey{signKey}}
	jwks := httptest.NewServer(jmux)
	t.Cleanup(jwks.Close)

	v, err := verify.NewVerifier(context.Background(), jwks.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	var encKey, cookieSecret [32]byte
	if _, err := rand.Read(encKey[:]); err != nil {
		t.Fatalf("seed enc key: %v", err)
	}
	if _, err := rand.Read(cookieSecret[:]); err != nil {
		t.Fatalf("seed cookie secret: %v", err)
	}
	store := sessions.NewStore(h.AdminDB, sessions.Key(encKey))
	tokenStore := apitokens.NewStore(h.AdminDB)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	fake := newSSOFakeGoTrue(t, ssoTestServiceToken)
	t.Setenv(envServiceRoleToken, ssoTestServiceToken)

	s := server.New(h.AdminDB, stores)

	rigCtx, rigCancel := context.WithCancel(context.Background())
	t.Cleanup(rigCancel)
	// gotrueURL = fake: the real exchange/sso/admin code paths all run against
	// the fake. gotrueAdminBaseURL derives from this same value.
	if err := s.SetAuthDeps(rigCtx, v, store, tokenStore, fake.srv.URL, publicURL, cookieSecret); err != nil {
		t.Fatalf("SetAuthDeps: %v", err)
	}

	return &authRig{t: t, h: h, srv: s, jwks: jwks, jwksMux: jmux, signKey: signKey, fake: fake}
}

// newSSORig is newAuthRig plus a handle to the fake, for tests that assert what
// TF sent GoTrue's admin API.
func newSSORig(t *testing.T) (*authRig, *ssoFakeGoTrue) {
	r := newAuthRig(t)
	return r, r.fake
}

// serve drives one request through the server's public handler (the same
// middleware chain production serves) without opening a socket.
func (r *authRig) serve(req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

// ---------- login drivers (black-box: drive start, follow the server's cookies) ----------

// startLogin drives the OAuth start route, returning the cookies the server set
// (the signed state cookie among them) and the csrf to echo back at the
// callback. github carries csrf in the start redirect's redirect_to; saml posts
// it to the fake /sso, which captured it.
func (r *authRig) startLogin(startPath string, saml bool) (cookies []*http.Cookie, csrf string) {
	r.t.Helper()
	resp := r.serve(httptest.NewRequest(http.MethodGet, startPath, nil))
	cookies = resp.Cookies()
	if saml {
		csrf = r.fake.ssoCSRF()
	} else {
		csrf = csrfFromStartLocation(resp.Header.Get("Location"))
	}
	if csrf == "" {
		r.t.Fatalf("startLogin(%s): no csrf (status=%d, location=%s)",
			startPath, resp.StatusCode, resp.Header.Get("Location"))
	}
	return cookies, csrf
}

// csrfFromStartLocation pulls the csrf out of a github start redirect, whose
// redirect_to is <publicURL>/api/auth/callback?state=<csrf>.
func csrfFromStartLocation(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	rt, err := url.Parse(u.Query().Get("redirect_to"))
	if err != nil {
		return ""
	}
	return rt.Query().Get("state")
}

// driveCallbackClaims drives a full GitHub login: stage the JWT the exchange
// returns, drive start to get a real state cookie, then hit the callback.
// Returns the callback response (carrying the sid Set-Cookie) and the token.
func (r *authRig) driveCallbackClaims(claims jwt.MapClaims) (resp *http.Response, accessToken string) {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, claims)
	r.fake.stageToken(token)
	cookies, csrf := r.startLogin("/api/auth/oauth/github?return_to=/dashboard", false)
	return r.fireCallback(cookies, csrf, url.Values{"code": {"fake-" + uuid.NewString()}}), token
}

func (r *authRig) driveCallback(userID uuid.UUID) (resp *http.Response, accessToken string) {
	r.t.Helper()
	return r.driveCallbackClaims(validClaimsFor(userID))
}

// fireCallback drives GET /api/auth/callback with the start's cookies replayed
// (so the signed state cookie is present — the rig never needs its name) and the
// given query (csrf + a code or error).
func (r *authRig) fireCallback(cookies []*http.Cookie, csrf string, q url.Values) *http.Response {
	r.t.Helper()
	q.Set("state", csrf)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?"+q.Encode(), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return r.serve(req)
}

// ---------- session helpers ----------

// signInAs mints an authed session for userID via a real GitHub login and
// returns the sid cookie value.
func (r *authRig) signInAs(userID uuid.UUID) string {
	r.t.Helper()
	return r.signInWithClaims(userID, validClaimsFor(userID))
}

// signInWithClaims is signInAs with caller-shaped JWT claims.
func (r *authRig) signInWithClaims(userID uuid.UUID, claims jwt.MapClaims) string {
	r.t.Helper()
	resp, _ := r.driveCallbackClaims(claims)
	return r.sidFromResp(resp)
}

// sidFromResp pulls the sid cookie value from a callback response, fatal on miss.
func (r *authRig) sidFromResp(resp *http.Response) string {
	r.t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sidCookieName {
			return c.Value
		}
	}
	r.t.Fatalf("response had no %s cookie (status=%d, set-cookie=%v)",
		sidCookieName, resp.StatusCode, resp.Header["Set-Cookie"])
	return ""
}

// gotSidCookie reports whether the response set a non-empty sid cookie.
func (r *authRig) gotSidCookie(resp *http.Response) bool {
	r.t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sidCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

// requestWithSid fires a request with the sid cookie + a same-origin Origin
// header (so the CSRF Origin check on mutating routes passes).
func (r *authRig) requestWithSid(method, path, sid string) *http.Response {
	r.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: sidCookieName, Value: sid})
	}
	req.Header.Set("Origin", testPublicURL)
	return r.serve(req)
}

// ---------- seeding (admin pool; bypasses RLS — fixtures) ----------

// seedUser inserts auth.users + public.users + a github identity for a fresh
// UUID, so a driveCallback resolves back to it rather than minting a fresh
// principal.
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
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $1, 'github', $2, true)`, idStr, idStr+"@test"); err != nil {
		r.t.Fatalf("seed user identity: %v", err)
	}
	return uuid.MustParse(idStr)
}

// seedAuthOnlyUser seeds an auth.users row WITHOUT a public.users row — a
// genuine first-time login, where the callback creates public.users.
func (r *authRig) seedAuthOnlyUser() uuid.UUID {
	r.t.Helper()
	var idStr string
	if err := r.h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		r.t.Fatalf("gen uuid: %v", err)
	}
	r.h.SeedAuthUser(r.t, idStr, idStr+"@corp.example")
	return uuid.MustParse(idStr)
}

// seedOrg inserts an org owned by ownerID (with an 'owner' org_membership and an
// 'admin' membership on its default team) and returns the org + team ids.
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
	return uuid.MustParse(oID), uuid.MustParse(tID)
}

// ---------- assertion helpers ----------

type meBody struct {
	ID          string `json:"id"`
	ActiveOrgID string `json:"active_org_id"`
	Orgs        []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"orgs"`
}

func (r *authRig) me(sid string) meBody {
	r.t.Helper()
	resp := r.requestWithSid("GET", "/api/me", sid)
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("/api/me status=%d, want 200", resp.StatusCode)
	}
	var m meBody
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		r.t.Fatalf("decode /api/me: %v", err)
	}
	return m
}

// membershipCount returns how many org_memberships rows a user holds (optionally
// scoped to one org when orgID != uuid.Nil).
func (r *authRig) membershipCount(userID, orgID uuid.UUID) int {
	r.t.Helper()
	var n int
	q := `SELECT count(*) FROM org_memberships WHERE user_id = $1`
	args := []any{userID}
	if orgID != uuid.Nil {
		q += ` AND org_id = $2`
		args = append(args, orgID)
	}
	if err := r.h.AdminDB.QueryRow(q, args...).Scan(&n); err != nil {
		r.t.Fatalf("count memberships: %v", err)
	}
	return n
}

// principalOf resolves a GoTrue login identity (auth.users id) to its principal
// (public.users id) via the user_identities link the callback writes.
func (r *authRig) principalOf(authUserID uuid.UUID) uuid.UUID {
	r.t.Helper()
	var pid uuid.UUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT user_id FROM user_identities WHERE auth_user_id = $1`, authUserID,
	).Scan(&pid); err != nil {
		r.t.Fatalf("resolve principal for identity %s: %v", authUserID, err)
	}
	return pid
}

// ---------- generic HTTP helpers ----------

// doReq fires a request with an optional sid cookie + a same-origin Origin
// header (so mutating routes pass the CSRF check) and an optional JSON body.
func doReq(r *authRig, method, path, sid string, body any) *http.Response {
	r.t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testPublicURL)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: sidCookieName, Value: sid})
	}
	return r.serve(req)
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	var sb strings.Builder
	_, _ = io.Copy(&sb, resp.Body)
	return sb.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
