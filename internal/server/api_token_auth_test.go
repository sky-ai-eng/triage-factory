package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/internal/apitokens"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// ---------- rig helpers ----------

// mintToken creates a live API token for (userID, orgID) and returns the
// plaintext the caller puts in an Authorization header. The store is the same
// one the middleware resolves through, so nothing here is a stub.
func (r *authRig) mintToken(userID, orgID uuid.UUID, name string, cidrs ...string) (apitokens.Token, string) {
	r.t.Helper()
	tok, plaintext, err := r.srv.authDeps.apiTokens.MintSystem(
		context.Background(), userID.String(), orgID.String(), name, nil, cidrs, userID.String())
	if err != nil {
		r.t.Fatalf("mint token: %v", err)
	}
	return tok, plaintext
}

// bearerReq builds a request carrying an Authorization header and a
// same-origin Origin, the shape a well-behaved headless client sends.
func (r *authRig) bearerReq(method, path, token string) *http.Request {
	r.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	return req
}

// serve drives the server's mux and returns the recorder, so a test can read
// both status and body without the *http.Response detour.
func (r *authRig) serve(req *http.Request) *httptest.ResponseRecorder {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec
}

// ---------- precedence and fail-closed ----------

// TestBearerAuth_TakesPrecedenceOverCookie is the rule that makes the token
// path safe to add at all: a request that names a credential is decided on that
// credential. A bad Bearer alongside a perfectly good session cookie is a 401,
// never a quiet downgrade to the ambient one.
func TestBearerAuth_TakesPrecedenceOverCookie(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "precedence-org")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	// Sanity: the cookie alone works, so a 401 below is the Bearer's doing.
	if rec := r.serve(func() *http.Request {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
		return req
	}()); rec.Code != http.StatusOK {
		t.Fatalf("cookie-only /api/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	req := r.bearerReq("GET", "/api/me", "tf_this-token-never-existed")
	req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	if rec := r.serve(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad Bearer + good cookie = %d, want 401 (the cookie must not be consulted): %s",
			rec.Code, rec.Body.String())
	}
}

// TestBearerAuth_ValidTokenWithoutCookie is the acceptance half: a minted token
// alone drives a cursor-scoped route, no cookies at any point.
func TestBearerAuth_ValidTokenWithoutCookie(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "bearer-only-org")
	_, plaintext := r.mintToken(userID, orgID, "ci")

	rec := r.serve(r.bearerReq("GET", "/api/me", plaintext))
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer-only /api/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var me struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		ActiveOrgID string `json:"active_org_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /api/me: %v (body=%s)", err, rec.Body.String())
	}
	if me.ID != userID.String() {
		t.Errorf("/api/me id = %q, want the principal %q", me.ID, userID)
	}
	if me.ActiveOrgID != orgID.String() {
		t.Errorf("/api/me active org = %q, want the token's org %q", me.ActiveOrgID, orgID)
	}
}

// TestBearerAuth_MalformedCredentialsAllRefuse walks every shape that is not a
// usable token and pins the single answer. The point is that they are
// indistinguishable: a caller learns "no" and never how close they got.
func TestBearerAuth_MalformedCredentialsAllRefuse(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "malformed-org")
	tok, plaintext := r.mintToken(userID, orgID, "revoke-me")

	if err := r.srv.authDeps.apiTokens.RevokeSystem(
		context.Background(), userID.String(), tok.ID, userID.String()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"unknown scheme", "Token " + plaintext},
		{"basic auth", "Basic dXNlcjpwYXNz"},
		{"scheme only", "Bearer"},
		{"scheme with no credential", "Bearer "},
		{"tab instead of space", "Bearer\t" + plaintext},
		{"missing tf_ prefix", "Bearer " + strings.TrimPrefix(plaintext, apitokens.Prefix)},
		{"no such token", "Bearer tf_" + strings.Repeat("A", 43)},
		{"revoked token", "Bearer " + plaintext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/me", nil)
			req.Header.Set("Authorization", tc.header)
			if rec := r.serve(req); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// Case-insensitive scheme with a single space is the one accepted spelling,
	// and it must actually work — the negatives above only mean something if
	// the positive does.
	_, freshPlaintext := r.mintToken(userID, orgID, "lowercase-scheme")
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "bEaReR "+freshPlaintext)
	if rec := r.serve(req); rec.Code != http.StatusOK {
		t.Fatalf("lowercase scheme = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// ---------- request context ----------

// TestBearerAuth_SeedsClaimsOrgAndTokenMarker pins what the token path puts in
// context: the principal and their email as claims, the token's org as the
// request's tenant, the TokenAuth marker every credential-type gate reads —
// and NO auth-identity id, because there is no GoTrue login behind a token.
func TestBearerAuth_SeedsClaimsOrgAndTokenMarker(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "ctx-token-org")
	tok, plaintext := r.mintToken(userID, orgID, "probe")

	var (
		gotSubject, gotEmail, gotOrg, gotAuthIdentity string
		gotToken                                      *httpx.TokenAuth
		gotSession                                    bool
	)
	r.srv.mux.Handle("GET /api/test/token-ctx-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if c := ClaimsFrom(req.Context()); c != nil {
				gotSubject, gotEmail = c.Subject, c.Email
			}
			gotOrg = OrgIDFrom(req.Context())
			gotAuthIdentity = httpx.AuthIdentityIDFrom(req.Context())
			gotToken = httpx.TokenAuthFrom(req.Context())
			gotSession = SessionFrom(req.Context()) != nil
			w.WriteHeader(http.StatusOK)
		})))

	if rec := r.serve(r.bearerReq("GET", "/api/test/token-ctx-probe", plaintext)); rec.Code != http.StatusOK {
		t.Fatalf("probe status = %d: %s", rec.Code, rec.Body.String())
	}

	if gotSubject != userID.String() {
		t.Errorf("claims.Subject = %q, want the principal %q", gotSubject, userID)
	}
	if gotEmail != userID.String()+"@test" {
		t.Errorf("claims.Email = %q, want the principal's identity email", gotEmail)
	}
	if gotOrg != orgID.String() {
		t.Errorf("OrgIDFrom = %q, want the token's org %q", gotOrg, orgID)
	}
	if gotAuthIdentity != "" {
		t.Errorf("AuthIdentityIDFrom = %q, want empty (no GoTrue login on this path)", gotAuthIdentity)
	}
	if gotSession {
		t.Error("SessionFrom returned a session on a token-authed request")
	}
	if gotToken == nil {
		t.Fatal("TokenAuthFrom = nil on a token-authed request")
	}
	if gotToken.TokenID != tok.ID || gotToken.OrgID != orgID.String() {
		t.Errorf("TokenAuth = %+v, want {TokenID:%s OrgID:%s}", *gotToken, tok.ID, orgID)
	}
}

// TestSessionAuth_LeavesTokenMarkerUnset is the other half: a cookie request
// must carry no TokenAuth, or every gate that reads it would fire on the wrong
// credential.
func TestSessionAuth_LeavesTokenMarkerUnset(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "session-marker-org")
	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	var seen *httpx.TokenAuth
	r.srv.mux.Handle("GET /api/test/session-marker-probe",
		r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			seen = httpx.TokenAuthFrom(req.Context())
			w.WriteHeader(http.StatusOK)
		})))

	if got := r.requestWithSid("GET", "/api/test/session-marker-probe", sid); got.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d", got.StatusCode)
	}
	if seen != nil {
		t.Errorf("TokenAuthFrom = %+v on a cookie-authed request, want nil", *seen)
	}
}

// ---------- CSRF ----------

// TestBearerAuth_SkipsCSRFOriginCheck pins the skip and its blast radius: the
// same hostile Origin that gets a cookie request rejected is irrelevant to a
// Bearer one, because a cross-site browser context cannot attach the header at
// all without a preflight this server never approves.
func TestBearerAuth_SkipsCSRFOriginCheck(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "csrf-org")
	_, plaintext := r.mintToken(userID, orgID, "csrf-probe")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	r.srv.mux.Handle("POST /api/test/csrf-probe",
		r.srv.withCSRFOriginCheck(r.srv.withSession(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))))

	const hostile = "https://evil.example"

	bearer := httptest.NewRequest("POST", "/api/test/csrf-probe", nil)
	bearer.Header.Set("Authorization", "Bearer "+plaintext)
	bearer.Header.Set("Origin", hostile)
	if rec := r.serve(bearer); rec.Code != http.StatusOK {
		t.Fatalf("Bearer + hostile Origin = %d, want 200 (origin check skipped): %s",
			rec.Code, rec.Body.String())
	}

	cookie := httptest.NewRequest("POST", "/api/test/csrf-probe", nil)
	cookie.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	cookie.Header.Set("Origin", hostile)
	if rec := r.serve(cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie + hostile Origin = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// ---------- IP allowlist ----------

// TestBearerAuth_CIDRAllowlist covers all three halves of the allowlist: an
// in-range caller passes, an out-of-range one is refused with a body byte for
// byte identical to an invalid token's (no disclosure that the secret was
// otherwise good), and a v4-mapped v6 peer — what a dual-stack listener hands
// Go — matches a plain v4 range.
func TestBearerAuth_CIDRAllowlist(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "cidr-org")
	_, plaintext := r.mintToken(userID, orgID, "fenced", "203.0.113.0/24")

	at := func(remoteAddr, token string) *httptest.ResponseRecorder {
		req := r.bearerReq("GET", "/api/me", token)
		req.RemoteAddr = remoteAddr
		return r.serve(req)
	}

	if rec := at("203.0.113.7:5555", plaintext); rec.Code != http.StatusOK {
		t.Fatalf("in-range caller = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := at("[::ffff:203.0.113.9]:5555", plaintext); rec.Code != http.StatusOK {
		t.Fatalf("v4-mapped v6 caller = %d, want 200 (must match the v4 range): %s",
			rec.Code, rec.Body.String())
	}

	outOfRange := at("198.51.100.4:5555", plaintext)
	invalidToken := at("198.51.100.4:5555", "tf_"+strings.Repeat("B", 43))
	if outOfRange.Code != http.StatusUnauthorized {
		t.Fatalf("out-of-range caller = %d, want 401: %s", outOfRange.Code, outOfRange.Body.String())
	}
	if outOfRange.Body.String() != invalidToken.Body.String() {
		t.Errorf("out-of-range body %q differs from invalid-token body %q; the refusals must be indistinguishable",
			outOfRange.Body.String(), invalidToken.Body.String())
	}

	// A token with no allowlist is unrestricted, not deny-all.
	_, open := r.mintToken(userID, orgID, "unfenced")
	if rec := at("198.51.100.4:5555", open); rec.Code != http.StatusOK {
		t.Fatalf("allowlist-free token = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestAddrInCIDRs covers the matcher's own edges without a server: the empty
// allowlist admits, an unresolvable caller does not, and a v6 range is matched
// on its own terms.
func TestAddrInCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ip    string
		cidrs []string
		want  bool
	}{
		{"empty allowlist admits anything", "198.51.100.1", nil, true},
		{"v4 in range", "10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"v4 out of range", "11.1.2.3", []string{"10.0.0.0/8"}, false},
		{"v4-mapped v6 hits a v4 range", "::ffff:10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"v6 in range", "2001:db8::5", []string{"2001:db8::/32"}, true},
		{"v6 against a v4-only allowlist", "2001:db8::5", []string{"10.0.0.0/8"}, false},
		{"host route", "192.0.2.9", []string{"192.0.2.9/32"}, true},
		{"second entry matches", "192.0.2.9", []string{"10.0.0.0/8", "192.0.2.0/24"}, true},
		{"no address fails closed", "", []string{"10.0.0.0/8"}, false},
		{"unparseable address fails closed", "not-an-ip", []string{"10.0.0.0/8"}, false},
		{"corrupt entry is skipped, not a match", "10.1.2.3", []string{"nonsense"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := addrInCIDRs(tc.ip, tc.cidrs); got != tc.want {
				t.Errorf("addrInCIDRs(%q, %v) = %v, want %v", tc.ip, tc.cidrs, got, tc.want)
			}
		})
	}
}

// TestBearerCredential pins the one accepted spelling of the header.
func TestBearerCredential(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer tf_abc", "tf_abc", true},
		{"bearer tf_abc", "tf_abc", true},
		{"BEARER tf_abc", "tf_abc", true},
		{"Bearer  tf_abc", " tf_abc", true}, // the extra space is part of the credential
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Bearer\ttf_abc", "", false},
		{"Token tf_abc", "", false},
		{"", "", false},
	} {
		got, ok := bearerCredential(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("bearerCredential(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// ---------- org scope ----------

// TestBearerAuth_OrgScopeIsSealed is the epic's central divergence from
// sessions: the same user, a member of two orgs, reaches both with a cookie and
// only the token's own org with a token. The refusal is 404, not 403 — outside
// your credential's scope, the org does not exist.
func TestBearerAuth_OrgScopeIsSealed(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "scope-org-a")
	orgB, _ := r.seedOrg(userID, "scope-org-b")
	_, plaintext := r.mintToken(userID, orgA, "a-bound")

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)

	if rec := r.serve(r.bearerReq("GET", "/api/orgs/"+orgA.String(), plaintext)); rec.Code != http.StatusOK {
		t.Fatalf("token reading its own org = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := r.serve(r.bearerReq("GET", "/api/orgs/"+orgB.String(), plaintext)); rec.Code != http.StatusNotFound {
		t.Fatalf("token reading another org = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// The same route, same user, session-authed: a session cursor is movable,
	// so it reaches every membership.
	if got := r.requestWithSid("GET", "/api/orgs/"+orgB.String(), sid); got.StatusCode != http.StatusOK {
		t.Fatalf("session reading the other org = %d, want 200", got.StatusCode)
	}
}

// TestBearerAuth_OrgScopeCoversCallerNamedOrgs guards the routes that take
// their org from a query parameter rather than the path: a token must not reach
// another org just by naming it there either.
func TestBearerAuth_OrgScopeCoversCallerNamedOrgs(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "query-org-a")
	orgB, _ := r.seedOrg(userID, "query-org-b")
	_, plaintext := r.mintToken(userID, orgA, "a-bound")

	rec := r.serve(r.bearerReq("GET", "/api/fleet/queue?org="+orgB.String(), plaintext))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("token naming another org in ?org= = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// ---------- failure rate limit ----------

// TestBearerAuth_FailureBudget pins the shape of the cap: it meters failures,
// not requests. A working token never spends from it however often it calls;
// once a stream of failures has drained it, the next request is refused before
// the lookup it would otherwise have cost.
func TestBearerAuth_FailureBudget(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "budget-org")
	_, plaintext := r.mintToken(userID, orgID, "good")

	// A deterministic two-failure budget with no refill, so the test asserts
	// the accounting rather than racing a wall clock.
	frozen := time.Now()
	limiter := newIPRateLimiter(0, 2, time.Minute)
	limiter.now = func() time.Time { return frozen }
	r.srv.tokenAuthFailureLimiter = limiter

	const peer = "192.0.2.77:4000"
	call := func(token string) *httptest.ResponseRecorder {
		req := r.bearerReq("GET", "/api/me", token)
		req.RemoteAddr = peer
		return r.serve(req)
	}

	// Successful auths must not touch the bucket.
	for i := 0; i < 5; i++ {
		if rec := call(plaintext); rec.Code != http.StatusOK {
			t.Fatalf("success #%d = %d, want 200: %s", i, rec.Code, rec.Body.String())
		}
	}

	bad := "tf_" + strings.Repeat("C", 43)
	for i := 0; i < 2; i++ {
		if rec := call(bad); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure #%d = %d, want 401 (budget not yet spent): %s", i, rec.Code, rec.Body.String())
		}
	}

	rec := call(bad)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failure past the budget = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonRateLimited {
		t.Errorf("429 reason = %+v, want RATE_LIMITED", body.Errors)
	}

	// The gate runs before the lookup, so even a valid token is refused while
	// the address is in the penalty box.
	if rec := call(plaintext); rec.Code != http.StatusTooManyRequests {
		t.Errorf("valid token from an exhausted address = %d, want 429", rec.Code)
	}
	// Another address is unaffected: the budget is per-IP.
	req := r.bearerReq("GET", "/api/me", plaintext)
	req.RemoteAddr = "192.0.2.78:4000"
	if rec := r.serve(req); rec.Code != http.StatusOK {
		t.Errorf("valid token from a different address = %d, want 200", rec.Code)
	}
}

// TestIPRateLimiter_PeekAndCharge covers the split directly: peek reports
// without spending, charge spends without reporting, and the balance floors at
// zero rather than banking a debt.
func TestIPRateLimiter_PeekAndCharge(t *testing.T) {
	frozen := time.Now()
	l := newIPRateLimiter(0, 2, time.Minute)
	l.now = func() time.Time { return frozen }

	for i := 0; i < 10; i++ {
		if ok, _ := l.peek("ip"); !ok {
			t.Fatalf("peek #%d refused; peeking must not spend", i)
		}
	}
	l.charge("ip")
	if ok, _ := l.peek("ip"); !ok {
		t.Fatal("one charge of a two-token bucket must leave budget")
	}
	l.charge("ip")
	ok, wait := l.peek("ip")
	if ok {
		t.Fatal("a spent bucket must refuse")
	}
	if wait <= 0 {
		t.Errorf("refusal wait = %v, want a positive back-off", wait)
	}
	// Overcharging must not bank a debt: with a refill, one token's worth of
	// time buys one request back, not eleven.
	for i := 0; i < 10; i++ {
		l.charge("ip")
	}
	l.rate = 1
	frozen = frozen.Add(time.Second)
	if ok, _ := l.peek("ip"); !ok {
		t.Fatal("a second of refill must restore a token; the bucket banked a debt")
	}
}

// ---------- websocket parity ----------

// TestBearerAuth_WebsocketHandshakeAndRevocation is the streaming half of
// session parity: a Bearer handshake connects (a non-browser client can set the
// header), and revoking the token closes the socket on the next revalidation
// sweep — the same bound a revoked session gets.
func TestBearerAuth_WebsocketHandshakeAndRevocation(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "ws-token-org")
	tok, plaintext := r.mintToken(userID, orgID, "streamer")

	conn := dialServerWSBearer(t, r.liveServer(t), plaintext)
	waitWSRegistered(t, r.srv.ws, conn, orgID.String())

	if err := r.srv.authDeps.apiTokens.RevokeSystem(
		context.Background(), userID.String(), tok.ID, userID.String()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	r.srv.revalidateCredentialsOnce(context.Background())

	expectServerCloseCode(t, conn, websocket.CloseSessionRevoked)
}

// dialServerWSBearer opens a websocket against the live server authenticated by
// an API token — the handshake a headless client makes, with no cookie jar in
// sight.
func dialServerWSBearer(t *testing.T, baseURL, token string) *ws.Conn {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	conn, _, err := ws.Dial(context.Background(), wsURL, &ws.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial ws with bearer: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ws.StatusNormalClosure, "") })
	return conn
}

// ---------- deprovisioning ----------

// TestOrgMemberRemove_RevokesAPITokens pins the headless half of removal
// hygiene: taking someone out of an org kills the tokens they hold in it, and
// leaves the ones they hold elsewhere alone.
func TestOrgMemberRemove_RevokesAPITokens(t *testing.T) {
	r := newAuthRig(t)
	adminID := r.seedUser()
	orgID, _ := r.seedOrg(adminID, "removal-org")

	memberID := r.seedUser()
	otherOrg, _ := r.seedOrg(memberID, "members-other-org")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		memberID, orgID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	_, inOrg := r.mintToken(memberID, orgID, "will-die")
	_, elsewhere := r.mintToken(memberID, otherOrg, "survives")

	resp, _ := r.driveCallback(adminID)
	sid := r.sidFromResp(resp)
	got := r.requestWithSid("DELETE",
		"/api/orgs/"+orgID.String()+"/members/"+memberID.String(), sid)
	if got.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(got.Body)
		t.Fatalf("remove member = %d, want 204: %s", got.StatusCode, b)
	}

	ctx := context.Background()
	if id, err := r.srv.authDeps.apiTokens.LookupSystem(ctx, inOrg); err != nil {
		t.Fatalf("lookup revoked token: %v", err)
	} else if id != nil {
		t.Error("the removed member's token in that org still authenticates")
	}
	if id, err := r.srv.authDeps.apiTokens.LookupSystem(ctx, elsewhere); err != nil {
		t.Fatalf("lookup other-org token: %v", err)
	} else if id == nil {
		t.Error("removal from one org killed the member's token in another")
	}

	// The revocation is auditable, and names both the admin who caused it and
	// the member it happened to.
	var n int
	if err := r.h.AdminDB.QueryRow(`
		SELECT COUNT(*) FROM public.access_change_log
		 WHERE org_id = $1 AND action = 'api_token_revoked'
		   AND actor_user_id = $2 AND target_user_id = $3
	`, orgID, adminID, memberID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("api_token_revoked audit rows = %d, want 1", n)
	}
}
