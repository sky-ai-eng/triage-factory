package sso_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SSO auth_events capture (TFAC-76): the two EE-only events recorded through the
// RecordAuthEvent extension seam — sso_enforcement_rejected (a non-SSO login
// bounced off an enforced domain) and break_glass_login (a break-glass account
// bypassing enforcement that would otherwise apply). Black-box: driven through
// the real login HTTP path, read back from the admin pool.

// capturedSSOAuthEvent is one auth_events row read back for assertion.
type capturedSSOAuthEvent struct {
	userID, orgID, ipAddress, userAgent string
	detail                              map[string]any
}

// oneSSOAuthEvent reads the single auth_events row of eventType; fatal otherwise.
// detail_json is parsed (jsonb normalizes its text form); host() yields the bare
// source IP the seam captured.
func (r *authRig) oneSSOAuthEvent(eventType string) capturedSSOAuthEvent {
	r.t.Helper()
	rows, err := r.h.AdminDB.Query(`
		SELECT COALESCE(user_id::text, ''), COALESCE(org_id::text, ''),
		       COALESCE(host(ip_address), ''), COALESCE(user_agent, ''),
		       COALESCE(detail_json::text, '')
		  FROM auth_events WHERE event_type = $1`, eventType)
	if err != nil {
		r.t.Fatalf("query auth_events(%s): %v", eventType, err)
	}
	defer rows.Close()
	var out []capturedSSOAuthEvent
	for rows.Next() {
		var ce capturedSSOAuthEvent
		var detail string
		if err := rows.Scan(&ce.userID, &ce.orgID, &ce.ipAddress, &ce.userAgent, &detail); err != nil {
			r.t.Fatalf("scan auth_event: %v", err)
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &ce.detail); err != nil {
				r.t.Fatalf("auth_event detail not json: %q", detail)
			}
		}
		out = append(out, ce)
	}
	if len(out) != 1 {
		r.t.Fatalf("auth_events(%s) = %d rows, want exactly 1", eventType, len(out))
	}
	return out[0]
}

// An enforced verified domain that rejects a non-SSO login records
// sso_enforcement_rejected, scoped to the resolved principal + the enforcing org,
// with {domain, org_id} detail. The principal still gets minted (enforcement
// blocks only the session), so the event carries the resolved user.
func TestSSOAuthEvents_EnforcementRejected_Recorded(t *testing.T) {
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
	if !rejectedForSSO(resp) {
		t.Fatalf("precondition: login was not rejected for SSO (Location=%q)", resp.Header.Get("Location"))
	}

	ev := r.oneSSOAuthEvent("sso_enforcement_rejected")
	if ev.userID != r.principalOf(user).String() {
		t.Errorf("event user=%q, want resolved principal %q", ev.userID, r.principalOf(user))
	}
	if ev.orgID != orgA.String() {
		t.Errorf("event org=%q, want %q", ev.orgID, orgA)
	}
	if ev.detail["domain"] != "corp.example" || ev.detail["org_id"] != orgA.String() {
		t.Errorf("event detail=%v, want domain=corp.example org_id=%s", ev.detail, orgA)
	}
	// The seam fills the source IP from the callback request (httptest's default
	// RemoteAddr 192.0.2.1:1234 → 192.0.2.1) via core's canonical clientIP parsing.
	if ev.ipAddress != "192.0.2.1" {
		t.Errorf("event ip_address=%q, want 192.0.2.1 (seam must capture the source IP)", ev.ipAddress)
	}
}

// A break-glass account that bypasses enforcement records break_glass_login —
// the highest-scrutiny SOC2 event — scoped to the principal + org, with the same
// {domain, org_id} detail. The login still succeeds (a session is minted).
func TestSSOAuthEvents_BreakGlassLogin_Recorded(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)
	r.seedVerifiedDomain(orgA, providerID, "corp.example")
	r.setConnectionEnforced(providerID, true)
	r.addBreakGlass(orgA, owner)

	resp := r.driveGitHubLogin(owner, "owner@corp.example")
	if resp.StatusCode != http.StatusFound || rejectedForSSO(resp) {
		t.Fatalf("break-glass login was rejected (status=%d, loc=%q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !r.gotSidCookie(resp) {
		t.Fatal("no session minted for the break-glass login")
	}

	ev := r.oneSSOAuthEvent("break_glass_login")
	if ev.userID != owner.String() {
		t.Errorf("event user=%q, want break-glass principal %q", ev.userID, owner)
	}
	if ev.orgID != orgA.String() {
		t.Errorf("event org=%q, want %q", ev.orgID, orgA)
	}
	if ev.detail["domain"] != "corp.example" || ev.detail["org_id"] != orgA.String() {
		t.Errorf("event detail=%v, want domain=corp.example org_id=%s", ev.detail, orgA)
	}
	if ev.ipAddress != "192.0.2.1" {
		t.Errorf("event ip_address=%q, want 192.0.2.1 (seam must capture the source IP)", ev.ipAddress)
	}

	// And NO enforcement-rejected event for the same login (the BG path bypasses).
	var rejected int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM auth_events WHERE event_type = 'sso_enforcement_rejected'`,
	).Scan(&rejected); err != nil {
		t.Fatalf("count rejected: %v", err)
	}
	if rejected != 0 {
		t.Errorf("break-glass login also recorded %d enforcement-rejected events, want 0", rejected)
	}
}
