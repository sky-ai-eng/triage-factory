package sso_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SP-initiated SAML login + JIT provisioning + cross-org isolation.
//
// NOTE on account merge: one might expect "GitHub-then-Entra, same verified
// email → one auth.users". Investigation of GoTrue v2.189.0 source established
// that this does NOT happen — SSO is excluded from GoTrue's account linking
// (an `sso:` provider's linking domain is the provider itself; the
// email-grouping path filters is_sso_user=false), so a SAML login is ALWAYS
// its own auth.users. There is therefore no TF-side merge to test or build;
// claims.Subject is the SAML user's own id. The relevant guard we DO assert is
// that a SAML login writes no github identity row
// (TestSSOLogin_WritesNoGitHubIdentityRow).
//
// The other consequence: TF never reads the GoTrue JWT to discover which SSO
// provider this login used (provider / app_metadata / amr are GoTrue-internal +
// version-fragile). It carries the provider_id in its OWN signed state cookie
// (start → callback). The adversarial isolation test below proves TF ignores a
// mismatched app_metadata provider in the JWT and keys only on its state.

// ---------- SSO-login test helpers (build on the black-box rig in rig_test.go) ----------

// seedSSOConnection inserts an sso_connections row binding providerID → orgID.
// Mirrors what the connection handler writes after GoTrue registers the provider,
// without standing up GoTrue. role "" falls to the schema default ('member').
func (r *authRig) seedSSOConnection(orgID uuid.UUID, providerID, role string, enabled bool) {
	r.t.Helper()
	if role == "" {
		role = "member"
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO sso_connections (org_id, provider_id, default_role, enabled)
		VALUES ($1, $2, $3::public.org_role, $4)
	`, orgID, providerID, role, enabled); err != nil {
		r.t.Fatalf("seed sso_connection: %v", err)
	}
}

// setSSOConnectionEnabled flips the enabled flag — used to disable a connection
// in the window between the SAML start (which mints the state cookie) and the
// callback, so the callback's enable re-check can be exercised black-box.
func (r *authRig) setSSOConnectionEnabled(providerID string, enabled bool) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(
		`UPDATE sso_connections SET enabled = $1 WHERE provider_id = $2`, enabled, providerID); err != nil {
		r.t.Fatalf("set connection enabled=%v: %v", enabled, err)
	}
}

// deleteSSOConnection removes the connection — models a binding that vanished
// between the SAML start and the callback.
func (r *authRig) deleteSSOConnection(providerID string) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(
		`DELETE FROM sso_connections WHERE provider_id = $1`, providerID); err != nil {
		r.t.Fatalf("delete connection: %v", err)
	}
}

// validSAMLClaimsFor returns JWT claims for a SAML login. jwtProviderID drives
// the app_metadata.provider value (a real Entra login carries "sso:<id>") — TF
// must IGNORE it and key on its signed state, so the isolation test deliberately
// passes a value that differs from the state's provider_id. user_metadata
// carries a preferred_username (an Entra UPN, NOT a github handle) to prove
// upsertUserFromClaims won't mint a github identity row from it.
func validSAMLClaimsFor(userID uuid.UUID, jwtProviderID string) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":            userID.String(),
		"email":          userID.String() + "@corp.example",
		"email_verified": true, // SAML assertion emails are verified by definition
		"iss":            testIssuer,
		"aud":            testAudience,
		"exp":            time.Now().Add(1 * time.Hour).Unix(),
		"iat":            time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "sso:" + jwtProviderID,
			"providers": []any{"sso:" + jwtProviderID},
		},
		"user_metadata": map[string]any{
			"preferred_username": userID.String() + "@corp.example",
			"full_name":          "SAML User",
		},
	}
}

// startSAML drives the SP-initiated SAML start for providerID (the binding must
// be enabled — that's the product gate), returning the cookies the server set
// (the signed state cookie, carrying providerID) and the csrf to echo back. The
// rig follows the server's own cookies, so it never hand-signs state.
func (r *authRig) startSAML(providerID string) (cookies []*http.Cookie, csrf string) {
	r.t.Helper()
	return r.startLogin(ssoStartURL(providerID, "/dashboard"), true)
}

// completeSAMLLogin finishes a SAML login at the callback: stage the JWT the
// exchange returns (with jwtProviderID in app_metadata) and replay the start's
// cookies. Split from startSAML so a test can mutate connection state in the
// window between start and callback.
func (r *authRig) completeSAMLLogin(cookies []*http.Cookie, csrf string, userID uuid.UUID, jwtProviderID string) *http.Response {
	r.t.Helper()
	r.fake.stageToken(r.signKey.mintJWT(r.t, validSAMLClaimsFor(userID, jwtProviderID)))
	return r.fireCallback(cookies, csrf, url.Values{"code": {"fake-" + uuid.NewString()}})
}

// driveSAMLCallback completes a full SAML login: SP-initiated start (state cookie
// carries stateProviderID — the value the JIT keys on) then the callback (JWT
// advertises jwtProviderID in app_metadata; pass the same value for an "honest"
// login). Returns the callback response (carrying the sid Set-Cookie).
func (r *authRig) driveSAMLCallback(userID uuid.UUID, stateProviderID, jwtProviderID string) *http.Response {
	r.t.Helper()
	cookies, csrf := r.startSAML(stateProviderID)
	return r.completeSAMLLogin(cookies, csrf, userID, jwtProviderID)
}

// meBody, me, membershipCount, principalOf live on the rig in rig_test.go.

// accessChangeRow is one access_change_log row read back in a test — the columns
// the JIT audit assertions care about (NULLs folded to "").
type accessChangeRow struct {
	actor, target, team, detail string
}

// accessChangeRows reads orgID's access_change_log rows of the given action on
// the admin pool (the test has no claims; admin bypasses RLS), newest-first. The
// JIT audit assertions count these to prove a net-new login writes exactly one
// row and a returning login writes none.
func (r *authRig) accessChangeRows(orgID uuid.UUID, action string) []accessChangeRow {
	r.t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT COALESCE(actor_user_id::text, ''), COALESCE(target_user_id::text, ''),
		       COALESCE(team_id::text, ''), COALESCE(detail_json, '')
		  FROM access_change_log
		 WHERE org_id = $1 AND action = $2
		 ORDER BY created_at DESC, id DESC
	`, orgID, action)
	if err != nil {
		r.t.Fatalf("query access_change_log: %v", err)
	}
	defer rows.Close()
	var out []accessChangeRow
	for rows.Next() {
		var a accessChangeRow
		if err := rows.Scan(&a.actor, &a.target, &a.team, &a.detail); err != nil {
			r.t.Fatalf("scan access_change_log: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("iterate access_change_log: %v", err)
	}
	return out
}

// ---------- callback / JIT tests ----------

// Existing member via SAML → session, lands in their org.
func TestSSOLogin_ExistingMember_LandsInOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, orgA); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q", me.ActiveOrgID, orgA)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 1 {
		t.Errorf("memberships=%d, want 1 (grant must be idempotent for an existing member)", got)
	}
	if len(me.Orgs) != 1 || me.Orgs[0].Role != "member" {
		t.Errorf("orgs=%+v, want one org with role member (JIT grants, never re-roles)", me.Orgs)
	}
}

// New user via SAML (provider bound to org A) → JIT 'member' in org A, lands in
// the app (active org set), not onboarding.
func TestSSOLogin_NewUser_JITProvisionsMember(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser() // genuine first login: no public.users yet
	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	principal := r.principalOf(user)
	if got := r.membershipCount(principal, uuid.Nil); got != 1 {
		t.Fatalf("memberships=%d, want exactly 1", got)
	}
	var gotOrg, gotRole string
	if err := r.h.AdminDB.QueryRow(
		`SELECT org_id::text, role FROM org_memberships WHERE user_id=$1`, principal,
	).Scan(&gotOrg, &gotRole); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if gotOrg != orgA.String() || gotRole != "member" {
		t.Errorf("membership = (%s, %s), want (%s, member)", gotOrg, gotRole, orgA)
	}

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q (a JIT'd user must land in the app, not onboarding)", me.ActiveOrgID, orgA)
	}
}

// A net-new SSO login (JIT auto-provision) writes exactly one org_member_granted
// audit row: actor == target (the user joined themselves, the self-grant via the
// org's SSO domain binding), team_id NULL (org-only), detail
// {"source":"sso_jit","role":"member"}. This is the TFAC-471 gap TFAC-486 closes —
// JIT is the SSO analog of the audited invite-accept.
func TestSSOLogin_JIT_WritesAccessChangeLog(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser() // genuine first login
	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	principal := r.principalOf(user)

	rows := r.accessChangeRows(orgA, domain.AccessActionOrgMemberGranted)
	if len(rows) != 1 {
		t.Fatalf("org_member_granted rows=%d, want exactly 1 (the net-new JIT grant)", len(rows))
	}
	got := rows[0]
	if got.actor != principal.String() || got.target != principal.String() {
		t.Errorf("actor/target = %s/%s, want %s/%s (self-grant)", got.actor, got.target, principal, principal)
	}
	if got.team != "" {
		t.Errorf("team_id = %q, want empty (JIT grants org-only)", got.team)
	}
	var d struct {
		Source string `json:"source"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal([]byte(got.detail), &d); err != nil {
		t.Fatalf("detail_json %q not valid JSON: %v", got.detail, err)
	}
	if d.Source != domain.AccessSourceSSOJIT {
		t.Errorf("detail source=%q, want %q", d.Source, domain.AccessSourceSSOJIT)
	}
	if d.Role != "member" {
		t.Errorf("detail role=%q, want member (the binding's default role)", d.Role)
	}
}

// A returning SSO login (already a member) writes NO new audit row — the net-new
// gate. JIT fires on every SSO login and grants via ON CONFLICT DO NOTHING, so an
// unconditional record would log "joined" on each login; the orgRowInserted gate
// keeps the log to the one real grant. TFAC-486.
func TestSSOLogin_JIT_ReturningMember_NoAccessChangeLog(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	// Already a member before the SSO login — the grant will be a no-op.
	user := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, orgA); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}

	if rows := r.accessChangeRows(orgA, domain.AccessActionOrgMemberGranted); len(rows) != 0 {
		t.Errorf("org_member_granted rows=%d, want 0 (returning member — the net-new gate)", len(rows))
	}
}

// Isolation: a provider bound to org A never provisions into org B — even when
// the JWT's app_metadata advertises org B's provider. The JIT keys on TF's
// signed state (the provider_id TF itself chose at start), not on anything the
// assertion carries.
func TestSSOLogin_Isolation_ProviderBoundToANeverProvisionsB(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerA := uuid.NewString()
	providerB := uuid.NewString()
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "org-a")
	ownerB := r.seedUser()
	orgB, _ := r.seedOrg(ownerB, "org-b")
	r.seedSSOConnection(orgA, providerA, "member", true)
	r.seedSSOConnection(orgB, providerB, "member", true)

	// Authenticate through A's provider (state=providerA) while the JWT lies and
	// claims B's provider in app_metadata.
	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLCallback(user, providerA, providerB)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	principal := r.principalOf(user)
	if got := r.membershipCount(principal, orgA); got != 1 {
		t.Errorf("org A memberships=%d, want 1", got)
	}
	if got := r.membershipCount(principal, orgB); got != 0 {
		t.Errorf("LEAK: org B memberships=%d, want 0 — A's provider provisioned into B", got)
	}
	if me := r.me(sid); me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q (TF must trust its state, not the JWT app_metadata)", me.ActiveOrgID, orgA)
	}
}

// SSO login routes to the SSO org even when the user already belongs to an
// earlier org — the active org is the org whose IdP they came through, not the
// earliest-membership default a non-SSO login would pick.
func TestSSOLogin_ActiveOrgIsSSOOrg_NotEarliestMembership(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerA := uuid.NewString()
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "sso-org")
	r.seedSSOConnection(orgA, providerA, "member", true)

	user := r.seedUser()
	ownerX := r.seedUser()
	orgX, _ := r.seedOrg(ownerX, "other-org")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, orgX); err != nil {
		t.Fatalf("seed prior membership: %v", err)
	}

	resp := r.driveSAMLCallback(user, providerA, providerA)
	sid := r.sidFromResp(resp)

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want SSO org %q (not earliest membership %q)", me.ActiveOrgID, orgA, orgX)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 2 {
		t.Errorf("memberships=%d, want 2 (kept org X, added org A)", got)
	}
}

// New user via SAML whose provider_id has no TF connection → no provisioning,
// falls through to onboarding (active org omitted). Guards the case where the
// connection vanished between start and callback (start would otherwise 404).
func TestSSOLogin_NoMatchingConnection_FallsToOnboarding(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	// Start needs an enabled binding to mint the state; then the connection
	// vanishes before the callback runs — exactly the modelled race.
	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "vanish")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser()
	cookies, csrf := r.startSAML(providerID)
	r.deleteSSOConnection(providerID) // binding gone between start and callback
	resp := r.completeSAMLLogin(cookies, csrf, user, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	if got := r.membershipCount(r.principalOf(user), uuid.Nil); got != 0 {
		t.Errorf("memberships=%d, want 0 (no accidental provisioning without a binding)", got)
	}
	me := r.me(sid)
	if me.ActiveOrgID != "" {
		t.Errorf("active_org_id=%q, want empty (zero-membership onboarding)", me.ActiveOrgID)
	}
}

// A disabled connection provisions nothing (login still succeeds; the user just
// isn't JIT'd).
func TestSSOLogin_DisabledConnection_NoProvisioning(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	// Enabled at start (so the state mints), disabled before the callback — the
	// callback's enable re-check must then refuse to JIT.
	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser()
	cookies, csrf := r.startSAML(providerID)
	r.setSSOConnectionEnabled(providerID, false) // disabled between start and callback
	resp := r.completeSAMLLogin(cookies, csrf, user, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	if got := r.membershipCount(r.principalOf(user), uuid.Nil); got != 0 {
		t.Errorf("memberships=%d, want 0 (a disabled connection must not JIT)", got)
	}
}

// A SAML login writes no user_github_identities row — even when the assertion
// carries a preferred_username (which is a UPN, not a github handle).
func TestSSOLogin_WritesNoGitHubIdentityRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser()
	r.driveSAMLCallback(user, providerID, providerID)

	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM user_github_identities WHERE user_id = $1`, r.principalOf(user),
	).Scan(&n); err != nil {
		t.Fatalf("count github identities: %v", err)
	}
	if n != 0 {
		t.Errorf("user_github_identities rows=%d, want 0 for a SAML login", n)
	}
}

// A raw IdP-initiated assertion (no TF state cookie) is rejected at the callback
// — reinforces SP-initiated-only.
func TestSSOLogin_RawIdPInitiated_NoStateCookie_Rejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	resp := r.serve(httptest.NewRequest("GET", "/api/auth/callback?state=x&code=y", nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (missing state cookie → reject)", resp.StatusCode)
	}
}

// ---------- start-endpoint tests ----------

// GET /api/auth/oauth/saml?provider_id=… → 303 to the IdP GoTrue returned, after
// TF posted GoTrue's /sso with the provider_id, a callback redirect_to, and an
// S256 PKCE challenge. (The signed state cookie's internals are opaque to the
// client by design; that the provider_id + PKCE verifier survive start→callback
// is proven transitively by the JIT round-trip tests below.)
func TestSAMLStart_RedirectsToIdP(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	r.fake.ssoLocation = "https://login.microsoftonline.com/saml?SAMLRequest=abc"
	resp := r.serve(httptest.NewRequest("GET",
		"/api/auth/oauth/saml?provider_id="+providerID+"&return_to=/dashboard", nil))

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.microsoftonline.com/saml?SAMLRequest=abc" {
		t.Errorf("Location=%q, want the IdP redirect GoTrue returned", loc)
	}

	body := r.fake.lastSSO()
	if body["provider_id"] != providerID {
		t.Errorf("provider_id posted to GoTrue=%q, want %q", body["provider_id"], providerID)
	}
	wantPrefix := testPublicURL + "/api/auth/callback?state="
	if !strings.HasPrefix(body["redirect_to"], wantPrefix) {
		t.Errorf("redirect_to=%q, want prefix %q", body["redirect_to"], wantPrefix)
	}
	if body["code_challenge_method"] != "S256" {
		t.Errorf("code_challenge_method=%q, want S256", body["code_challenge_method"])
	}
	if body["code_challenge"] == "" {
		t.Error("no PKCE code_challenge posted to GoTrue")
	}

	// The signed state cookie was set (its contents are opaque to the client).
	var sawState bool
	for _, c := range resp.Cookies() {
		if c.Value != "" && c.Name != sidCookieName {
			sawState = true
		}
	}
	if !sawState {
		t.Error("no state cookie set on the SAML start")
	}
}

// An unknown, disabled, or malformed provider_id → 404, and TF never posts GoTrue.
func TestSAMLStart_UnknownOrDisabledProvider_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	// Unknown provider_id (no row at all).
	if resp := r.serve(httptest.NewRequest("GET",
		"/api/auth/oauth/saml?provider_id="+uuid.NewString(), nil)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider status=%d, want 404", resp.StatusCode)
	}

	// Disabled connection.
	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false)
	if resp := r.serve(httptest.NewRequest("GET",
		"/api/auth/oauth/saml?provider_id="+providerID, nil)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("disabled provider status=%d, want 404", resp.StatusCode)
	}

	// A malformed (non-UUID) provider_id is an opaque text key that matches no row
	// → 404, NOT a DB cast error / 500.
	if resp := r.serve(httptest.NewRequest("GET",
		"/api/auth/oauth/saml?provider_id=not-a-uuid", nil)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("malformed provider status=%d, want 404 (opaque text key, no cast)", resp.StatusCode)
	}

	if r.fake.ssoCalls() != 0 {
		t.Errorf("GoTrue /sso calls=%d, want 0 (never called for unknown/disabled/malformed)", r.fake.ssoCalls())
	}
}

// Missing provider_id → 400.
func TestSAMLStart_MissingProviderID_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	if resp := r.serve(httptest.NewRequest("GET", "/api/auth/oauth/saml", nil)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
}

// Verified-email linking: two DISTINCT GoTrue login identities carrying the same
// verified email must resolve to ONE principal. Drives the full callback, so it
// also guards the verifier's email_verified extraction: if that regressed to
// always-false, no link would happen and the identities would fork into separate
// principals, failing here. Both legs are GitHub callbacks to isolate the
// verifier + link logic; the real cross-provider GitHub→SAML round-trip (where
// the second leg also skips the github-identity write) is
// TestSSOLogin_SAMLEmailLinksExistingGitHubPrincipal.
func TestSSOLogin_VerifiedEmailLinksToOnePrincipal(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	const email = "shared@corp.example"
	authA := r.seedAuthOnlyUser()
	authB := r.seedAuthOnlyUser()

	withVerifiedEmail := func(id uuid.UUID) jwt.MapClaims {
		c := validClaimsFor(id)
		c["email"] = email
		c["email_verified"] = true
		return c
	}

	if resp, _ := r.driveCallbackClaims(withVerifiedEmail(authA)); resp.StatusCode != http.StatusFound {
		t.Fatalf("login A status=%d, want 302", resp.StatusCode)
	}
	if resp, _ := r.driveCallbackClaims(withVerifiedEmail(authB)); resp.StatusCode != http.StatusFound {
		t.Fatalf("login B status=%d, want 302", resp.StatusCode)
	}
	if pA, pB := r.principalOf(authA), r.principalOf(authB); pA != pB {
		t.Fatalf("identities with the same verified email resolved to principals %s and %s — want one", pA, pB)
	}

	// An UNVERIFIED matching email must NOT link — a separate principal (the safe
	// default; we never merge a human on an unverified claim).
	authC := r.seedAuthOnlyUser()
	cC := validClaimsFor(authC)
	cC["email"] = email
	cC["email_verified"] = false
	if resp, _ := r.driveCallbackClaims(cC); resp.StatusCode != http.StatusFound {
		t.Fatalf("login C status=%d, want 302", resp.StatusCode)
	}
	if r.principalOf(authC) == r.principalOf(authA) {
		t.Fatal("unverified matching email linked to the verified principal — must stay separate")
	}
}

// The real cross-provider round-trip through the SAML HTTP path: a human logs in
// via GitHub (minting a principal), then via Entra SAML with the same VERIFIED
// email — the SAML callback must LINK to the existing GitHub principal (not mint
// a second) and JIT that principal into the SSO org. Guards the SAML-specific
// link path the github-on-both test above can't reach.
func TestSSOLogin_SAMLEmailLinksExistingGitHubPrincipal(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	// validSAMLClaimsFor uses <id>@corp.example as the verified email; reuse that
	// exact address for the prior GitHub login so the SAML login links to it.
	samlAuth := r.seedAuthOnlyUser()
	sharedEmail := samlAuth.String() + "@corp.example"

	githubAuth := r.seedAuthOnlyUser()
	gh := validClaimsFor(githubAuth)
	gh["email"] = sharedEmail
	gh["email_verified"] = true
	if resp, _ := r.driveCallbackClaims(gh); resp.StatusCode != http.StatusFound {
		t.Fatalf("github login status=%d, want 302", resp.StatusCode)
	}
	principal := r.principalOf(githubAuth)

	if resp := r.driveSAMLCallback(samlAuth, providerID, providerID); resp.StatusCode != http.StatusFound {
		t.Fatalf("saml login status=%d, want 302", resp.StatusCode)
	}
	if got := r.principalOf(samlAuth); got != principal {
		t.Fatalf("SAML login minted a new principal %s — want it linked to the existing GitHub principal %s", got, principal)
	}
	// The linked principal (not a fresh one) is what gets JIT'd into the SSO org.
	if got := r.membershipCount(principal, orgA); got != 1 {
		t.Errorf("linked principal memberships in the SSO org=%d, want 1", got)
	}
}
