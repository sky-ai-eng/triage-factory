package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TFAC-431 — verify-before-enforce SSO test. An org admin round-trips a REAL
// sign-in through their own (possibly not-yet-enabled) connection to confirm the
// IdP is wired up, and the callback short-circuits before any writes: no
// session, no membership, no principal, no identity link.

// ---------- helpers ----------

// seedVerifiedSSODomain inserts a VERIFIED sso_domains row for the connection
// bound to providerID in orgID, so the test path's domain-match read resolves.
// Admin pool (bypasses RLS), mirroring seedSSOConnection.
func (r *authRig) seedVerifiedSSODomain(orgID uuid.UUID, providerID, domain string) {
	r.t.Helper()
	var connID string
	if err := r.h.AdminDB.QueryRow(
		`SELECT id::text FROM sso_connections WHERE provider_id = $1 AND org_id = $2`,
		providerID, orgID,
	).Scan(&connID); err != nil {
		r.t.Fatalf("lookup connection for domain seed: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token, verified_at)
		VALUES ($1, $2, $3, 'seed-token', now())
	`, connID, orgID, domain); err != nil {
		r.t.Fatalf("seed verified domain: %v", err)
	}
}

// stubTestExchange makes gotrueExchange return a JWT minted for claims (the test
// path verifies it through the live Verifier, so a bad iss/aud still 4xx-equiv).
func (r *authRig) stubTestExchange(claims jwt.MapClaims) {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, claims)
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, "refresh-" + uuid.NewString(), time.Now().Add(time.Hour).Unix(), nil
	}
}

// fireSAMLTestCallback signs a Test=true state for stateProviderID and drives the
// callback with the given query values (a `code`, or an `error`). The caller
// stubs gotrueExchange first when a code is supplied.
func (r *authRig) fireSAMLTestCallback(stateProviderID string, q url.Values) *http.Response {
	r.t.Helper()
	csrf := "test-csrf-" + uuid.NewString()
	state := stateClaims{
		ReturnTo:     "/dashboard",
		CSRF:         csrf,
		CodeVerifier: "test-pkce-verifier",
		ProviderID:   stateProviderID,
		Test:         true,
		ExpiresAt:    time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(r.srv.deployCfg.hmacKey)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}
	q.Set("state", csrf)
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// driveSAMLTest is the happy-path driver: stub the exchange to return a token for
// claims, then drive the test callback with a code.
func (r *authRig) driveSAMLTest(claims jwt.MapClaims, stateProviderID string) *http.Response {
	r.t.Helper()
	r.stubTestExchange(claims)
	q := url.Values{}
	q.Set("code", "fake-"+uuid.NewString())
	return r.fireSAMLTestCallback(stateProviderID, q)
}

// hasSidCookie reports whether the response set a non-empty sid cookie (a
// session was minted) — the test path must never set one.
func (r *authRig) hasSidCookie(resp *http.Response) bool {
	r.t.Helper()
	name := r.srv.sidCookieName()
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}

func (r *authRig) identityLinkCount(authUserID uuid.UUID) int {
	r.t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM user_identities WHERE auth_user_id = $1`, authUserID,
	).Scan(&n); err != nil {
		r.t.Fatalf("count identities: %v", err)
	}
	return n
}

func (r *authRig) publicUserCount() int {
	r.t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM public.users`).Scan(&n); err != nil {
		r.t.Fatalf("count public.users: %v", err)
	}
	return n
}

// TestSSOTestResultRendering moved to ee/sso/handlers_test.go with
// renderSAMLTestResult.

// ---------- callback test-branch tests ----------

// A valid round-trip renders a PASS page AND writes nothing: no session cookie,
// no membership, no public.users principal, no user_identities link. This is the
// headline invariant — a test must mint nothing.
func TestSSOTest_ValidRoundTrip_PassesAndWritesNothing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	// Disabled on purpose: a test must work on a not-yet-enabled connection.
	r.seedSSOConnection(orgA, providerID, "member", false)

	user := r.seedAuthOnlyUser() // genuine first login: only auth.users exists
	usersBefore := r.publicUserCount()

	resp := r.driveSAMLTest(validSAMLClaimsFor(user, providerID), providerID)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (the test renders a result page)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type=%q, want text/html", ct)
	}
	body := readBody(resp)
	if !strings.Contains(body, "SSO test passed") {
		t.Errorf("body did not render a pass result:\n%s", body)
	}
	if !strings.Contains(body, user.String()+"@corp.example") {
		t.Errorf("body did not surface the authenticated email:\n%s", body)
	}

	// The load-bearing invariant: a test writes NOTHING.
	if r.hasSidCookie(resp) {
		t.Error("test response set a sid cookie — a test must NOT mint a session")
	}
	if got := r.identityLinkCount(user); got != 0 {
		t.Errorf("user_identities rows=%d, want 0 — a test must NOT link a principal", got)
	}
	if got := r.publicUserCount(); got != usersBefore {
		t.Errorf("public.users count %d→%d — a test must NOT mint a principal", usersBefore, got)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 0 {
		t.Errorf("org_memberships=%d, want 0 — a test must NOT JIT-provision", got)
	}
}

// When the authenticated email's domain is a VERIFIED sso_domains row for the
// connection's org, the result reports a domain match (informational — it tells
// the admin identifier-first login will also route here).
func TestSSOTest_DomainMatch_WhenVerifiedDomainExists(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	// validSAMLClaimsFor uses <id>@corp.example, so the domain is corp.example.
	r.seedVerifiedSSODomain(orgA, providerID, "corp.example")

	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLTest(validSAMLClaimsFor(user, providerID), providerID)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test passed") {
		t.Fatalf("expected a pass result:\n%s", body)
	}
	if !strings.Contains(body, "is verified for your organization") {
		t.Errorf("expected a positive domain-match line for a verified domain:\n%s", body)
	}
}

// With no verified domain, the result still PASSES (the SP-initiated/tile flow
// doesn't need a verified domain) but reports the domain as not matched.
func TestSSOTest_NoDomainMatch_StillPasses(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true) // no domain seeded

	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLTest(validSAMLClaimsFor(user, providerID), providerID)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test passed") {
		t.Fatalf("expected a pass result even without a verified domain:\n%s", body)
	}
	if !strings.Contains(body, "is not a verified domain for your organization") {
		t.Errorf("expected a negative domain-match line:\n%s", body)
	}
}

// An assertion missing the email claim → FAIL with an actionable message, and
// still no writes (the short-circuit precedes the email check anyway).
func TestSSOTest_MissingEmail_Fails(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser()
	claims := validSAMLClaimsFor(user, providerID)
	delete(claims, "email") // the classic Entra "email claim not mapped" misconfig
	delete(claims, "email_verified")

	resp := r.driveSAMLTest(claims, providerID)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	body := readBody(resp)
	if !strings.Contains(body, "SSO test failed") {
		t.Errorf("body did not render a fail result:\n%s", body)
	}
	if !strings.Contains(body, "no email claim") {
		t.Errorf("fail message did not name the missing email claim:\n%s", body)
	}
	if r.hasSidCookie(resp) {
		t.Error("a failed test set a sid cookie")
	}
	if got := r.identityLinkCount(user); got != 0 {
		t.Errorf("user_identities rows=%d, want 0", got)
	}
}

// A failed code exchange (GoTrue rejected the assertion — cert mismatch / bad
// signature) → FAIL with a useful message.
func TestSSOTest_ExchangeError_Fails(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return "", "", 0, context.DeadlineExceeded // any error stands in for a rejected assertion
	}
	q := url.Values{}
	q.Set("code", "fake-"+uuid.NewString())
	resp := r.fireSAMLTestCallback(providerID, q)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test failed") {
		t.Errorf("body did not render a fail result:\n%s", body)
	}
	if !strings.Contains(body, "signing certificate") {
		t.Errorf("fail message did not hint at the likely cause:\n%s", body)
	}
}

// A successful exchange that hands back a token the verifier rejects (here a
// wrong-issuer JWT — stands in for a rotated / mismatched signing key) → FAIL.
// Guards the verify-after-exchange branch end-to-end, not just via the renderer,
// and confirms the short-circuit still holds past the exchange (no writes).
func TestSSOTest_TokenVerifyFails_Fails(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	// Exchange succeeds but returns a JWT the live Verifier rejects: a valid
	// signature over the wrong issuer. (A garbage string would also fail, but a
	// well-formed-yet-untrusted token exercises the verifier's claim checks.)
	user := r.seedAuthOnlyUser()
	badClaims := validSAMLClaimsFor(user, providerID)
	badClaims["iss"] = "https://wrong-issuer.example"
	token := r.signKey.mintJWT(t, badClaims)
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, "refresh-" + uuid.NewString(), time.Now().Add(time.Hour).Unix(), nil
	}
	q := url.Values{}
	q.Set("code", "fake-"+uuid.NewString())
	resp := r.fireSAMLTestCallback(providerID, q)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test failed") {
		t.Errorf("body did not render a fail result:\n%s", body)
	}
	if !strings.Contains(body, "failed verification") {
		t.Errorf("fail message did not mention verification:\n%s", body)
	}
	if r.hasSidCookie(resp) {
		t.Error("a failed test set a sid cookie")
	}
	if got := r.identityLinkCount(user); got != 0 {
		t.Errorf("user_identities rows=%d, want 0", got)
	}
}

// GoTrue's ACS redirects back with ?error=&error_description= when it rejects the
// assertion → FAIL surfacing the IdP's description.
func TestSSOTest_ACSErrorParam_Fails(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	q := url.Values{}
	q.Set("error", "access_denied")
	q.Set("error_description", "SAML assertion signature mismatch")
	resp := r.fireSAMLTestCallback(providerID, q)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test failed") {
		t.Errorf("body did not render a fail result:\n%s", body)
	}
	if !strings.Contains(body, "SAML assertion signature mismatch") {
		t.Errorf("fail message did not surface the IdP error_description:\n%s", body)
	}
}

// ---------- test-start endpoint tests ----------

// An org admin starting a test on a NOT-yet-enabled connection → 303 to the IdP,
// with TF's signed state carrying Test=true + the connection's provider_id.
func TestSSOTestStart_AdminNotYetEnabled_RedirectsWithTestState(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false) // disabled — testable
	sid := r.signInAs(owner)

	var gotProvider, gotChallenge string
	r.srv.authDeps.gotrueSSO = func(ctx context.Context, pid, redirectTo, challenge string) (string, error) {
		gotProvider, gotChallenge = pid, challenge
		return "https://login.microsoftonline.com/saml?SAMLRequest=abc", nil
	}

	resp := r.requestWithSid("GET", "/api/sso/connection/test", sid)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.microsoftonline.com/saml?SAMLRequest=abc" {
		t.Errorf("Location=%q, want the IdP redirect", loc)
	}
	if gotProvider != providerID {
		t.Errorf("provider_id posted to GoTrue=%q, want %q (the active-org connection)", gotProvider, providerID)
	}

	var stateVal string
	for _, c := range resp.Cookies() {
		if c.Name == stateCookieName {
			stateVal = c.Value
		}
	}
	if stateVal == "" {
		t.Fatal("no state cookie set")
	}
	sc, err := parseStateCookie(stateVal, r.srv.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("parse state cookie: %v", err)
	}
	if !sc.Test {
		t.Error("state cookie did not carry Test=true")
	}
	if sc.ProviderID != providerID {
		t.Errorf("state provider_id=%q, want %q", sc.ProviderID, providerID)
	}
	if gotChallenge != pkceChallenge(sc.CodeVerifier) {
		t.Error("challenge posted to GoTrue is not S256(verifier in state)")
	}
}

// A non-admin member of the org → 404 (non-disclosure), and TF never calls GoTrue.
func TestSSOTestStart_NonAdmin_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	// A plain member (has an active org, but is not an admin).
	member := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		member, orgA); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	sid := r.signInAs(member)

	called := false
	r.srv.authDeps.gotrueSSO = func(ctx context.Context, _, _, _ string) (string, error) {
		called = true
		return "", nil
	}

	resp := r.requestWithSid("GET", "/api/sso/connection/test", sid)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404 for a non-admin", resp.StatusCode)
	}
	if called {
		t.Error("GoTrue /sso was called for a non-admin")
	}
}

// An admin whose org has no connection → 404 (nothing to test).
func TestSSOTestStart_NoConnection_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	owner := r.seedUser()
	r.seedOrg(owner, "acme") // no seedSSOConnection
	sid := r.signInAs(owner)

	resp := r.requestWithSid("GET", "/api/sso/connection/test", sid)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404 when no connection is registered", resp.StatusCode)
	}
}

// Local mode (and an unwired multi deploy) has no auth stack → 404 ("feature
// absent"), exercised directly via the authDeps==nil guard.
func TestSSOTestStart_NoAuthStack_404(t *testing.T) {
	// SSO is an ee feature gated on the `sso` entitlement; a local-mode test
	// server mounts no SSO routes, so the test-start route 404s — the same
	// "feature absent" contract the old authDeps==nil guard enforced, observed
	// at the route boundary. (The handler-level AuthReady guard moved to ee.)
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodGet, "/api/sso/connection/test", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404 when SSO/auth stack is unavailable (local mode)", rec.Code)
	}
}
