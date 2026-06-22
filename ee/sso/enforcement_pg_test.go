package sso_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SSO enforcement — "Require SSO" per-org toggle with break-glass.
//
// The login-path tests prove the enforcement decision: a non-SSO (GitHub) login
// whose VERIFIED email is on an enforced verified domain is rejected unless the
// principal is break-glass; off-domain and non-enforced logins are untouched.
// The handler tests prove the toggle's lockout-safety gating + the
// can't-remove-the-last-break-glass guard.

// ---------- enforcement test helpers ----------

// connIDForProvider returns the seeded connection's id (sso_domains FKs the
// (connection_id, org_id) pair, so a verified-domain seed needs it).
func (r *authRig) connIDForProvider(providerID string) string {
	r.t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(
		`SELECT id::text FROM sso_connections WHERE provider_id = $1`, providerID,
	).Scan(&id); err != nil {
		r.t.Fatalf("lookup connection id for provider %s: %v", providerID, err)
	}
	return id
}

// seedVerifiedDomain inserts a VERIFIED sso_domains row routing `dom` to the
// connection bound to providerID.
func (r *authRig) seedVerifiedDomain(orgID uuid.UUID, providerID, dom string) {
	r.t.Helper()
	connID := r.connIDForProvider(providerID)
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token, verified_at)
		VALUES ($1, $2, $3, 'tok-'||$3, now())
	`, connID, orgID, dom); err != nil {
		r.t.Fatalf("seed verified domain %s: %v", dom, err)
	}
}

// setConnectionEnforced flips the enforced flag on the provider's connection
// (only that flag — see markConnectionTested for the tested signal).
func (r *authRig) setConnectionEnforced(providerID string, enforced bool) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		UPDATE sso_connections SET enforced = $1 WHERE provider_id = $2
	`, enforced, providerID); err != nil {
		r.t.Fatalf("set connection enforced=%v: %v", enforced, err)
	}
}

// markConnectionTested stamps last_tested_at so the connection looks like one
// that has passed a verify-before-enforce Test — the enforce toggle gates on it.
func (r *authRig) markConnectionTested(providerID string) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		UPDATE sso_connections SET last_tested_at = now() WHERE provider_id = $1
	`, providerID); err != nil {
		r.t.Fatalf("mark connection tested: %v", err)
	}
}

// addBreakGlass designates a principal as break-glass for the org (the recovery
// path under enforcement).
func (r *authRig) addBreakGlass(orgID, userID uuid.UUID) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO sso_break_glass (org_id, user_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, orgID, userID); err != nil {
		r.t.Fatalf("add break-glass: %v", err)
	}
}

// driveGitHubLogin drives a GitHub OAuth callback for authUserID carrying a
// specific VERIFIED email — the shape the enforcement check keys on.
func (r *authRig) driveGitHubLogin(authUserID uuid.UUID, email string) *http.Response {
	r.t.Helper()
	c := validClaimsFor(authUserID)
	c["email"] = email
	c["email_verified"] = true
	resp, _ := r.driveCallbackClaims(c)
	return resp
}

// rejectedForSSO reports whether the callback bounced to the SSO-required login
// page. Matched by prefix because the redirect also carries the original
// return_to (e.g. /login?error=sso_required&return_to=%2Fdashboard).
func rejectedForSSO(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Location"), "/login?error=sso_required")
}

// ---------- login-path enforcement tests ----------

// An enforced verified domain rejects a GitHub login whose verified email is on
// it: 302 to the SSO-required login page, no session minted.
func TestSSOEnforcement_RejectsGitHubOnEnforcedDomain(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	user := r.seedAuthOnlyUser()
	resp := r.driveGitHubLogin(user, "alice@corp.example")

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302 (reject → login)", resp.StatusCode)
	}
	if !rejectedForSSO(resp) {
		t.Errorf("Location=%q, want an /login?error=sso_required bounce", resp.Header.Get("Location"))
	}
	if r.gotSidCookie(resp) {
		t.Error("a session was minted for an enforced non-SSO login — must be rejected")
	}
}

// The break-glass principal still gets in via GitHub even under enforcement —
// the recovery path so they can un-enforce if the IdP breaks.
func TestSSOEnforcement_BreakGlassGitHubAllowed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgA, owner) // owner is the designated recovery principal

	// The owner signs in via GitHub with an on-domain verified email.
	resp := r.driveGitHubLogin(owner, "owner@corp.example")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302", resp.StatusCode)
	}
	if rejectedForSSO(resp) {
		t.Error("break-glass principal was rejected — must retain GitHub access under enforcement")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted for the break-glass principal")
	}
}

// An off-domain login (e.g. an invited external @gmail.com) is unaffected by an
// enforced domain — enforcement is domain-scoped.
func TestSSOEnforcement_ExternalDomainUnaffected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	user := r.seedAuthOnlyUser()
	resp := r.driveGitHubLogin(user, "bob@gmail.com") // off the enforced domain

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302", resp.StatusCode)
	}
	if rejectedForSSO(resp) {
		t.Error("an off-domain login was rejected — enforcement must be domain-scoped")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted for the off-domain login")
	}
}

// A verified domain on a NON-enforced connection still allows GitHub login (the
// domain routes SSO when chosen, but does not forbid GitHub).
func TestSSOEnforcement_NotEnforcedAllowsGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	// NOT enforced.

	user := r.seedAuthOnlyUser()
	resp := r.driveGitHubLogin(user, "alice@corp.example")

	if rejectedForSSO(resp) {
		t.Error("GitHub login rejected on a non-enforced domain")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted on a non-enforced domain")
	}
}

// Un-enforcing restores non-SSO login: the same on-domain GitHub login that was
// rejected succeeds once enforcement is turned off.
func TestSSOEnforcement_UnenforceRestoresGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	user := r.seedAuthOnlyUser()
	if !rejectedForSSO(r.driveGitHubLogin(user, "alice@corp.example")) {
		t.Fatalf("precondition: enforced login was not rejected")
	}

	r.setConnectionEnforced(providerID, false)
	resp := r.driveGitHubLogin(user, "alice@corp.example")
	if rejectedForSSO(resp) {
		t.Error("login still rejected after un-enforce")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted after un-enforce")
	}
}

// A DISABLED connection never enforces, even with enforced=true on the row —
// blocking GitHub when SSO isn't actually available would lock the domain out.
func TestSSOEnforcement_DisabledConnectionDoesNotEnforce(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false) // disabled
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	user := r.seedAuthOnlyUser()
	resp := r.driveGitHubLogin(user, "alice@corp.example")
	if rejectedForSSO(resp) {
		t.Error("a disabled connection enforced login — fail-open expected when SSO is unavailable")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted for a disabled-connection login")
	}
}

// An UNVERIFIED on-domain email is not enforced (the rule is verified-email
// only — an unverified claim can't prove domain membership).
func TestSSOEnforcement_UnverifiedEmailNotEnforced(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	user := r.seedAuthOnlyUser()
	c := validClaimsFor(user)
	c["email"] = "alice@corp.example"
	c["email_verified"] = false // unverified
	resp, _ := r.driveCallbackClaims(c)
	if rejectedForSSO(resp) {
		t.Error("an unverified on-domain login was enforced — only verified emails count")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted for the unverified-email login")
	}
}

// ---------- toggle gating tests ----------

// The enforce toggle is unavailable until the connection is enabled + tested +
// has a verified domain. Each missing precondition yields a 409 with an
// actionable message; with all three present the toggle succeeds and seeds the
// owner as break-glass.
func TestSSOEnforcement_ToggleGating(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true) // enabled, untested, no domain
	sid := r.signInAs(owner)

	enforce := func(v bool) *http.Response {
		return doInviteReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid,
			map[string]bool{"enforced": v})
	}

	// Enabled but not tested → 409.
	if resp := enforce(true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("untested enforce status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}

	// Tested but still no verified domain → 409.
	r.markConnectionTested(providerID)
	if resp := enforce(true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("no-domain enforce status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}

	// All three present → 200, enforced.
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	resp := enforce(true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gated enforce status=%d, want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if !connEnforced(t, r, providerID) {
		t.Error("connection not marked enforced after a successful toggle")
	}
	// The owner is seeded as the default break-glass principal on enable.
	if n := breakGlassCount(t, r, orgA); n != 1 {
		t.Errorf("break-glass count after enable=%d, want 1 (owner seeded)", n)
	}
}

// Disabling the connection blocks the enforce toggle (enable first).
func TestSSOEnforcement_ToggleRequiresEnabled(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", false) // disabled
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.markConnectionTested(providerID)
	sid := r.signInAs(owner)

	resp := doInviteReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid,
		map[string]bool{"enforced": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disabled-connection enforce status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
}

// ---------- break-glass guard tests ----------

// Can't remove the last break-glass principal while enforced; un-enforcing
// first makes the removal possible.
func TestBreakGlass_CantRemoveLastWhileEnforced(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true) // enforced
	r.addBreakGlass(orgA, owner)              // the single break-glass principal
	sid := r.signInAs(owner)

	// Removing the last one while enforced is refused.
	resp := doInviteReq(r, http.MethodDelete, "/api/sso/break-glass/"+owner.String(), sid, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("remove-last-while-enforced status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if n := breakGlassCount(t, r, orgA); n != 1 {
		t.Errorf("break-glass count=%d after refused remove, want 1 (unchanged)", n)
	}

	// Un-enforce, then the same removal succeeds.
	r.setConnectionEnforced(providerID, false)
	resp2 := doInviteReq(r, http.MethodDelete, "/api/sso/break-glass/"+owner.String(), sid, nil)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("remove-after-unenforce status=%d, want 204 (body=%s)", resp2.StatusCode, readBody(resp2))
	}
	if n := breakGlassCount(t, r, orgA); n != 0 {
		t.Errorf("break-glass count=%d after remove, want 0", n)
	}
}

// Adding a break-glass principal by verified email, then removing it (allowed
// because the owner remains the last one).
func TestBreakGlass_AddByEmailAndRemove(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgA, owner)
	sid := r.signInAs(owner)

	// A second member with a verified email (seedUser links a verified <id>@test
	// identity), made a member so the resolver accepts them.
	member := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		member, orgA); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	resp := doInviteReq(r, http.MethodPost, "/api/sso/break-glass", sid,
		map[string]string{"email": member.String() + "@test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add status=%d, want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}
	// The response body must reflect the just-added principal (it's read after the
	// add commits, not from the still-uncommitted tx) — guards the stale-list bug.
	var added []struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatalf("decode add response: %v", err)
	}
	foundInBody := false
	for _, p := range added {
		if p.UserID == member.String() {
			foundInBody = true
		}
	}
	if !foundInBody {
		t.Errorf("add response body %+v omits the just-added member %s", added, member)
	}
	if n := breakGlassCount(t, r, orgA); n != 2 {
		t.Fatalf("break-glass count=%d after add, want 2", n)
	}

	// Now removing the added member is fine (owner is still break-glass).
	resp2 := doInviteReq(r, http.MethodDelete, "/api/sso/break-glass/"+member.String(), sid, nil)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("remove status=%d, want 204 (body=%s)", resp2.StatusCode, readBody(resp2))
	}
	if n := breakGlassCount(t, r, orgA); n != 1 {
		t.Errorf("break-glass count=%d after remove, want 1", n)
	}
}

// Adding by an email that belongs to no member of the org → 404, no row written.
func TestBreakGlass_AddUnknownEmail404(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	sid := r.signInAs(owner)

	resp := doInviteReq(r, http.MethodPost, "/api/sso/break-glass", sid,
		map[string]string{"email": "nobody@elsewhere.example"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-email add status=%d, want 404 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if n := breakGlassCount(t, r, orgA); n != 0 {
		t.Errorf("break-glass count=%d, want 0 (nothing added for an unknown email)", n)
	}
}

// ---------- small DB probes ----------

func connEnforced(t *testing.T, r *authRig, providerID string) bool {
	t.Helper()
	var enforced bool
	if err := r.h.AdminDB.QueryRow(
		`SELECT enforced FROM sso_connections WHERE provider_id = $1`, providerID,
	).Scan(&enforced); err != nil {
		t.Fatalf("read enforced: %v", err)
	}
	return enforced
}

func breakGlassCount(t *testing.T, r *authRig, orgID uuid.UUID) int {
	t.Helper()
	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM sso_break_glass WHERE org_id = $1`, orgID,
	).Scan(&n); err != nil {
		t.Fatalf("count break-glass: %v", err)
	}
	return n
}
