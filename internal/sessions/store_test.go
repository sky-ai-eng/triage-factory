package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// seedUser inserts the minimum FK chain (auth.users → public.users)
// needed for a sessions row to be insertable. Mirrors the helper in
// pgtest/baseline_test.go but lives here because that helper is
// test-package-private. If a third caller materializes this should
// move into pgtest as an exported helper.
func seedUser(t *testing.T, h *pgtest.Harness) uuid.UUID {
	t.Helper()
	var idStr string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	h.SeedAuthUser(t, idStr, idStr+"@test")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, idStr, "test-user"); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	return uuid.MustParse(idStr)
}

func newStoreForTest(t *testing.T) (*Store, *pgtest.Harness, uuid.UUID) {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	uid := seedUser(t, h)
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return NewStore(h.AdminDB, k), h, uid
}

func TestStore_CreateLookupRoundtrip(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()

	jwt, refresh := "fake.jwt.token", "fake-refresh-token"
	jwtExp := time.Now().Add(1 * time.Hour).UTC()
	sessExp := time.Now().Add(30 * 24 * time.Hour).UTC()

	created, err := store.CreateSystem(ctx, uid, jwt, refresh, jwtExp, sessExp, "test-ua", "127.0.0.1", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("Create returned nil id")
	}

	got, err := store.LookupSystem(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil {
		t.Fatal("Lookup returned nil for existing session")
	}
	if got.JWT != jwt {
		t.Errorf("JWT mismatch: got %q want %q", got.JWT, jwt)
	}
	if got.RefreshToken != refresh {
		t.Errorf("refresh mismatch: got %q want %q", got.RefreshToken, refresh)
	}
	if got.UserID != uid {
		t.Errorf("user_id mismatch: got %s want %s", got.UserID, uid)
	}
	if got.UserAgent != "test-ua" {
		t.Errorf("user_agent: got %q want %q", got.UserAgent, "test-ua")
	}
	if got.IPAddr != "127.0.0.1" {
		t.Errorf("ip_addr: got %q want %q", got.IPAddr, "127.0.0.1")
	}
}

func TestStore_CiphertextAtRest(t *testing.T) {
	// Acceptance bullet: SELECT jwt_enc with the master key absent
	// yields ciphertext, not plaintext.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	plainJWT := "header.payload.signature"
	created, err := store.CreateSystem(ctx, uid, plainJWT, "ref",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var jwtEnc []byte
	if err := h.AdminDB.QueryRow(
		`SELECT jwt_enc FROM public.sessions WHERE id = $1`, created.ID,
	).Scan(&jwtEnc); err != nil {
		t.Fatalf("raw select jwt_enc: %v", err)
	}
	if len(jwtEnc) == 0 {
		t.Fatal("jwt_enc empty")
	}
	if string(jwtEnc) == plainJWT {
		t.Fatal("jwt_enc stored as plaintext — encryption not applied")
	}
	// Ensure the JWT bytes don't appear anywhere in the ciphertext as
	// a contiguous substring (loose canary, defends against accidental
	// "encrypt only metadata" bugs).
	for i := 0; i+len(plainJWT) <= len(jwtEnc); i++ {
		if string(jwtEnc[i:i+len(plainJWT)]) == plainJWT {
			t.Fatal("plaintext JWT substring found in stored ciphertext")
		}
	}
}

func TestStore_Lookup_NotFound(t *testing.T) {
	store, _, _ := newStoreForTest(t)
	got, err := store.LookupSystem(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned non-nil for missing session")
	}
}

func TestStore_Lookup_FiltersRevoked(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeSystem(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, err := store.LookupSystem(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned revoked session")
	}
}

func TestStore_Lookup_FiltersExpired(t *testing.T) {
	// Acceptance bullet: force-expiry test. Even if jwt_expires_at is
	// still future, expires_at in the past forces re-login.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate the session's outer expiry directly. The CHECK constraint
	// requires expires_at > created_at, so we have to push created_at
	// further into the past to satisfy it. Also push jwt_expires_at
	// (jwt_expires_at <= expires_at).
	if _, err := h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '2 hours',
		       jwt_expires_at = now() - interval '1 hour 30 minutes',
		       expires_at     = now() - interval '1 minute'
		 WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	got, err := store.LookupSystem(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned expired session")
	}
}

func TestStore_UpdateJWT(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "old-jwt", "old-ref",
		time.Now().Add(1*time.Minute), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newExp := time.Now().Add(2 * time.Hour).UTC()
	if err := store.UpdateJWTSystem(ctx, c.ID, "new-jwt", "new-ref", newExp); err != nil {
		t.Fatalf("UpdateJWT: %v", err)
	}

	got, err := store.LookupSystem(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.JWT != "new-jwt" {
		t.Errorf("JWT not rotated: got %q", got.JWT)
	}
	if got.RefreshToken != "new-ref" {
		t.Errorf("refresh not rotated: got %q", got.RefreshToken)
	}
	if got.JWTExpiresAt.Unix() != newExp.Unix() {
		t.Errorf("jwt_expires_at not rotated: got %v want %v", got.JWTExpiresAt, newExp)
	}
}

func TestStore_UpdateJWT_OnRevokedReturnsErr(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeSystem(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	err = store.UpdateJWTSystem(ctx, c.ID, "x", "y", time.Now().Add(1*time.Hour))
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("expected ErrSessionGone, got %v", err)
	}
}

func TestStore_Revoke_PreservesRow(t *testing.T) {
	// Acceptance bullet: logout flips revoked_at; row persists for audit.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeSystem(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var revokedAt sql.NullTime
	if err := h.AdminDB.QueryRow(
		`SELECT revoked_at FROM public.sessions WHERE id = $1`, c.ID,
	).Scan(&revokedAt); err != nil {
		t.Fatalf("post-revoke select: %v", err)
	}
	if !revokedAt.Valid {
		t.Fatal("row revoked but revoked_at is NULL")
	}
}

func TestStore_Revoke_Idempotent(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeSystem(ctx, c.ID); err != nil {
		t.Fatalf("Revoke #1: %v", err)
	}
	if err := store.RevokeSystem(ctx, c.ID); err != nil {
		t.Fatalf("Revoke #2: %v", err)
	}
}

func TestStore_TouchLastSeen(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate last_seen_at so we can detect the bump.
	if _, err := h.AdminDB.Exec(
		`UPDATE public.sessions SET last_seen_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := store.TouchLastSeenSystem(ctx, c.ID); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}
	var lastSeen time.Time
	if err := h.AdminDB.QueryRow(
		`SELECT last_seen_at FROM public.sessions WHERE id = $1`, c.ID,
	).Scan(&lastSeen); err != nil {
		t.Fatalf("post-touch select: %v", err)
	}
	if time.Since(lastSeen) > 1*time.Minute {
		t.Fatalf("last_seen_at not refreshed: %v ago", time.Since(lastSeen))
	}
}

func TestStore_ListActiveForUser_AndRevokeAll(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	// Three sessions for the same user. Mix of states:
	//   active1, active2 — show up in ListActive
	//   revoked          — pre-revoked, filtered out
	active1, _ := store.CreateSystem(ctx, uid, "jwt-1", "ref-1",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "ua-1", "1.1.1.1", uuid.NullUUID{})
	active2, _ := store.CreateSystem(ctx, uid, "jwt-2", "ref-2",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "ua-2", "2.2.2.2", uuid.NullUUID{})
	revoked, _ := store.CreateSystem(ctx, uid, "jwt-3", "ref-3",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err := store.RevokeSystem(ctx, revoked.ID); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}

	// Another user's session — must NOT appear in our list.
	other := seedUser(t, h)
	otherSess, _ := store.CreateSystem(ctx, other, "jwt-other", "ref-other",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})

	got, err := store.ListActiveForUserSystem(ctx, uid)
	if err != nil {
		t.Fatalf("ListActiveForUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (active1 + active2 only)", len(got))
	}
	// Both decrypted — list shouldn't return ciphertext.
	seenJWTs := map[string]bool{}
	for _, s := range got {
		seenJWTs[s.JWT] = true
		if s.UserID != uid {
			t.Errorf("session %s has user_id %s, want %s", s.ID, s.UserID, uid)
		}
	}
	for _, want := range []string{"jwt-1", "jwt-2"} {
		if !seenJWTs[want] {
			t.Errorf("missing decrypted jwt %q in list", want)
		}
	}

	// Revoke all for uid. Returns 2 (active1 + active2).
	n, err := store.RevokeAllForUserSystem(ctx, uid)
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d rows, want 2", n)
	}

	// Both active sessions are now unfindable via Lookup.
	for _, sess := range []*Session{active1, active2} {
		got, err := store.LookupSystem(ctx, sess.ID)
		if err != nil {
			t.Fatalf("post-revoke Lookup: %v", err)
		}
		if got != nil {
			t.Errorf("session %s still active after revoke-all", sess.ID)
		}
	}

	// Other user's session is untouched.
	stillThere, err := store.LookupSystem(ctx, otherSess.ID)
	if err != nil {
		t.Fatalf("other-user Lookup: %v", err)
	}
	if stillThere == nil {
		t.Error("revoke-all bled across users — other user's session got revoked")
	}

	// Calling RevokeAllForUser again is a no-op (idempotent).
	n2, err := store.RevokeAllForUserSystem(ctx, uid)
	if err != nil {
		t.Fatalf("RevokeAllForUser #2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second revoke-all returned %d, want 0 (idempotent)", n2)
	}
}

// seedOrg inserts an org owned by ownerID and returns its UUID. The
// FK on sessions.active_org_id requires a real public.orgs row, so the
// active-org tests below seed one before passing it to CreateSystem.
func seedOrg(t *testing.T, h *pgtest.Harness, ownerID uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	var oID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1, $1, $2) RETURNING id::text
	`, slug, ownerID).Scan(&oID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
		ownerID, oID); err != nil {
		t.Fatalf("insert org_membership: %v", err)
	}
	return uuid.MustParse(oID)
}

// TestStore_CreateLookupRoundtrip_WithActiveOrg pins SKY-313's storage
// path: a valid active_org_id at create time round-trips through
// Lookup so the middleware can lift it into ctxKeyOrgID.
func TestStore_CreateLookupRoundtrip_WithActiveOrg(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	orgID := seedOrg(t, h, uid, "active-org-test")
	active := uuid.NullUUID{UUID: orgID, Valid: true}

	created, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", active)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.ActiveOrgID.Valid || created.ActiveOrgID.UUID != orgID {
		t.Errorf("Create returned ActiveOrgID=%+v, want valid=%s", created.ActiveOrgID, orgID)
	}

	got, err := store.LookupSystem(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil {
		t.Fatal("Lookup nil")
	}
	if !got.ActiveOrgID.Valid || got.ActiveOrgID.UUID != orgID {
		t.Errorf("Lookup ActiveOrgID=%+v, want valid=%s", got.ActiveOrgID, orgID)
	}
}

// TestStore_CreateLookupRoundtrip_NullActiveOrg pins the zero-membership
// case: a user with no orgs gets a session whose active_org_id is NULL,
// and Lookup surfaces that as Valid=false rather than the zero UUID.
func TestStore_CreateLookupRoundtrip_NullActiveOrg(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()

	created, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ActiveOrgID.Valid {
		t.Errorf("Create returned ActiveOrgID valid for NULL input")
	}

	got, err := store.LookupSystem(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ActiveOrgID.Valid {
		t.Errorf("Lookup returned ActiveOrgID valid for NULL row, got %s", got.ActiveOrgID.UUID)
	}
}

// TestStore_UpdateActiveOrg pins the switch path: a session created
// with one active org can be swapped to a different org, and the next
// Lookup reflects the change. This is the storage side of the
// POST /api/me/active-org endpoint.
func TestStore_UpdateActiveOrg(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	orgA := seedOrg(t, h, uid, "org-a")
	orgB := seedOrg(t, h, uid, "org-b")

	created, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "",
		uuid.NullUUID{UUID: orgA, Valid: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.UpdateActiveOrgSystem(ctx, created.ID, orgB); err != nil {
		t.Fatalf("UpdateActiveOrg: %v", err)
	}

	got, err := store.LookupSystem(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.ActiveOrgID.Valid || got.ActiveOrgID.UUID != orgB {
		t.Errorf("post-update ActiveOrgID=%+v, want %s", got.ActiveOrgID, orgB)
	}
}

// TestStore_UpdateActiveOrg_FromNull pins the "user just got their first
// org membership" case: a session created with NULL active_org_id can
// be updated to a valid org without a separate Create.
func TestStore_UpdateActiveOrg_FromNull(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	created, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	orgID := seedOrg(t, h, uid, "later-org")
	if err := store.UpdateActiveOrgSystem(ctx, created.ID, orgID); err != nil {
		t.Fatalf("UpdateActiveOrg: %v", err)
	}

	got, err := store.LookupSystem(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.ActiveOrgID.Valid || got.ActiveOrgID.UUID != orgID {
		t.Errorf("post-update ActiveOrgID=%+v, want %s", got.ActiveOrgID, orgID)
	}
}

// TestStore_UpdateActiveOrg_OnRevokedReturnsNoRows pins the safety
// posture: UpdateActiveOrgSystem won't silently succeed against a
// revoked session. The handler turns sql.ErrNoRows into 401.
func TestStore_UpdateActiveOrg_OnRevokedReturnsNoRows(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	orgID := seedOrg(t, h, uid, "rev-org")
	created, err := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.RevokeSystem(ctx, created.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	err = store.UpdateActiveOrgSystem(ctx, created.ID, orgID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateActiveOrg on revoked: err=%v, want sql.ErrNoRows", err)
	}
}

// TestStore_RevokeForUserInOrg pins TFAC-487's deprovisioning revoke:
// RevokeForUserInOrgSystem revokes only the rows matching BOTH user_id and
// active_org_id, leaves the same user's session in another org and another
// user's session in the same org alone, and is idempotent on already-revoked
// rows.
func TestStore_RevokeForUserInOrg(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	orgA := seedOrg(t, h, uid, "revoke-org-a")
	orgB := seedOrg(t, h, uid, "revoke-org-b")

	// mustCreate inserts a session for user in org and asserts success, so a
	// CreateSystem failure (schema/constraint regression) surfaces here with a
	// clear message rather than as a confusing nil-deref later in the test.
	mustCreate := func(user uuid.UUID, jwt string, org uuid.UUID) *Session {
		t.Helper()
		s, err := store.CreateSystem(ctx, user, jwt, "r",
			time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "",
			uuid.NullUUID{UUID: org, Valid: true})
		if err != nil {
			t.Fatalf("create session (user=%s org=%s): %v", user, org, err)
		}
		return s
	}

	// The target user's sessions: two in orgA (both should be revoked) and
	// one in orgB (must survive — org-scoped revoke leaves other orgs alone).
	a1 := mustCreate(uid, "a1", orgA)
	a2 := mustCreate(uid, "a2", orgA)
	bSess := mustCreate(uid, "b", orgB)

	// Another user active in orgA — same org, different user; must survive.
	other := seedUser(t, h)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		other, orgA); err != nil {
		t.Fatalf("seed other membership: %v", err)
	}
	otherSess := mustCreate(other, "o", orgA)

	n, err := store.RevokeForUserInOrgSystem(ctx, uid, orgA)
	if err != nil {
		t.Fatalf("RevokeForUserInOrg: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d rows, want 2 (uid's two orgA sessions)", n)
	}

	// The two orgA sessions are now unfindable.
	for _, sess := range []*Session{a1, a2} {
		got, err := store.LookupSystem(ctx, sess.ID)
		if err != nil {
			t.Fatalf("post-revoke Lookup: %v", err)
		}
		if got != nil {
			t.Errorf("session %s still active after org-scoped revoke", sess.ID)
		}
	}
	// uid's orgB session survives (org-scoped revoke).
	if got, err := store.LookupSystem(ctx, bSess.ID); err != nil {
		t.Fatalf("orgB Lookup: %v", err)
	} else if got == nil {
		t.Error("org-scoped revoke bled across orgs — uid's orgB session got revoked")
	}
	// Other user's orgA session survives (user-scoped).
	if got, err := store.LookupSystem(ctx, otherSess.ID); err != nil {
		t.Fatalf("other-user Lookup: %v", err)
	} else if got == nil {
		t.Error("org-scoped revoke bled across users — other user's orgA session got revoked")
	}

	// Idempotent: a second call finds nothing left to revoke.
	n2, err := store.RevokeForUserInOrgSystem(ctx, uid, orgA)
	if err != nil {
		t.Fatalf("RevokeForUserInOrg #2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second revoke returned %d, want 0 (idempotent)", n2)
	}
}

// TestStore_UpdateActiveOrg_OnMissingReturnsNoRows pins the same
// posture for a session that never existed.
func TestStore_UpdateActiveOrg_OnMissingReturnsNoRows(t *testing.T) {
	store, _, _ := newStoreForTest(t)
	err := store.UpdateActiveOrgSystem(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateActiveOrg on missing: err=%v, want sql.ErrNoRows", err)
	}
}

func TestStore_ReapExpired(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	// Three rows:
	//   keep — fresh, non-revoked
	//   reap-rev — revoked 31 days ago (older than retention)
	//   reap-exp — expired 31 days ago, never revoked
	keep, _ := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	reapRev, _ := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})
	reapExp, _ := store.CreateSystem(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "", uuid.NullUUID{})

	if _, err := h.AdminDB.Exec(
		`UPDATE public.sessions SET revoked_at = now() - interval '31 days' WHERE id = $1`, reapRev.ID); err != nil {
		t.Fatalf("backdate revoked_at: %v", err)
	}
	// reapExp: push all three timestamps into the past to satisfy
	// expires_at > created_at and jwt_expires_at <= expires_at.
	if _, err := h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '60 days',
		       jwt_expires_at = now() - interval '32 days',
		       expires_at     = now() - interval '31 days'
		 WHERE id = $1`, reapExp.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	n, err := store.ReapExpiredSystem(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d rows, want 2", n)
	}

	// keep survives
	var c int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM public.sessions WHERE id = $1`, keep.ID,
	).Scan(&c); err != nil {
		t.Fatalf("count keep: %v", err)
	}
	if c != 1 {
		t.Errorf("keep row missing post-reap (count=%d)", c)
	}
}
