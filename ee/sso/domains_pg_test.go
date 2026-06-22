package sso_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/dnsoverride"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ssoDomainJSON / ssoTXTHost moved to ee/sso with the handler. These pgtest
// integration tests depend on the package-server authRig harness, so they keep
// faithful local copies of the wire shape the ee endpoint emits.
type ssoDomainJSON struct {
	ID         string     `json:"id"`
	Domain     string     `json:"domain"`
	TXTHost    string     `json:"txt_host"`
	TXTValue   string     `json:"txt_value"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

func ssoTXTHost(domainName string) string { return "_triagefactory-verification." + domainName }

// fakeTXTResolver is the injected DNS seam for the verify tests — the suite
// never touches real DNS. records maps a TXT host to its published values; a
// host with no entry resolves to no records (the "not propagated yet" case).
// err, when set, forces every lookup to fail (a resolver-down case, which the
// handler still surfaces as a retryable 409).
type fakeTXTResolver struct {
	records map[string][]string
	err     error
}

func (f *fakeTXTResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.records[name], nil
}

// seedSSOConnForTest inserts an sso_connections row via the admin pool (a
// fixture; bypasses RLS) so the domain-claim handler has a connection to
// attach the domain to. Returns the connection id.
func seedSSOConnForTest(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)
		RETURNING id::text
	`, orgID, uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seed sso_connection: %v", err)
	}
	return id
}

// ssoClaim POSTs a domain claim and decodes the response. Returns the status,
// the decoded body, and the raw text (for error messages).
func ssoClaim(r *authRig, sid, domain string) (int, ssoDomainJSON, string) {
	r.t.Helper()
	resp := doInviteReq(r, http.MethodPost, "/api/sso/domains", sid, map[string]string{"domain": domain})
	raw := readBody(resp)
	var body ssoDomainJSON
	_ = json.Unmarshal([]byte(raw), &body)
	return resp.StatusCode, body, raw
}

// TestSSODomainClaim_PendingAndIdempotent: a claim mints one pending row with a
// token + the TXT challenge, and re-claiming the same domain (any case) returns
// the existing row without churning the token or minting a second row.
func TestSSODomainClaim_PendingAndIdempotent(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-claim")
	seedSSOConnForTest(t, r.h, orgID.String())
	sid := r.signInAs(owner)

	status, body, raw := ssoClaim(r, sid, "Corp.com")
	if status != http.StatusCreated {
		t.Fatalf("claim status = %d; want 201 (body=%s)", status, raw)
	}
	if body.Domain != "corp.com" {
		t.Errorf("domain = %q; want normalized corp.com", body.Domain)
	}
	if body.Status != "pending" {
		t.Errorf("status = %q; want pending", body.Status)
	}
	if body.TXTHost != "_triagefactory-verification.corp.com" {
		t.Errorf("txt_host = %q; want _triagefactory-verification.corp.com", body.TXTHost)
	}
	if body.TXTValue == "" || body.ID == "" {
		t.Errorf("missing token/id: %s", raw)
	}

	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM sso_domains WHERE org_id=$1 AND domain='corp.com'`, orgID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d; want 1 pending row", n)
	}

	// Re-claim (lower-case this time) → idempotent 200, same id + token.
	status2, body2, raw2 := ssoClaim(r, sid, "corp.com")
	if status2 != http.StatusOK {
		t.Fatalf("re-claim status = %d; want 200 idempotent (body=%s)", status2, raw2)
	}
	if body2.ID != body.ID {
		t.Errorf("re-claim id = %q; want first claim's id %q", body2.ID, body.ID)
	}
	if body2.TXTValue != body.TXTValue {
		t.Errorf("re-claim token churned: %q -> %q; want stable", body.TXTValue, body2.TXTValue)
	}
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_domains WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count2: %v", err)
	}
	if n != 1 {
		t.Errorf("rows after re-claim = %d; want 1 (no duplicate)", n)
	}
}

// TestSSODomainClaim_NoConnection_409: claiming a domain before the org has
// registered an SSO connection is a 409 and mints nothing — the FK precondition
// surfaced as an actionable error.
func TestSSODomainClaim_NoConnection_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-noconn")
	sid := r.signInAs(owner)

	status, _, raw := ssoClaim(r, sid, "corp.com")
	if status != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (no connection); body=%s", status, raw)
	}
	if !strings.Contains(raw, "register an SSO connection") {
		t.Errorf("body = %q; want the connection-first message", raw)
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_domains WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d; want 0 (nothing minted without a connection)", n)
	}
}

// TestSSODomainVerify_MatchAndMismatch: a matching TXT value stamps the claim
// verified (and re-verify stays an idempotent 200 — verify-once-keep); a
// missing/non-matching record is a retryable 409 that leaves the claim pending.
func TestSSODomainVerify_MatchAndMismatch(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-verify")
	seedSSOConnForTest(t, r.h, orgID.String())
	sid := r.signInAs(owner)

	_, good, _ := ssoClaim(r, sid, "good.com")
	_, missing, _ := ssoClaim(r, sid, "missing.com")

	// good.com's host carries an unrelated value alongside the real token, so
	// the match must scan the whole TXT set. missing.com has no record.
	dnsoverride.Set(&fakeTXTResolver{records: map[string][]string{
		ssoTXTHost("good.com"): {"some-other-record", good.TXTValue},
	}})

	vresp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+good.ID+"/verify", sid, nil)
	vraw := readBody(vresp)
	if vresp.StatusCode != http.StatusOK {
		t.Fatalf("verify good status = %d; want 200 (body=%s)", vresp.StatusCode, vraw)
	}
	var vbody ssoDomainJSON
	_ = json.Unmarshal([]byte(vraw), &vbody)
	if vbody.Status != "verified" || vbody.VerifiedAt == nil {
		t.Errorf("verified body = %+v; want status=verified + verified_at", vbody)
	}
	var stamped sql.NullTime
	if err := r.h.AdminDB.QueryRow(`SELECT verified_at FROM sso_domains WHERE id=$1`, good.ID).Scan(&stamped); err != nil {
		t.Fatalf("read verified_at: %v", err)
	}
	if !stamped.Valid {
		t.Error("good.com verified_at not stamped in DB")
	}

	// missing.com → 409 retry, stays pending.
	mresp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+missing.ID+"/verify", sid, nil)
	mraw := readBody(mresp)
	if mresp.StatusCode != http.StatusConflict {
		t.Fatalf("verify missing status = %d; want 409 (body=%s)", mresp.StatusCode, mraw)
	}
	if !strings.Contains(mraw, "propagation") {
		t.Errorf("409 body = %q; want the retry/propagation message", mraw)
	}
	var missingVer sql.NullTime
	if err := r.h.AdminDB.QueryRow(`SELECT verified_at FROM sso_domains WHERE id=$1`, missing.ID).Scan(&missingVer); err != nil {
		t.Fatalf("read missing verified_at: %v", err)
	}
	if missingVer.Valid {
		t.Error("missing.com should stay pending after a failed verify")
	}

	// Re-verify good.com → idempotent 200 even with an empty resolver (the
	// customer may have removed the TXT record by now).
	dnsoverride.Set(&fakeTXTResolver{})
	reresp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+good.ID+"/verify", sid, nil)
	if reresp.StatusCode != http.StatusOK {
		t.Errorf("re-verify status = %d; want 200 idempotent (body=%s)", reresp.StatusCode, readBody(reresp))
	}
}

// TestSSODomainVerify_ResolverError_RetryableConflict: a resolver failure
// (not just a missing record) is surfaced as the same retryable 409, and the
// claim stays pending — a transient DNS/resolver outage must not hard-fail.
func TestSSODomainVerify_ResolverError_RetryableConflict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-resolver-err")
	seedSSOConnForTest(t, r.h, orgID.String())
	sid := r.signInAs(owner)

	_, claim, _ := ssoClaim(r, sid, "errdom.com")

	// Every lookup fails (resolver down), exercising the lookupErr != nil branch.
	dnsoverride.Set(&fakeTXTResolver{err: errors.New("resolver unavailable")})

	resp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+claim.ID+"/verify", sid, nil)
	raw := readBody(resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("verify status = %d; want 409 (resolver error); body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "propagation") {
		t.Errorf("409 body = %q; want the retry/propagation message", raw)
	}
	var ver sql.NullTime
	if err := r.h.AdminDB.QueryRow(`SELECT verified_at FROM sso_domains WHERE id=$1`, claim.ID).Scan(&ver); err != nil {
		t.Fatalf("read verified_at: %v", err)
	}
	if ver.Valid {
		t.Error("claim should stay pending after a resolver error")
	}
}

// TestSSODomainVerify_GlobalUniqueness_OpaqueConflict: two orgs both hold a
// pending claim on the same domain; the first to verify wins, and the second's
// verify hits the deployment-wide verified-domain uniqueness index — surfaced
// as an opaque 409 that never names the holding org.
func TestSSODomainVerify_GlobalUniqueness_OpaqueConflict(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "sso-glob-a")
	seedSSOConnForTest(t, r.h, orgA.String())
	sidA := r.signInAs(ownerA)

	ownerB := r.seedUser()
	orgB, _ := r.seedOrg(ownerB, "sso-glob-b")
	seedSSOConnForTest(t, r.h, orgB.String())
	sidB := r.signInAs(ownerB)

	_, claimA, _ := ssoClaim(r, sidA, "shared.com")
	_, claimB, _ := ssoClaim(r, sidB, "shared.com")
	if claimA.TXTValue == claimB.TXTValue {
		t.Fatal("expected distinct per-claim tokens for the two orgs")
	}

	// The shared TXT host carries both orgs' tokens — each published its own,
	// so each org's verify matches against its own claim's token.
	dnsoverride.Set(&fakeTXTResolver{records: map[string][]string{
		ssoTXTHost("shared.com"): {claimA.TXTValue, claimB.TXTValue},
	}})

	aResp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+claimA.ID+"/verify", sidA, nil)
	if aResp.StatusCode != http.StatusOK {
		t.Fatalf("orgA verify status = %d; want 200 (body=%s)", aResp.StatusCode, readBody(aResp))
	}

	bResp := doInviteReq(r, http.MethodPost, "/api/sso/domains/"+claimB.ID+"/verify", sidB, nil)
	bRaw := readBody(bResp)
	if bResp.StatusCode != http.StatusConflict {
		t.Fatalf("orgB verify status = %d; want 409 (body=%s)", bResp.StatusCode, bRaw)
	}
	// Non-disclosure: the opaque message must not name the holding org.
	if strings.Contains(bRaw, orgA.String()) || strings.Contains(bRaw, "sso-glob-a") {
		t.Errorf("409 body leaked the holder org: %s", bRaw)
	}
	if !strings.Contains(bRaw, "already verified by another organization") {
		t.Errorf("409 body = %q; want the opaque cross-org message", bRaw)
	}

	// First-verify-wins: A verified, B still pending (its tx rolled back).
	var aVer, bVer sql.NullTime
	if err := r.h.AdminDB.QueryRow(`SELECT verified_at FROM sso_domains WHERE id=$1`, claimA.ID).Scan(&aVer); err != nil {
		t.Fatalf("read A verified_at: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`SELECT verified_at FROM sso_domains WHERE id=$1`, claimB.ID).Scan(&bVer); err != nil {
		t.Fatalf("read B verified_at: %v", err)
	}
	if !aVer.Valid {
		t.Error("orgA shared.com should be verified (won the race)")
	}
	if bVer.Valid {
		t.Error("orgB shared.com should remain pending (lost the race)")
	}
}

// TestSSODomainDelete_AndList: list surfaces the org's claims; delete unclaims
// (200) and a second delete of the gone id is a clean 404.
func TestSSODomainDelete_AndList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-del")
	seedSSOConnForTest(t, r.h, orgID.String())
	sid := r.signInAs(owner)

	_, claim, _ := ssoClaim(r, sid, "del.com")

	lresp := doInviteReq(r, http.MethodGet, "/api/sso/domains", sid, nil)
	lraw := readBody(lresp)
	if lresp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d; want 200 (body=%s)", lresp.StatusCode, lraw)
	}
	var list []ssoDomainJSON
	if err := json.Unmarshal([]byte(lraw), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].Domain != "del.com" {
		t.Fatalf("list = %+v; want one del.com entry", list)
	}

	dresp := doInviteReq(r, http.MethodDelete, "/api/sso/domains/"+claim.ID, sid, nil)
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d; want 200 (body=%s)", dresp.StatusCode, readBody(dresp))
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_domains WHERE id=$1`, claim.ID).Scan(&n); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if n != 0 {
		t.Errorf("rows after delete = %d; want 0", n)
	}

	d2 := doInviteReq(r, http.MethodDelete, "/api/sso/domains/"+claim.ID, sid, nil)
	if d2.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d; want 404 (already gone)", d2.StatusCode)
	}
}

// TestSSODomainRoutes_NonAdmin404: a plain org member gets 404 on every route
// (non-disclosure), and their attempted claim mints nothing.
func TestSSODomainRoutes_NonAdmin404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, teamID := r.seedOrg(owner, "sso-nonadmin")
	seedSSOConnForTest(t, r.h, orgID.String())
	ownerSid := r.signInAs(owner)
	_, claim, _ := ssoClaim(r, ownerSid, "member.com")

	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgID.String(), teamID.String(), "member", "member")
	sid := r.signInAs(member)

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/sso/domains", nil},
		{http.MethodPost, "/api/sso/domains", map[string]string{"domain": "x.com"}},
		{http.MethodPost, "/api/sso/domains/" + claim.ID + "/verify", nil},
		{http.MethodDelete, "/api/sso/domains/" + claim.ID, nil},
	}
	for _, tc := range cases {
		resp := doInviteReq(r, tc.method, tc.path, sid, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s as member: status = %d; want 404 (non-disclosure); body=%s",
				tc.method, tc.path, resp.StatusCode, readBody(resp))
		}
	}

	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_domains WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d; want 1 (member's POST created nothing, owner's row intact)", n)
	}
}
