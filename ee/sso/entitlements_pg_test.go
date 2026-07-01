package sso_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Per-request `sso` entitlement gating — replaces the retired process-global
// entitlements.Active()/Checker mount-gate. rig_test.go's init() licenses the
// `sso` feature for this test binary by default; these tests flip the
// process-global provider unlicensed for their duration and restore it via
// t.Cleanup, relying on the package's tests running sequentially (no
// t.Parallel() in this suite).

// A licensed org: the admin route answers 2xx and an SP-initiated SAML login
// JIT-provisions membership — the always-mount change must not regress the
// licensed path.
func TestEntitlement_LicensedOrg_AdminAndJITWork(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r, _ := newSSORig(t)

	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	sid := r.signInAs(owner)

	if resp := doReq(r, http.MethodGet, "/api/sso/connection", sid, nil); resp.StatusCode/100 != 2 {
		t.Fatalf("admin route status=%d, want 2xx for a licensed org", resp.StatusCode)
	}

	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("saml callback status=%d, want 302", resp.StatusCode)
	}
	if got := r.membershipCount(r.principalOf(user), orgA); got != 1 {
		t.Errorf("JIT membership count=%d, want 1 for a licensed org", got)
	}
}

// An unlicensed org is refused at every seam: the admin route 404s, the
// SP-initiated SAML start 404s, and discovery collapses to {sso:false}.
func TestEntitlement_UnlicensedOrg_RefusedEverywhere(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static())
	t.Cleanup(func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSSO)) })

	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	sid := r.signInAs(owner)

	if resp := doReq(r, http.MethodGet, "/api/sso/connection", sid, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("admin route status=%d, want 404 for an unlicensed org", resp.StatusCode)
	}

	startResp := r.serve(httptest.NewRequest(http.MethodGet,
		"/api/auth/oauth/saml?provider_id="+providerID, nil))
	if startResp.StatusCode != http.StatusNotFound {
		t.Errorf("saml start status=%d, want 404 for an unlicensed org", startResp.StatusCode)
	}

	body, raw := ssoDiscover(r, map[string]string{"email": "alice@corp.example"})
	if body.SSO || body.StartURL != "" {
		t.Errorf("discovery = %+v, want {sso:false} for an unlicensed org (body=%s)", body, raw)
	}
}

// The anti-lockout guarantee: an org whose connection is already enforced
// (enforced=true) but has since lapsed (unlicensed) must NOT bounce a non-SSO
// login on the enforced domain — enforcement lifts when the entitlement is
// gone, so a lapse can never lock everyone out.
func TestEntitlement_LapsedEnforcedOrg_NotLockedOut(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)

	// The org lapses AFTER enforcement was already turned on — the DB row still
	// says enforced=true.
	entitlements.RegisterProvider(entitlements.Static())
	t.Cleanup(func() { entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSSO)) })

	user := r.seedAuthOnlyUser()
	resp := r.driveGitHubLogin(user, "alice@corp.example")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want 302", resp.StatusCode)
	}
	if rejectedForSSO(resp) {
		t.Error("a lapsed (unlicensed) org's enforced=true connection still rejected a non-SSO login — enforcement must lift on entitlement loss")
	}
	if !r.gotSidCookie(resp) {
		t.Error("no session minted for the lapsed org's non-SSO login — the anti-lockout guarantee must let it through")
	}
}
