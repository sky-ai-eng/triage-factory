package sso_test

// Coverage for the access-change-log half of the SSO admin surface. Registering
// an IdP, enabling it, requiring it org-wide, verifying a domain that arms JIT
// provisioning, and exempting a principal from enforcement are all changes to
// who can get in — and none of them left a trace before.
//
// Two properties, asserted on every write-point: the action records, and an
// idempotent re-save records NOTHING. The second is the one that matters for an
// audit log — a phantom row saying an admin re-enabled SSO on a day they only
// re-saved a form is worse than no row at all.
//
// Lives in ee/sso for the same reason its Slack sibling does: the open-core
// boundary keeps core's credential-audit suite from ever reaching these
// handlers.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/ee/sso/dnsoverride"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// auditCount is the number of rows recorded for one action in an org.
func auditCount(r *authRig, orgID uuid.UUID, action string) int {
	r.t.Helper()
	return len(r.accessChangeRows(orgID, action))
}

// registerConnection drives the create endpoint and fails the test if it
// doesn't take — the shared preamble for the connection/enforcement cases.
func registerConnection(r *authRig, sid string) {
	r.t.Helper()
	resp := doReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if resp.StatusCode != http.StatusCreated {
		r.t.Fatalf("create connection status = %d (body=%s)", resp.StatusCode, readBody(resp))
	}
}

// Registering an IdP records one row carrying the GoTrue provider id.
func TestSSOAudit_ConnectionCreate(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-create")
	sid := r.signInAs(owner)

	registerConnection(r, sid)

	rows := r.accessChangeRows(orgA, domain.AccessActionSSOConnectionCreated)
	if len(rows) != 1 {
		t.Fatalf("sso_connection_created rows = %d, want 1", len(rows))
	}
	if rows[0].actor != r.principalOf(owner).String() {
		t.Errorf("actor = %q, want the registering admin", rows[0].actor)
	}
	if rows[0].detail == "" {
		t.Error("detail_json empty; want the provider id — the connection row may be gone by read time")
	}
}

// A second POST is idempotent (it returns the existing connection), so it must
// not record a second registration.
func TestSSOAudit_ConnectionCreate_IdempotentRecordsOnce(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-create-twice")
	sid := r.signInAs(owner)

	registerConnection(r, sid)
	resp := doReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second create status = %d, want 200 (idempotent)", resp.StatusCode)
	}

	if n := auditCount(r, orgA, domain.AccessActionSSOConnectionCreated); n != 1 {
		t.Errorf("sso_connection_created rows = %d, want 1 — the second POST registered nothing", n)
	}
}

// Disabling then re-enabling records one row each way; re-saving the value it
// already has records nothing.
func TestSSOAudit_ConnectionEnableDisable(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-toggle")
	sid := r.signInAs(owner)
	registerConnection(r, sid)

	setEnabled := func(v bool) {
		t.Helper()
		resp := doReq(r, http.MethodPatch, "/api/sso/connection", sid, map[string]bool{"enabled": v})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set enabled=%v status = %d (body=%s)", v, resp.StatusCode, readBody(resp))
		}
	}

	setEnabled(false)
	if n := auditCount(r, orgA, domain.AccessActionSSOConnectionDisabled); n != 1 {
		t.Fatalf("sso_connection_disabled rows = %d, want 1", n)
	}

	setEnabled(false) // already disabled
	if n := auditCount(r, orgA, domain.AccessActionSSOConnectionDisabled); n != 1 {
		t.Errorf("sso_connection_disabled rows = %d after a no-op re-save, want 1", n)
	}

	setEnabled(true)
	if n := auditCount(r, orgA, domain.AccessActionSSOConnectionEnabled); n != 1 {
		t.Errorf("sso_connection_enabled rows = %d, want 1", n)
	}
}

// Requiring SSO — the widest-reaching change this surface makes — records in
// both directions, once per real transition.
func TestSSOAudit_EnforcementToggle(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-enforce")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.markConnectionTested(providerID)
	sid := r.signInAs(owner)

	setEnforced := func(v bool) {
		t.Helper()
		resp := doReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid,
			map[string]bool{"enforced": v})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set enforced=%v status = %d (body=%s)", v, resp.StatusCode, readBody(resp))
		}
	}

	setEnforced(true)
	if n := auditCount(r, orgA, domain.AccessActionSSOEnforcementEnabled); n != 1 {
		t.Fatalf("sso_enforcement_enabled rows = %d, want 1", n)
	}

	setEnforced(true) // already enforced
	if n := auditCount(r, orgA, domain.AccessActionSSOEnforcementEnabled); n != 1 {
		t.Errorf("sso_enforcement_enabled rows = %d after a no-op re-save, want 1", n)
	}

	setEnforced(false)
	if n := auditCount(r, orgA, domain.AccessActionSSOEnforcementDisabled); n != 1 {
		t.Errorf("sso_enforcement_disabled rows = %d, want 1", n)
	}
}

// A refused enforce (preconditions unmet → 409) changed nothing, so it records
// nothing.
func TestSSOAudit_EnforcementRefused_RecordsNothing(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-enforce-refused")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true) // untested, no verified domain
	sid := r.signInAs(owner)

	resp := doReq(r, http.MethodPatch, "/api/sso/connection/enforcement", sid,
		map[string]bool{"enforced": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (unmet preconditions)", resp.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSOEnforcementEnabled); n != 0 {
		t.Errorf("sso_enforcement_enabled rows = %d, want 0 for a refused toggle", n)
	}
}

// Claim → verify → delete each record once, and the idempotent re-runs of the
// first two record nothing.
func TestSSOAudit_DomainLifecycle(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-domain")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	sid := r.signInAs(owner)

	const dom = "corp.example"
	status, claimed, raw := ssoClaim(r, sid, dom)
	if status != http.StatusCreated {
		t.Fatalf("claim status = %d, want 201 (body=%s)", status, raw)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSODomainClaimed); n != 1 {
		t.Fatalf("sso_domain_claimed rows = %d, want 1", n)
	}

	if status, _, _ := ssoClaim(r, sid, dom); status != http.StatusOK {
		t.Fatalf("re-claim status = %d, want 200 (idempotent)", status)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSODomainClaimed); n != 1 {
		t.Errorf("sso_domain_claimed rows = %d after a re-claim, want 1", n)
	}

	dnsoverride.Set(&fakeTXTResolver{
		records: map[string][]string{ssoTXTHost(dom): {claimed.TXTValue}},
	})
	t.Cleanup(func() { dnsoverride.Set(nil) })
	verify := doReq(r, http.MethodPost, "/api/sso/domains/"+claimed.ID+"/verify", sid, nil)
	if verify.StatusCode != http.StatusOK {
		t.Fatalf("verify status = %d (body=%s)", verify.StatusCode, readBody(verify))
	}
	if n := auditCount(r, orgA, domain.AccessActionSSODomainVerified); n != 1 {
		t.Fatalf("sso_domain_verified rows = %d, want 1", n)
	}

	reverify := doReq(r, http.MethodPost, "/api/sso/domains/"+claimed.ID+"/verify", sid, nil)
	if reverify.StatusCode != http.StatusOK {
		t.Fatalf("re-verify status = %d, want 200", reverify.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSODomainVerified); n != 1 {
		t.Errorf("sso_domain_verified rows = %d after a re-verify, want 1", n)
	}

	del := doReq(r, http.MethodDelete, "/api/sso/domains/"+claimed.ID, sid, nil)
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d (body=%s)", del.StatusCode, readBody(del))
	}
	rows := r.accessChangeRows(orgA, domain.AccessActionSSODomainRemoved)
	if len(rows) != 1 {
		t.Fatalf("sso_domain_removed rows = %d, want 1", len(rows))
	}
	// The domain has to be captured on the row: the sso_domains row is gone, so
	// an id-only detail would leave the log unreadable.
	if rows[0].detail == "" {
		t.Error("detail_json empty on the removal; want the domain name")
	}
}

// Deleting a domain claim that doesn't exist is a 404 and records nothing.
func TestSSOAudit_DomainDeleteMissing_RecordsNothing(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-domain-missing")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	sid := r.signInAs(owner)

	resp := doReq(r, http.MethodDelete, "/api/sso/domains/"+uuid.NewString(), sid, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSODomainRemoved); n != 0 {
		t.Errorf("sso_domain_removed rows = %d, want 0", n)
	}
}

// seedOrgMember makes a second seeded user a member of orgA so the break-glass
// email resolver accepts them.
func seedOrgMember(r *authRig, orgA uuid.UUID) uuid.UUID {
	r.t.Helper()
	member := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		member, orgA); err != nil {
		r.t.Fatalf("seed member: %v", err)
	}
	return member
}

// Adding a break-glass principal records the exemption against that principal;
// re-adding the same one records nothing.
func TestSSOAudit_BreakGlassAdd(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-bg-add")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	sid := r.signInAs(owner)
	member := seedOrgMember(r, orgA)

	add := func() *http.Response {
		return doReq(r, http.MethodPost, "/api/sso/break-glass", sid,
			map[string]string{"email": member.String() + "@test"})
	}
	if resp := add(); resp.StatusCode != http.StatusOK {
		t.Fatalf("add status = %d (body=%s)", resp.StatusCode, readBody(resp))
	}

	rows := r.accessChangeRows(orgA, domain.AccessActionSSOBreakGlassAdded)
	if len(rows) != 1 {
		t.Fatalf("sso_break_glass_added rows = %d, want 1", len(rows))
	}
	if rows[0].target != member.String() {
		t.Errorf("target = %q, want the exempted principal %q", rows[0].target, member)
	}
	if rows[0].actor != r.principalOf(owner).String() {
		t.Errorf("actor = %q, want the admin who granted it", rows[0].actor)
	}

	if resp := add(); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-add status = %d, want 200", resp.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSOBreakGlassAdded); n != 1 {
		t.Errorf("sso_break_glass_added rows = %d after a re-add, want 1 — Add is ON CONFLICT DO NOTHING", n)
	}
}

// Removing a principal records the revocation; a remove the guard blocked, and
// a remove of someone who was never on the list, record nothing.
func TestSSOAudit_BreakGlassRemove(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "sso-audit-bg-remove")
	providerID := uuid.NewString()
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgA, r.principalOf(owner))
	sid := r.signInAs(owner)
	member := seedOrgMember(r, orgA)
	r.addBreakGlass(orgA, member)

	del := doReq(r, http.MethodDelete, "/api/sso/break-glass/"+member.String(), sid, nil)
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("remove status = %d (body=%s)", del.StatusCode, readBody(del))
	}
	rows := r.accessChangeRows(orgA, domain.AccessActionSSOBreakGlassRemoved)
	if len(rows) != 1 {
		t.Fatalf("sso_break_glass_removed rows = %d, want 1", len(rows))
	}
	if rows[0].target != member.String() {
		t.Errorf("target = %q, want the principal whose exemption was pulled", rows[0].target)
	}

	// The owner is now the last principal under enforcement — the guard holds
	// the delete, so nothing was revoked and nothing may be recorded.
	blocked := doReq(r, http.MethodDelete, "/api/sso/break-glass/"+r.principalOf(owner).String(), sid, nil)
	if blocked.StatusCode != http.StatusConflict {
		t.Fatalf("guarded remove status = %d, want 409", blocked.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSOBreakGlassRemoved); n != 1 {
		t.Errorf("sso_break_glass_removed rows = %d after a blocked remove, want 1", n)
	}

	// A principal who was never break-glass: the handler answers 204, but there
	// was no exemption to revoke.
	stranger := seedOrgMember(r, orgA)
	noop := doReq(r, http.MethodDelete, "/api/sso/break-glass/"+stranger.String(), sid, nil)
	if noop.StatusCode != http.StatusNoContent {
		t.Fatalf("no-op remove status = %d, want 204", noop.StatusCode)
	}
	if n := auditCount(r, orgA, domain.AccessActionSSOBreakGlassRemoved); n != 1 {
		t.Errorf("sso_break_glass_removed rows = %d after a no-op remove, want 1", n)
	}
}
