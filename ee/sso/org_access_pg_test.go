package sso_test

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// "Require SSO" for org access: an org with an enabled, enforced connection is
// reachable only through a session or API token signed in through that
// connection, for every member whatever their email, break-glass principals
// excepted. These drive a real server over its public handler and pin the
// answers the routes give: 403 SSO_REQUIRED to a barred member, the route's own
// 404 to a non-member, and today's behaviour wherever the requirement does not
// apply.

// ---------- helpers ----------

// seedRequiringOrg seeds an org owned by owner whose connection is enabled and
// tested, with dom verified, and returns the org and the connection's
// provider. It does not turn the requirement on: most tests sign someone in
// first, the order an admin turning it on meets existing sessions in.
func (r *authRig) seedRequiringOrg(owner uuid.UUID, slug, dom string) (uuid.UUID, string) {
	r.t.Helper()
	providerID := uuid.NewString()
	orgID, _ := r.seedOrg(owner, slug)
	r.seedSSOConnection(orgID, providerID, "member", true)
	r.seedVerifiedDomain(orgID, providerID, dom)
	r.markConnectionTested(providerID)
	return orgID, providerID
}

// addOrgMember makes userID a member of orgID with role.
func (r *authRig) addOrgMember(orgID, userID uuid.UUID, role string) {
	r.t.Helper()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, $3::public.org_role)`,
		userID, orgID, role); err != nil {
		r.t.Fatalf("add org member: %v", err)
	}
}

// linkSAMLIdentity gives principal a second login identity, a SAML one through
// providerID holding email verified, and returns its auth.users id — so a SAML
// sign-in as that identity resolves to principal, the way a person who has
// signed in both ways before is resolved.
func (r *authRig) linkSAMLIdentity(principal uuid.UUID, providerID, email string) uuid.UUID {
	r.t.Helper()
	identity := uuid.New()
	r.h.SeedAuthUser(r.t, identity.String(), identity.String()+"@saml.test")
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, sso_provider_id, email, email_verified)
		VALUES ($1, $2, 'saml', $3, $4, true)
	`, identity, principal, providerID, email); err != nil {
		r.t.Fatalf("link saml identity: %v", err)
	}
	return identity
}

// ssoSignIn signs in as a SAML identity through providerID and returns the sid.
func (r *authRig) ssoSignIn(identity uuid.UUID, providerID, email string) string {
	r.t.Helper()
	return r.sidFromResp(r.driveSAMLLoginAs(identity, providerID, email))
}

// bearer fires a request authenticated by an API token, with an optional JSON
// body.
func bearer(r *authRig, method, path, token string, body any) *http.Response {
	r.t.Helper()
	b := "{}"
	if body != nil {
		raw, _ := json.Marshal(body)
		b = string(raw)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return r.serve(req)
}

// mintToken mints a token for orgID through POST /api/me/tokens on the given
// session and returns the plaintext, fatal unless the mint succeeds.
func mintToken(r *authRig, sid string, orgID uuid.UUID) string {
	r.t.Helper()
	resp := doReq(r, http.MethodPost, "/api/me/tokens", sid,
		map[string]string{"name": "automation", "org_id": orgID.String()})
	if resp.StatusCode != http.StatusCreated {
		r.t.Fatalf("mint token status=%d, want 201 (body=%s)", resp.StatusCode, readBody(resp))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		r.t.Fatalf("decode token: %v", err)
	}
	return out.Token
}

// errorReason returns the first error item's reason, "" for a body without one.
func errorReason(resp *http.Response) string {
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Errors) == 0 {
		return ""
	}
	return body.Errors[0].Reason
}

// wantSSORequired asserts a 403 SSO_REQUIRED.
func wantSSORequired(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("%s: status=%d, want 403 (body=%s)", what, resp.StatusCode, readBody(resp))
		return
	}
	if got := errorReason(resp); got != "SSO_REQUIRED" {
		t.Errorf("%s: reason=%q, want SSO_REQUIRED", what, got)
	}
}

// wantStatus asserts resp carries want.
func wantStatus(t *testing.T, what string, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Errorf("%s: status=%d, want %d (body=%s)", what, resp.StatusCode, want, readBody(resp))
	}
}

// cursorRoute and pathRoute are one org-scoped read of each kind: the first
// resolves its org from the credential, the second names it in the path.
func cursorRoute(r *authRig, sid string) *http.Response {
	return doReq(r, http.MethodPost, "/api/teams/list", sid, map[string]any{})
}

func pathRoute(r *authRig, sid string, orgID uuid.UUID) *http.Response {
	return doReq(r, http.MethodGet, "/api/orgs/"+orgID.String(), sid, nil)
}

type meOrgAccess struct {
	ActiveOrgID string `json:"active_org_id"`
	Orgs        []struct {
		ID          string `json:"id"`
		SSORequired bool   `json:"sso_required"`
		SSOStartURL string `json:"sso_start_url"`
	} `json:"orgs"`
}

func meAccess(t *testing.T, r *authRig, sid string) meOrgAccess {
	t.Helper()
	resp := r.requestWithSid(http.MethodGet, "/api/me", sid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}
	var m meOrgAccess
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	return m
}

// orgRow returns /api/me's row for orgID, fatal when absent.
func (m meOrgAccess) orgRow(t *testing.T, orgID uuid.UUID) (required bool, startURL string) {
	t.Helper()
	for _, o := range m.Orgs {
		if o.ID == orgID.String() {
			return o.SSORequired, o.SSOStartURL
		}
	}
	t.Fatalf("/api/me lists no row for org %s", orgID)
	return false, ""
}

// ---------- the member signed in some other way ----------

// A member whose GitHub sign-in carries an address off the org's domain is
// exactly who sign-in enforcement never saw. Once the org requires SSO, their
// existing session can act in it no longer: cursor-scoped and path-scoped
// routes answer 403 SSO_REQUIRED, the viewer-relative /api/me still answers and
// says where to sign in, and neither switching to the org nor minting a token
// for it gets around the requirement.
func TestOrgAccess_GitHubMemberIsBarred(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	member := r.seedUser() // GitHub identity <id>@test: off the org's domain
	r.addOrgMember(orgX, member, "member")
	sid := r.signInAs(member)

	wantStatus(t, "cursor route before the requirement", cursorRoute(r, sid), http.StatusOK)
	r.setConnectionEnforced(providerID, true)

	wantSSORequired(t, "cursor route", cursorRoute(r, sid))
	wantSSORequired(t, "path route", pathRoute(r, sid, orgX))

	m := meAccess(t, r, sid)
	required, startURL := m.orgRow(t, orgX)
	if !required {
		t.Error("/api/me does not flag the org as requiring SSO")
	}
	if want := ssoStartURL(providerID, "/"); startURL != want {
		t.Errorf("sso_start_url=%q, want %q", startURL, want)
	}
	if m.ActiveOrgID != "" {
		t.Errorf("active_org_id=%q, want none: the session may not act in its cursor org", m.ActiveOrgID)
	}

	wantSSORequired(t, "switch to the org",
		doReq(r, http.MethodPost, "/api/me/active-org", sid, map[string]string{"org_id": orgX.String()}))
	wantSSORequired(t, "mint a token for the org",
		doReq(r, http.MethodPost, "/api/me/tokens", sid, map[string]string{"name": "ci", "org_id": orgX.String()}))
}

// The same member signed in through the org's own connection is admitted
// everywhere the GitHub session was refused.
func TestOrgAccess_SSOSignInIsAdmitted(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.setConnectionEnforced(providerID, true)
	member := r.seedUser()
	r.addOrgMember(orgX, member, "member")
	identity := r.linkSAMLIdentity(member, providerID, "bob@corp.example")

	sid := r.ssoSignIn(identity, providerID, "bob@corp.example")
	if got := r.principalOf(identity); got != member {
		t.Fatalf("SSO sign-in resolved to %s, want the member %s", got, member)
	}

	wantStatus(t, "cursor route", cursorRoute(r, sid), http.StatusOK)
	wantStatus(t, "path route", pathRoute(r, sid, orgX), http.StatusOK)
	if required, _ := meAccess(t, r, sid).orgRow(t, orgX); required {
		t.Error("/api/me flags the org as requiring SSO for a session signed in through it")
	}
	wantStatus(t, "switch to the org",
		doReq(r, http.MethodPost, "/api/me/active-org", sid, map[string]string{"org_id": orgX.String()}), http.StatusOK)
	mintToken(r, sid, orgX)
}

// Break-glass is the recovery path: a break-glass member signed in with GitHub
// keeps the org.
func TestOrgAccess_BreakGlassGitHubIsAdmitted(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgX, owner)

	sid := r.signInAs(owner)
	wantStatus(t, "cursor route", cursorRoute(r, sid), http.StatusOK)
	wantStatus(t, "path route", pathRoute(r, sid, orgX), http.StatusOK)
	if required, _ := meAccess(t, r, sid).orgRow(t, orgX); required {
		t.Error("/api/me flags the org as requiring SSO for a break-glass member")
	}
}

// A non-member naming the org gets the route's own 404, exactly as before: an
// org they cannot see does not tell them whether it requires SSO.
func TestOrgAccess_NonMemberStill404(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.setConnectionEnforced(providerID, true)

	stranger := r.seedUser()
	r.seedOrg(stranger, "elsewhere")
	sid := r.signInAs(stranger)

	resp := pathRoute(r, sid, orgX)
	wantStatus(t, "non-member path route", resp, http.StatusNotFound)
}

// ---------- API tokens ----------

// A token carries the sign-in of the credential that minted it. One minted from
// a session signed in through the org's connection works, and so does a token
// it mints in turn; one minted from a GitHub session before the requirement is
// refused after it.
func TestOrgAccess_TokensInheritTheirSignIn(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	member := r.seedUser()
	r.addOrgMember(orgX, member, "member")

	githubToken := mintToken(r, r.signInAs(member), orgX)
	wantStatus(t, "github token before the requirement",
		bearer(r, http.MethodPost, "/api/teams/list", githubToken, nil), http.StatusOK)

	r.setConnectionEnforced(providerID, true)
	wantSSORequired(t, "github token cursor route",
		bearer(r, http.MethodPost, "/api/teams/list", githubToken, nil))
	wantSSORequired(t, "github token path route",
		bearer(r, http.MethodGet, "/api/orgs/"+orgX.String(), githubToken, nil))

	identity := r.linkSAMLIdentity(member, providerID, "bob@corp.example")
	ssoToken := mintToken(r, r.ssoSignIn(identity, providerID, "bob@corp.example"), orgX)
	wantStatus(t, "sso token cursor route",
		bearer(r, http.MethodPost, "/api/teams/list", ssoToken, nil), http.StatusOK)
	wantStatus(t, "sso token path route",
		bearer(r, http.MethodGet, "/api/orgs/"+orgX.String(), ssoToken, nil), http.StatusOK)

	resp := bearer(r, http.MethodPost, "/api/me/tokens", ssoToken,
		map[string]string{"name": "child", "org_id": orgX.String()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("token-minted token status=%d, want 201 (body=%s)", resp.StatusCode, readBody(resp))
	}
	var child struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&child); err != nil {
		t.Fatalf("decode child token: %v", err)
	}
	wantStatus(t, "token minted by an sso token",
		bearer(r, http.MethodPost, "/api/teams/list", child.Token, nil), http.StatusOK)
}

// ---------- where the requirement does not apply ----------

// With the requirement off, or with the org's sso entitlement lapsed, a GitHub
// member acts in the org as they always have — the lapse half mirrors the
// sign-in anti-lockout guarantee.
func TestOrgAccess_UnenforcedOrLapsedIsUnchanged(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	member := r.seedUser()
	r.addOrgMember(orgX, member, "member")
	sid := r.signInAs(member)

	wantStatus(t, "not enforced: cursor route", cursorRoute(r, sid), http.StatusOK)
	wantStatus(t, "not enforced: path route", pathRoute(r, sid, orgX), http.StatusOK)

	r.setConnectionEnforced(providerID, true)
	entitlements.RegisterProvider(entitlements.Static())
	t.Cleanup(func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSSO)) })

	wantStatus(t, "lapsed: cursor route", cursorRoute(r, sid), http.StatusOK)
	wantStatus(t, "lapsed: path route", pathRoute(r, sid, orgX), http.StatusOK)
	if required, _ := meAccess(t, r, sid).orgRow(t, orgX); required {
		t.Error("/api/me flags a lapsed org as requiring SSO")
	}
}

// ---------- joining and landing ----------

// An invite into an org that requires SSO is accepted only through it: from a
// GitHub session the accept is refused and the invite stays pending, and from
// a session signed in through the org's connection it is accepted.
func TestOrgAccess_InviteAcceptRequiresSSO(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.setConnectionEnforced(providerID, true)

	invitee := r.seedUser()
	rawToken := "invite-" + uuid.NewString()
	hash := sha256.Sum256([]byte(rawToken))
	var inviteID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO org_invites (org_id, email, role, token_hash, invited_by, expires_at)
		VALUES ($1, $2, 'member', $3, $4, $5) RETURNING id::text
	`, orgX, invitee.String()+"@test", hash[:], owner, time.Now().Add(24*time.Hour)).Scan(&inviteID); err != nil {
		t.Fatalf("seed invite: %v", err)
	}
	accepted := func() bool {
		t.Helper()
		var at *time.Time
		if err := r.h.AdminDB.QueryRow(`SELECT accepted_at FROM org_invites WHERE id = $1`, inviteID).Scan(&at); err != nil {
			t.Fatalf("read invite: %v", err)
		}
		return at != nil
	}

	// A GitHub sign-in that keeps the invited address verified, so the accept
	// gets past the invitee-email check and meets the requirement.
	claims := validClaimsFor(invitee)
	claims["email_verified"] = true
	githubSID := r.signInWithClaims(invitee, claims)
	wantSSORequired(t, "accept from a github session",
		doReq(r, http.MethodPost, "/api/invites/accept", githubSID, map[string]string{"token": rawToken}))
	if accepted() {
		t.Fatal("a refused accept consumed the invite")
	}
	if n := r.membershipCount(invitee, orgX); n != 0 {
		t.Fatalf("a refused accept granted %d memberships", n)
	}

	identity := r.linkSAMLIdentity(invitee, providerID, "invitee@corp.example")
	ssoSID := r.ssoSignIn(identity, providerID, "invitee@corp.example")
	wantStatus(t, "accept from an sso session",
		doReq(r, http.MethodPost, "/api/invites/accept", ssoSID, map[string]string{"token": rawToken}), http.StatusOK)
	if !accepted() {
		t.Error("the accept from an sso session left the invite pending")
	}
}

// A GitHub sign-in no longer lands in an org that would refuse it: the session
// defaults to the earliest membership that admits it.
func TestOrgAccess_LoginDefaultSkipsRequiringOrg(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	other := r.seedUser()
	orgY, _ := r.seedOrg(other, "beta")

	member := r.seedUser()
	r.addOrgMember(orgX, member, "member")
	r.addOrgMember(orgY, member, "member")
	if _, err := r.h.AdminDB.Exec(`
		UPDATE org_memberships SET created_at = now() - interval '1 day'
		 WHERE user_id = $1 AND org_id = $2`, member, orgX); err != nil {
		t.Fatalf("backdate membership: %v", err)
	}

	if got := meAccess(t, r, r.signInAs(member)).ActiveOrgID; got != orgX.String() {
		t.Fatalf("default org before the requirement=%q, want the earliest membership %s", got, orgX)
	}
	r.setConnectionEnforced(providerID, true)
	if got := meAccess(t, r, r.signInAs(member)).ActiveOrgID; got != orgY.String() {
		t.Errorf("default org after the requirement=%q, want %s", got, orgY)
	}
}

// ---------- guards on the controls ----------

// An admin cannot turn the requirement on from a session it would bar: they
// must be signed in through the connection or be break-glass.
func TestOrgAccess_EnableRequiresAnAdmittedCaller(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.addBreakGlass(orgX, owner) // the set is not empty, so nobody is seeded
	admin := r.seedUser()
	r.addOrgMember(orgX, admin, "admin")

	enforce := func(sid string) *http.Response {
		return doReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid, map[string]bool{"enforced": true})
	}

	resp := enforce(r.signInAs(admin))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("github admin enable status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if body := readBody(resp); !strings.Contains(body, "Sign in through single sign-on, or be designated break-glass, before requiring it.") {
		t.Errorf("409 body=%s, want the sign-in-first message", body)
	}
	if connEnforced(t, r, providerID) {
		t.Fatal("a refused enable turned the requirement on")
	}

	identity := r.linkSAMLIdentity(admin, providerID, "admin@corp.example")
	wantStatus(t, "sso admin enable", enforce(r.ssoSignIn(identity, providerID, "admin@corp.example")), http.StatusOK)
	if !connEnforced(t, r, providerID) {
		t.Error("the sso admin's enable did not turn the requirement on")
	}
}

// While the requirement is on, neither the connection nor the org's last
// verified domain can be removed out from under it.
func TestOrgAccess_RequirementPinsConnectionAndLastDomain(t *testing.T) {
	r := newAuthRig(t)
	owner := r.seedUser()
	orgX, providerID := r.seedRequiringOrg(owner, "acme", "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgX, owner)
	sid := r.signInAs(owner)

	resp := doReq(r, http.MethodPatch, "/api/sso/connection", sid, map[string]bool{"enabled": false})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disable while enforced status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if body := readBody(resp); !strings.Contains(body, "Turn off Require SSO first.") {
		t.Errorf("disable 409 body=%s, want the turn-off-first message", body)
	}

	var domainID string
	if err := r.h.AdminDB.QueryRow(
		`SELECT id::text FROM sso_domains WHERE org_id = $1`, orgX).Scan(&domainID); err != nil {
		t.Fatalf("read domain id: %v", err)
	}
	resp = doReq(r, http.MethodDelete, "/api/sso/domains/"+domainID, sid, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete last verified domain status=%d, want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if body := readBody(resp); !strings.Contains(body, "Turn off Require SSO first.") {
		t.Errorf("delete 409 body=%s, want the turn-off-first message", body)
	}

	// With a second verified domain the first is no longer the last.
	r.seedVerifiedDomain(orgX, providerID, "corp2.example")
	wantStatus(t, "delete a verified domain that is not the last",
		doReq(r, http.MethodDelete, "/api/sso/domains/"+domainID, sid, nil), http.StatusOK)

	// Requirement off, the connection can go.
	r.setConnectionEnforced(providerID, false)
	wantStatus(t, "disable after the requirement is off",
		doReq(r, http.MethodPatch, "/api/sso/connection", sid, map[string]bool{"enabled": false}), http.StatusOK)
}
