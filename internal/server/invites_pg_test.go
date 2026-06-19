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
)

// ---------- grantOrgMembership (the TFAC-416 primitive) ----------

// TestGrantOrgMembership_OrgOnly: a NULL team writes only the
// org_memberships row — the deliberate "org member, zero teams" state an
// org-only invite produces.
func TestGrantOrgMembership_OrgOnly(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "grant-orgonly")
	invitee := pgtest.SeedUser(t, h, "grant-orgonly-invitee")

	if err := grantOrgMembership(context.Background(), h.AdminDB,
		uuid.MustParse(invitee), uuid.MustParse(orgID), "member",
		uuid.NullUUID{}, "member"); err != nil {
		t.Fatalf("grantOrgMembership: %v", err)
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

	if err := grantOrgMembership(context.Background(), h.AdminDB,
		uuid.MustParse(invitee), uuid.MustParse(orgID), "admin",
		uuid.NullUUID{UUID: uuid.MustParse(teamID), Valid: true}, "member"); err != nil {
		t.Fatalf("grantOrgMembership: %v", err)
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

// TestGrantOrgMembership_Idempotent: re-calling with the same args is a
// no-op (ON CONFLICT DO NOTHING) — no error, no duplicate rows.
func TestGrantOrgMembership_Idempotent(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "grant-idem")
	invitee := pgtest.SeedUser(t, h, "grant-idem-invitee")
	tid := uuid.NullUUID{UUID: uuid.MustParse(teamID), Valid: true}

	for i := 0; i < 2; i++ {
		if err := grantOrgMembership(context.Background(), h.AdminDB,
			uuid.MustParse(invitee), uuid.MustParse(orgID), "member", tid, "member"); err != nil {
			t.Fatalf("grantOrgMembership call %d: %v", i, err)
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

// TestInviteAccept_Valid_MintsMembershipAndStamps: a clean redeem mints the
// org membership, leaves the user team-less (target NULL), stamps
// accepted_at/accepted_by, and points the session at the org.
func TestInviteAccept_Valid_OrgOnly(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-orgonly")
	invitee := r.seedUser()
	email := invitee.String() + "@test"

	token := "tok-valid-" + uuid.NewString()
	inviteID := seedInvite(t, r.h, orgID.String(), email, "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	sid := r.signInAs(invitee)
	resp := postAccept(r, sid, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, readBody(resp))
	}

	if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 1 {
		t.Errorf("org_memberships = %d; want 1", n)
	}
	// Org-only: no team rows anywhere for the invitee.
	var teamRows int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM memberships WHERE user_id=$1`, invitee).Scan(&teamRows); err != nil {
		t.Fatalf("count team memberships: %v", err)
	}
	if teamRows != 0 {
		t.Errorf("team memberships = %d; want 0 (org-only invite)", teamRows)
	}
	assertAcceptedBy(t, r.h, inviteID, invitee.String())
}

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
		Error        string `json:"error"`
		InvitedEmail string `json:"invited_email"`
	}
	_ = json.Unmarshal([]byte(raw), &body)
	if body.InvitedEmail != "someone-else@test" {
		t.Errorf("invited_email = %q; want someone-else@test", body.InvitedEmail)
	}
	// The actionable message must steer toward the INVITED address, not the
	// caller's — re-inviting the account they're wrongly signed in as is the
	// misleading direction.
	if !strings.Contains(body.Error, "re-invite someone-else@test") {
		t.Errorf("error message %q; want it to re-invite the invited address", body.Error)
	}
	if strings.Contains(body.Error, "re-invite "+invitee.String()+"@test") {
		t.Errorf("error message re-invites the caller's address (misleading): %q", body.Error)
	}
	if n := countOrgMemberships(t, r.h, invitee.String(), orgID.String()); n != 0 {
		t.Errorf("org_memberships = %d; want 0 on mismatch", n)
	}
}

// TestInviteAccept_EmptyEmail_409: a JWT with no email claim can't match an
// invite → 409.
func TestInviteAccept_EmptyEmail_409(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "accept-noemail")
	invitee := r.seedUser()

	token := "tok-noemail-" + uuid.NewString()
	seedInvite(t, r.h, orgID.String(), invitee.String()+"@test", "member", "", sha256Of(token),
		time.Now().Add(24*time.Hour), false, false)

	claims := validClaimsFor(invitee)
	delete(claims, "email")
	sid := r.signInWithClaims(invitee, claims)
	resp := postAccept(r, sid, token)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", resp.StatusCode, readBody(resp))
	}
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

// signInAs completes the OAuth callback for userID with the default claims
// (email = userID@test) and returns the sid cookie value.
func (r *authRig) signInAs(userID uuid.UUID) string {
	return r.signInWithClaims(userID, validClaimsFor(userID))
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
	b, _ := json.Marshal(map[string]string{"token": token})
	req := httptest.NewRequest(http.MethodPost, "/api/invites/accept", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.srv.deployCfg.publicURL)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: r.srv.sidCookieName(), Value: sid})
	}
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
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
