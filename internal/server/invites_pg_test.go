package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// ---------- grantOrgMembership ----------

// TestGrantOrgMembership_OrgOnly: a NULL team writes only the
// org_memberships row — the deliberate "org member, zero teams" state an
// org-only invite produces.
func TestGrantOrgMembership_OrgOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "grant-orgonly")
	invitee := pgtest.SeedUser(t, h, "grant-orgonly-invitee")

	inserted, err := grantOrgMembership(context.Background(), h.AdminDB,
		uuid.MustParse(invitee), uuid.MustParse(orgID), "member",
		uuid.NullUUID{}, "member")
	if err != nil {
		t.Fatalf("grantOrgMembership: %v", err)
	}
	if !inserted {
		t.Error("orgRowInserted = false; want true (net-new org membership)")
	}

	if n := countOrgMemberships(t, h, invitee, orgID); n != 1 {
		t.Errorf("org_memberships rows = %d; want 1", n)
	}
	if n := countTeamMemberships(t, h, invitee, teamID); n != 0 {
		t.Errorf("team memberships rows = %d; want 0 (org-only invite)", n)
	}
}

// TestGrantOrgMembership_WithTeam: a valid team writes both the org and the
// team membership rows.
func TestGrantOrgMembership_WithTeam(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "grant-team")
	invitee := pgtest.SeedUser(t, h, "grant-team-invitee")

	inserted, err := grantOrgMembership(context.Background(), h.AdminDB,
		uuid.MustParse(invitee), uuid.MustParse(orgID), "admin",
		uuid.NullUUID{UUID: uuid.MustParse(teamID), Valid: true}, "member")
	if err != nil {
		t.Fatalf("grantOrgMembership: %v", err)
	}
	if !inserted {
		t.Error("orgRowInserted = false; want true (net-new org membership)")
	}

	if n := countOrgMemberships(t, h, invitee, orgID); n != 1 {
		t.Errorf("org_memberships rows = %d; want 1", n)
	}
	if n := countTeamMemberships(t, h, invitee, teamID); n != 1 {
		t.Errorf("team memberships rows = %d; want 1", n)
	}
	var role string
	if err := h.AdminDB.QueryRow(
		`SELECT role::text FROM org_memberships WHERE user_id=$1 AND org_id=$2`, invitee, orgID,
	).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "admin" {
		t.Errorf("org role = %q; want admin", role)
	}
}

// TestGrantOrgMembership_Idempotent: a repeated grant is a no-op with no
// duplicate rows, and orgRowInserted is true only for the first, net-new grant,
// which is what JIT provisioning audits on.
func TestGrantOrgMembership_Idempotent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "grant-idem")
	invitee := pgtest.SeedUser(t, h, "grant-idem-invitee")
	tid := uuid.NullUUID{UUID: uuid.MustParse(teamID), Valid: true}

	for i := 0; i < 2; i++ {
		inserted, err := grantOrgMembership(context.Background(), h.AdminDB,
			uuid.MustParse(invitee), uuid.MustParse(orgID), "member", tid, "member")
		if err != nil {
			t.Fatalf("grantOrgMembership call %d: %v", i, err)
		}
		// First call lands a net-new row (true); the second is an ON CONFLICT
		// DO NOTHING no-op (false) — the net-new gate.
		if want := i == 0; inserted != want {
			t.Errorf("call %d orgRowInserted = %v; want %v", i, inserted, want)
		}
	}
	if n := countOrgMemberships(t, h, invitee, orgID); n != 1 {
		t.Errorf("org_memberships rows = %d after 2 calls; want 1", n)
	}
	if n := countTeamMemberships(t, h, invitee, teamID); n != 1 {
		t.Errorf("team memberships rows = %d after 2 calls; want 1", n)
	}
}

// ---------- accept state machine (POST /api/invites/accept) ----------

// TestInviteAccept_Valid_WithTeam: target_team_id set adds the team
// membership as 'member'.
func TestInviteAccept_Valid_WithTeam(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, teamID := r.seedOrg(owner, "accept-team")
	invitee := r.seedUser()
	email := invitee.String() + "@test"

	token := "tok-team-" + uuid.NewString()
	seedInvite(t, r.h, orgID.String(), email, "member", teamID.String(), sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	sid := r.signInAs(invitee)
	resp := postAccept(r, sid, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if n := countTeamMemberships(t, r.h, invitee.String(), teamID.String()); n != 1 {
		t.Errorf("team memberships = %d; want 1", n)
	}
	var role string
	if err := r.h.AdminDB.QueryRow(
		`SELECT role::text FROM memberships WHERE user_id=$1 AND team_id=$2`, invitee, teamID,
	).Scan(&role); err != nil {
		t.Fatalf("read team role: %v", err)
	}
	if role != "member" {
		t.Errorf("team role = %q; want member", role)
	}
}

// TestInviteAccept_TerminalAndExpiry: revoked / accepted / expired each
// return their 409 and mint nothing.
func TestInviteAccept_TerminalAndExpiry(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	cases := []struct {
		name              string
		expiresIn         time.Duration
		accepted, revoked bool
	}{
		{"revoked", 24 * time.Hour, false, true},
		{"already_accepted", 24 * time.Hour, true, false},
		{"expired", -1 * time.Hour, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAuthRig(t)
			owner := r.seedUser()
			orgID, _ := r.seedOrg(owner, "accept-"+tc.name)
			invitee := r.seedUser()
			email := invitee.String() + "@test"
			token := "tok-" + tc.name + "-" + uuid.NewString()
			seedInvite(t, r.h, orgID.String(), email, "member", "", sha256Of(token),
				time.Now().Add(tc.expiresIn), tc.accepted, tc.revoked)

			sid := r.signInAs(invitee)
			resp := postAccept(r, sid, token)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d; want 409 (body=%s)", resp.StatusCode, readBody(resp))
			}
			// already_accepted seeds its own accepted state; the other two
			// must not have minted a membership.
			if !tc.accepted {
				if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 0 {
					t.Errorf("org_memberships = %d; want 0 (refused redeem)", n)
				}
			}
		})
	}
}

// TestInviteAccept_UnknownToken_404: a token matching no row is a 404.
func TestInviteAccept_UnknownToken_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	invitee := r.seedUser()
	sid := r.signInAs(invitee)
	resp := postAccept(r, sid, "no-such-token")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (body=%s)", resp.StatusCode, readBody(resp))
	}
}

// TestInviteAccept_EmailMismatch_409: signed in as someone other than the
// invited address → 409 carrying invited_email, no membership minted.
func TestInviteAccept_EmailMismatch_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-mismatch")
	invitee := r.seedUser()

	token := "tok-mismatch-" + uuid.NewString()
	seedInvite(t, r.h, orgID.String(), "someone-else@test", "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	sid := r.signInAs(invitee) // JWT email = invitee@test ≠ someone-else@test
	resp := postAccept(r, sid, token)
	raw := readBody(resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", resp.StatusCode, raw)
	}
	var body struct {
		Errors []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal([]byte(raw), &body)
	if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonInviteEmailMismatch {
		t.Fatalf("errors = %+v; want one INVITE_EMAIL_MISMATCH item", body.Errors)
	}
	if !strings.Contains(body.Errors[0].Message, "someone-else@test") {
		t.Errorf("message %q; want it to name the invited address", body.Errors[0].Message)
	}
	// The actionable message must steer toward the INVITED address, not the
	// caller's — re-inviting the account they're wrongly signed in as is the
	// misleading direction.
	if !strings.Contains(body.Errors[0].Message, "re-invite someone-else@test") {
		t.Errorf("error message %q; want it to re-invite the invited address", body.Errors[0].Message)
	}
	if strings.Contains(body.Errors[0].Message, "re-invite "+invitee.String()+"@test") {
		t.Errorf("error message re-invites the caller's address (misleading): %q", body.Errors[0].Message)
	}
	if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 0 {
		t.Errorf("org_memberships = %d; want 0 on mismatch", n)
	}
}

// TestInviteAccept_EmptyEmail_409: a JWT with no email claim, from a principal
// holding no identity with the invited address → 409, and the message names
// the missing address readably rather than "signed in as ."
func TestInviteAccept_EmptyEmail_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-noemail")
	invitee := r.seedUser()

	token := "tok-noemail-" + uuid.NewString()
	seedInvite(t, r.h, orgID.String(), "elsewhere@test", "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	claims := validClaimsFor(invitee)
	delete(claims, "email")
	sid := r.signInWithClaims(invitee, claims)
	resp := postAccept(r, sid, token)
	raw := readBody(resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "an account with no email") {
		t.Errorf("body %s; want the placeholder for a caller with no email", raw)
	}
}

// TestInviteAccept_UntrustedSSOIdentity_409: an SSO sign-in carrying the invited
// address on an unverified domain cannot accept, because acceptance matches on
// TF's identity record, not the JWT's email claim. The invite stays pending.
func TestInviteAccept_UntrustedSSOIdentity_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-untrusted-sso")

	const invited = "carol@corp.example"
	token := "tok-untrusted-" + uuid.NewString()
	inviteID := seedInvite(t, r.h, orgID.String(), invited, "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	// No login extension is registered in this package, so the callback's
	// domain trust check answers no, as ee/sso does for an unverified domain.
	samlAuth := uuid.New()
	r.h.SeedAuthUser(t, samlAuth.String(), samlAuth.String()+"@auth.test")
	claims := validClaimsFor(samlAuth)
	claims["email"] = invited
	claims["email_verified"] = true
	resp := r.driveSSOCallbackClaims(claims, uuid.NewString())
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sso callback status = %d; want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	accept := postAccept(r, sid, token)
	raw := readBody(accept)
	if accept.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", accept.StatusCode, raw)
	}
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	_ = json.Unmarshal([]byte(raw), &body)
	if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonInviteEmailMismatch {
		t.Fatalf("errors = %+v; want one INVITE_EMAIL_MISMATCH item", body.Errors)
	}
	var accepted bool
	if err := r.h.AdminDB.QueryRow(
		`SELECT accepted_at IS NOT NULL FROM org_invites WHERE id = $1`, inviteID,
	).Scan(&accepted); err != nil {
		t.Fatalf("read invite: %v", err)
	}
	if accepted {
		t.Error("invite was stamped accepted; want it still pending")
	}
}

// TestInviteAccept_TrustedLinkedIdentity_Accepted: an invitee signed in with
// GitHub under one address, holding the invited address verified on another of
// their identities, accepts the org-only invite and joins no team.
func TestInviteAccept_TrustedLinkedIdentity_Accepted(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-linked")
	invitee := r.seedUser()
	const workEmail = "invitee@corp.example"
	seedExtraIdentity(t, r.h, invitee.String(), workEmail, true)

	token := "tok-linked-" + uuid.NewString()
	inviteID := seedInvite(t, r.h, orgID.String(), workEmail, "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	sid := r.signInAs(invitee) // GitHub; email claim = invitee@test
	resp := postAccept(r, sid, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}
	if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 1 {
		t.Errorf("org_memberships = %d; want 1", n)
	}
	var teamRows int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM memberships WHERE user_id=$1`, invitee).Scan(&teamRows); err != nil {
		t.Fatalf("count team memberships: %v", err)
	}
	if teamRows != 0 {
		t.Errorf("team memberships = %d; want 0 (org-only invite)", teamRows)
	}
	assertAcceptedBy(t, r.h, inviteID, invitee.String())
}

// TestInviteAccept_AlreadyMember_Idempotent200: an invitee who is already an
// org member gets a 200 (idempotent) and the invite stamped accepted.
func TestInviteAccept_AlreadyMember_Idempotent200(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, teamID := r.seedOrg(owner, "accept-member")
	invitee := r.seedUser()
	pgtest.AddOrgMember(t, r.h, invitee.String(), orgID.String(), teamID.String(), "member", "member")
	email := invitee.String() + "@test"

	token := "tok-member-" + uuid.NewString()
	inviteID := seedInvite(t, r.h, orgID.String(), email, "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	sid := r.signInAs(invitee)
	resp := postAccept(r, sid, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 idempotent (body=%s)", resp.StatusCode, readBody(resp))
	}
	if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 1 {
		t.Errorf("org_memberships = %d; want 1 (no duplicate)", n)
	}
	assertAcceptedBy(t, r.h, inviteID, invitee.String())
}

// ---------- helpers ----------

func sha256Of(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// ---------- admin-surface handler tests (create/list/revoke/preview) ----------

// TestInviteCreate_CrossOrgTargetTeam_400: a target_team_id from another org is
// refused with 400 and mints nothing. RLS does not check that the team is in
// the org, so this handler check is the only boundary.
func TestInviteCreate_CrossOrgTargetTeam_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "create-orga")
	ownerB := r.seedUser()
	_, teamB := r.seedOrg(ownerB, "create-orgb")
	sid := r.signInAs(ownerA)

	resp := r.postJSONWithSid(http.MethodPost, "/api/invites", sid, map[string]string{
		"email": "x@example.com", "role": "member", "target_team_id": teamB.String(),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body=%s)", resp.StatusCode, readBody(resp))
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM org_invites WHERE org_id=$1`, orgA).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("invites created = %d; want 0 (cross-org team rejected before insert)", n)
	}
}

// TestInviteCreate_ExistingMemberGuard: an address that maps to a current member
// of this org through a verified identity is refused with 409 and mints nothing,
// whatever the member's role. A member of another org, or an address held only
// on an unverified identity, can be invited.
func TestInviteCreate_ExistingMemberGuard(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	for _, tc := range []struct {
		name string
		// invitee seeds whoever holds the address and returns the address.
		invitee    func(r *authRig, owner, orgID, teamID uuid.UUID) string
		wantStatus int
		wantRows   int
	}{
		{"the owner", func(r *authRig, owner, _, _ uuid.UUID) string {
			return owner.String() + "@test"
		}, http.StatusConflict, 0},
		{"a non-owner member", func(r *authRig, _, orgID, teamID uuid.UUID) string {
			member := r.seedUser()
			pgtest.AddOrgMember(r.t, r.h, member.String(), orgID.String(), teamID.String(), "member", "member")
			return member.String() + "@test"
		}, http.StatusConflict, 0},
		{"a member of another org", func(r *authRig, _, _, _ uuid.UUID) string {
			other := r.seedUser()
			r.seedOrg(other, "create-guard-other")
			return other.String() + "@test"
		}, http.StatusCreated, 1},
		{"a member whose identity with the address is unverified", func(r *authRig, _, orgID, teamID uuid.UUID) string {
			// seedUser's own <id>@test identity is verified; the invited address
			// is on a second, unverified one.
			member := r.seedUser()
			pgtest.AddOrgMember(r.t, r.h, member.String(), orgID.String(), teamID.String(), "member", "member")
			email := "unverified-" + member.String() + "@example.com"
			seedExtraIdentity(r.t, r.h, member.String(), email, false)
			return email
		}, http.StatusCreated, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAuthRig(t)
			owner := r.seedUser()
			orgID, teamID := r.seedOrg(owner, "create-guard")
			email := tc.invitee(r, owner, orgID, teamID)
			// signInAs reports the address verified: the login refreshes
			// email_verified from the claim, and an unverified claim would
			// downgrade the owner's seeded identity.
			sid := r.signInAs(owner)

			resp := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
				map[string]string{"email": email, "role": "member"})
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d; want %d (body=%s)", resp.StatusCode, tc.wantStatus, readBody(resp))
			}
			var n int
			if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM org_invites WHERE org_id=$1`, orgID).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != tc.wantRows {
				t.Errorf("invites created = %d; want %d", n, tc.wantRows)
			}
		})
	}
}

// seedExtraIdentity attaches a second user_identities row, with its own
// auth.users row (auth_user_id is the table's key), carrying email with the
// given verified flag.
func seedExtraIdentity(t *testing.T, h *pgtest.Harness, userID, email string, verified bool) {
	t.Helper()
	authID := uuid.NewString()
	h.SeedAuthUser(t, authID, email)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'saml', $3, $4)
	`, authID, userID, email, verified); err != nil {
		t.Fatalf("seed extra identity: %v", err)
	}
}

// TestInviteCreate_PendingDup_409: a second pending invite to the same
// address is a 409.
func TestInviteCreate_PendingDup_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "create-dup")
	sid := r.signInAs(owner)

	first := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "dup@example.com", "role": "member"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d; want 201 (body=%s)", first.StatusCode, readBody(first))
	}
	second := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "dup@example.com", "role": "admin"})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second create status = %d; want 409 (body=%s)", second.StatusCode, readBody(second))
	}
}

// TestInviteCreate_NonAdminIsForbidden: a plain member cannot create invites,
// and is told so with 403 rather than 404 since they can see their own org.
func TestInviteCreate_NonAdminIsForbidden(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, teamID := r.seedOrg(owner, "create-nonadmin")
	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgID.String(), teamID.String(), "member", "member")
	sid := r.signInAs(member)

	resp := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "x@example.com", "role": "member"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (non-admin); body=%s", resp.StatusCode, readBody(resp))
	}
}

// TestInviteList_ReturnsActive: the list and the single read answer with the
// created invite, its address lower-cased, and without the accept URL, which
// exists only in the create response.
func TestInviteList_ReturnsActive(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "list-active")
	sid := r.signInAs(owner)

	_ = r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "Listed@Example.com", "role": "member"})

	resp := r.postJSONWithSid(http.MethodPost, "/api/invites/list", sid, map[string]any{})
	raw := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"items"`
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 || page.TotalCount != 1 || page.Items[0].Email != "listed@example.com" {
		t.Fatalf("list = %+v (total %d); want one entry for listed@example.com", page.Items, page.TotalCount)
	}

	single := r.postJSONWithSid(http.MethodGet, "/api/invites/"+page.Items[0].ID, sid, nil)
	singleRaw := readBody(single)
	if single.StatusCode != http.StatusOK {
		t.Fatalf("single read status = %d; want 200 (body=%s)", single.StatusCode, singleRaw)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(singleRaw), &got); err != nil {
		t.Fatalf("decode single read: %v", err)
	}
	if got["email"] != "listed@example.com" || got["id"] != page.Items[0].ID {
		t.Errorf("single read = %v; want the listed invite", got)
	}
	for _, secret := range []string{"accept_url", "token", "token_hash"} {
		if _, present := got[secret]; present {
			t.Errorf("single read carries %q; the accept link is create-response-only", secret)
		}
	}

	// An invite id from another org, and a well-formed id that names nothing,
	// are both 404 — the read is scoped to the same active set the list is.
	if miss := r.postJSONWithSid(http.MethodGet, "/api/invites/11111111-1111-1111-1111-111111111111", sid, nil); miss.StatusCode != http.StatusNotFound {
		t.Errorf("unknown invite id = %d; want 404", miss.StatusCode)
	}
}

// TestInviteRevoke_RemovesFromList: revoking a pending invite returns 200 and
// drops it from the active list.
func TestInviteRevoke_RemovesFromList(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "revoke-http")
	sid := r.signInAs(owner)

	createResp := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "rev@example.com", "role": "member"})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(readBody(createResp)), &created)

	revResp := r.postJSONWithSid(http.MethodPost, "/api/invites/"+created.ID+"/revoke", sid, nil)
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d; want 200 (body=%s)", revResp.StatusCode, readBody(revResp))
	}
	listResp := r.postJSONWithSid(http.MethodPost, "/api/invites/list", sid, map[string]any{})
	var page struct {
		Items      []json.RawMessage `json:"items"`
		TotalCount int               `json:"total_count"`
	}
	_ = json.Unmarshal([]byte(readBody(listResp)), &page)
	if len(page.Items) != 0 || page.TotalCount != 0 {
		t.Errorf("active invites after revoke = %d (total %d); want 0", len(page.Items), page.TotalCount)
	}
	// The single read drops it too: a revoked invite is not an active one.
	if gone := r.postJSONWithSid(http.MethodGet, "/api/invites/"+created.ID, sid, nil); gone.StatusCode != http.StatusNotFound {
		t.Errorf("single read of a revoked invite = %d; want 404", gone.StatusCode)
	}
}

// TestInvitePreview_ValidAndUnknown: the create response carries an accept URL
// and an expiry, the unauthenticated preview resolves its token to status=valid
// with the org name, and an unknown token to status=not_found with nothing leaked.
func TestInvitePreview_ValidAndUnknown(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "preview-http")
	sid := r.signInAs(owner)

	createResp := r.postJSONWithSid(http.MethodPost, "/api/invites", sid,
		map[string]string{"email": "prev@example.com", "role": "member"})
	var created struct {
		AcceptURL string `json:"accept_url"`
		ExpiresAt string `json:"expires_at"`
	}
	_ = json.Unmarshal([]byte(readBody(createResp)), &created)
	if !strings.Contains(created.AcceptURL, "/invite/accept?token=") || created.ExpiresAt == "" {
		t.Errorf("create response = %+v; want an accept_url carrying the token and an expires_at", created)
	}
	token := tokenFromAcceptURL(t, created.AcceptURL)

	// Preview is unauthenticated — no sid.
	resp := r.postJSONWithSid(http.MethodGet, "/api/invites/preview?token="+url.QueryEscape(token), "", nil)
	raw := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, raw)
	}
	var pv struct {
		OrgName string `json:"org_name"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &pv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pv.Status != "valid" {
		t.Errorf("status = %q; want valid", pv.Status)
	}
	if pv.OrgName == "" {
		t.Error("org_name empty; want the org's name for the token holder")
	}

	unknown := r.postJSONWithSid(http.MethodGet, "/api/invites/preview?token=garbage", "", nil)
	var upv struct {
		OrgName string `json:"org_name"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal([]byte(readBody(unknown)), &upv)
	if upv.Status != "not_found" {
		t.Errorf("unknown token status = %q; want not_found", upv.Status)
	}
	if upv.OrgName != "" {
		t.Errorf("unknown token leaked org_name = %q; want empty", upv.OrgName)
	}
}

// ---------- helpers ----------

// signInAs completes a GitHub OAuth callback for userID with email userID@test
// reported verified and returns the sid. The flag keeps the seeded identity
// holding its address after the login refresh, which acceptance matches on.
func (r *authRig) signInAs(userID uuid.UUID) string {
	claims := validClaimsFor(userID)
	claims["email_verified"] = true
	return r.signInWithClaims(userID, claims)
}

// tokenFromAcceptURL pulls the ?token= value out of a create response's
// accept_url.
func tokenFromAcceptURL(t *testing.T, acceptURL string) string {
	t.Helper()
	u, err := url.Parse(acceptURL)
	if err != nil {
		t.Fatalf("parse accept_url %q: %v", acceptURL, err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("accept_url %q carried no token", acceptURL)
	}
	return tok
}

// signInWithClaims mirrors driveCallback but lets the caller shape the JWT
// claims (e.g. drop email) before the session is minted.
func (r *authRig) signInWithClaims(userID uuid.UUID, claims jwt.MapClaims) string {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, claims)
	refresh := "refresh-" + uuid.NewString()
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, refresh, time.Now().Add(time.Hour).Unix(), nil
	}
	csrf := "csrf-" + uuid.NewString()
	stateVal := r.signStateCookie("/dashboard", csrf, "pkce-verifier")
	q := url.Values{}
	q.Set("state", csrf)
	q.Set("code", "code-"+uuid.NewString())
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateVal})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return r.sidFromResp(rec.Result())
}

func postAccept(r *authRig, sid, token string) *http.Response {
	r.t.Helper()
	return r.postJSONWithSid(http.MethodPost, "/api/invites/accept", sid,
		map[string]string{"token": token})
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	var sb bytes.Buffer
	_, _ = sb.ReadFrom(resp.Body)
	return sb.String()
}

// seedInvite inserts an org_invites fixture via the admin pool (bypasses
// RLS) and returns its id. targetTeamID "" → NULL.
func seedInvite(t *testing.T, h *pgtest.Harness, orgID, email, role, targetTeamID string, tokenHash []byte, expiresAt time.Time, accepted, revoked bool) string {
	t.Helper()
	var team, acceptedAt, revokedAt any
	if targetTeamID != "" {
		team = targetTeamID
	}
	if accepted {
		acceptedAt = time.Now()
	}
	if revoked {
		revokedAt = time.Now()
	}
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO org_invites (org_id, email, role, target_team_id, token_hash, expires_at, accepted_at, revoked_at)
		VALUES ($1, lower($2), $3, $4, $5, $6, $7, $8)
		RETURNING id::text
	`, orgID, email, role, team, tokenHash, expiresAt, acceptedAt, revokedAt).Scan(&id); err != nil {
		t.Fatalf("seed invite: %v", err)
	}
	return id
}

func countOrgMemberships(t *testing.T, h *pgtest.Harness, userID, orgID string) int {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM org_memberships WHERE user_id=$1 AND org_id=$2`, userID, orgID,
	).Scan(&n); err != nil {
		t.Fatalf("count org_memberships: %v", err)
	}
	return n
}

func countTeamMemberships(t *testing.T, h *pgtest.Harness, userID, teamID string) int {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM memberships WHERE user_id=$1 AND team_id=$2`, userID, teamID,
	).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}

func assertAcceptedBy(t *testing.T, h *pgtest.Harness, inviteID, userID string) {
	t.Helper()
	var acceptedAt *time.Time
	var acceptedBy *string
	if err := h.AdminDB.QueryRow(
		`SELECT accepted_at, accepted_by::text FROM org_invites WHERE id=$1`, inviteID,
	).Scan(&acceptedAt, &acceptedBy); err != nil {
		t.Fatalf("read accepted columns: %v", err)
	}
	if acceptedAt == nil {
		t.Error("accepted_at not stamped")
	}
	if acceptedBy == nil || *acceptedBy != userID {
		t.Errorf("accepted_by = %v; want %s", acceptedBy, userID)
	}
}
