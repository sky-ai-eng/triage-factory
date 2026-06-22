package sso_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Verify-before-enforce SSO test. An org admin round-trips a REAL
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

// startSAMLTest drives the admin-initiated verify-test start (GET
// /api/sso/connection/test), which derives the connection from the admin's
// active org and mints a Test=true state. Returns the state cookies + the csrf
// (captured from the fake's /sso) to echo back at the callback.
func (r *authRig) startSAMLTest(adminSid string) (cookies []*http.Cookie, csrf string) {
	r.t.Helper()
	resp := r.requestWithSid("GET", "/api/sso/connection/test", adminSid)
	if resp.StatusCode != http.StatusSeeOther {
		r.t.Fatalf("test-start status=%d, want 303 (body=%s)", resp.StatusCode, readBody(resp))
	}
	return resp.Cookies(), r.fake.ssoCSRF()
}

// fireSAMLTestCallback starts a verify-test as adminSid, then drives the test
// callback with q (a `code`, or an `error`). When a code is supplied, the caller
// stages the exchange first (stageToken / failNextToken).
func (r *authRig) fireSAMLTestCallback(adminSid string, q url.Values) *http.Response {
	r.t.Helper()
	cookies, csrf := r.startSAMLTest(adminSid)
	return r.fireCallback(cookies, csrf, q)
}

// driveSAMLTest is the happy-path driver: sign in the admin, start the test,
// stage the exchange to return a token for claims, then fire the test callback.
func (r *authRig) driveSAMLTest(adminID uuid.UUID, claims jwt.MapClaims) *http.Response {
	r.t.Helper()
	sid := r.signInAs(adminID)
	r.fake.stageToken(r.signKey.mintJWT(r.t, claims))
	return r.fireSAMLTestCallback(sid, url.Values{"code": {"fake-" + uuid.NewString()}})
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

	resp := r.driveSAMLTest(owner, validSAMLClaimsFor(user, providerID))

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
	if r.gotSidCookie(resp) {
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
	resp := r.driveSAMLTest(owner, validSAMLClaimsFor(user, providerID))

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
	resp := r.driveSAMLTest(owner, validSAMLClaimsFor(user, providerID))

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

	resp := r.driveSAMLTest(owner, claims)

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
	if r.gotSidCookie(resp) {
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

	sid := r.signInAs(owner)
	r.fake.failNextToken() // GoTrue rejects the assertion (cert mismatch / bad signature)
	q := url.Values{}
	q.Set("code", "fake-"+uuid.NewString())
	resp := r.fireSAMLTestCallback(sid, q)

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
	sid := r.signInAs(owner)
	user := r.seedAuthOnlyUser()
	badClaims := validSAMLClaimsFor(user, providerID)
	badClaims["iss"] = "https://wrong-issuer.example"
	r.fake.stageToken(r.signKey.mintJWT(t, badClaims))
	q := url.Values{}
	q.Set("code", "fake-"+uuid.NewString())
	resp := r.fireSAMLTestCallback(sid, q)

	body := readBody(resp)
	if !strings.Contains(body, "SSO test failed") {
		t.Errorf("body did not render a fail result:\n%s", body)
	}
	if !strings.Contains(body, "failed verification") {
		t.Errorf("fail message did not mention verification:\n%s", body)
	}
	if r.gotSidCookie(resp) {
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
	sid := r.signInAs(owner)

	q := url.Values{}
	q.Set("error", "access_denied")
	q.Set("error_description", "SAML assertion signature mismatch")
	resp := r.fireSAMLTestCallback(sid, q)

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
// after TF posts GoTrue's /sso with the active-org connection's provider_id and
// an S256 PKCE challenge, having set a state cookie. (Test=true lives in the
// signed state, opaque to the client; the round-trip tests above prove it took.)
func TestSSOTestStart_AdminNotYetEnabled_RedirectsWithTestState(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false) // disabled — testable
	sid := r.signInAs(owner)

	r.fake.ssoLocation = "https://login.microsoftonline.com/saml?SAMLRequest=abc"
	resp := r.requestWithSid("GET", "/api/sso/connection/test", sid)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.microsoftonline.com/saml?SAMLRequest=abc" {
		t.Errorf("Location=%q, want the IdP redirect", loc)
	}
	body := r.fake.lastSSO()
	if body["provider_id"] != providerID {
		t.Errorf("provider_id posted to GoTrue=%q, want %q (the active-org connection)", body["provider_id"], providerID)
	}
	if body["code_challenge_method"] != "S256" || body["code_challenge"] == "" {
		t.Errorf("PKCE challenge not posted to GoTrue: %v", body)
	}
	var sawState bool
	for _, c := range resp.Cookies() {
		if c.Value != "" && c.Name != sidCookieName {
			sawState = true
		}
	}
	if !sawState {
		t.Error("no state cookie set on the test-start")
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

	resp := r.requestWithSid("GET", "/api/sso/connection/test", sid)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404 for a non-admin", resp.StatusCode)
	}
	if r.fake.ssoCalls() != 0 {
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
