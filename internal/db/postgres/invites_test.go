package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// hashOf is the same sha256 the handler stores/looks up by.
func hashOf(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// TestInvitesStore_Postgres_CreateRoundTripsByHash: a created invite reads
// back via GetByTokenHashSystem, the join-populated org name lands, and the
// raw token never appears in the row (only its hash).
func TestInvitesStore_Postgres_CreateRoundTripsByHash(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "inv-create")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	const raw = "raw-token-abc123"
	var inviteID string
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		inviteID, e = tx.Invites.Create(ctx, domain.CreateInviteParams{
			OrgID:     orgID,
			Email:     "Recruit@Example.com", // store should lower-case
			Role:      "member",
			TokenHash: hashOf(raw),
			InvitedBy: userID,
			ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		})
		return e
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inviteID == "" {
		t.Fatal("Create returned empty id")
	}

	got, err := stores.Invites.GetByTokenHashSystem(ctx, hashOf(raw))
	if err != nil {
		t.Fatalf("GetByTokenHashSystem: %v", err)
	}
	if got == nil {
		t.Fatal("invite not found by token hash")
	}
	if got.Email != "recruit@example.com" {
		t.Errorf("email = %q; want lower-cased recruit@example.com", got.Email)
	}
	if got.OrgName == "" {
		t.Error("OrgName not join-populated on redeem read")
	}

	// The raw token must never be persisted anywhere on the row.
	var blob []byte
	if err := h.AdminDB.QueryRow(`SELECT token_hash FROM org_invites WHERE id = $1`, inviteID).Scan(&blob); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if string(blob) == raw {
		t.Error("token_hash column holds the raw token, not its hash")
	}
	var anyRaw int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM org_invites WHERE id = $1 AND email = $2`, inviteID, raw,
	).Scan(&anyRaw); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if anyRaw != 0 {
		t.Error("raw token leaked into the email column")
	}
}

// TestInvitesStore_Postgres_GetByUnknownHashIsNil: a hash that matches no row
// returns (nil, nil), not an error — the redeem paths map that to 404.
func TestInvitesStore_Postgres_GetByUnknownHashIsNil(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Invites.GetByTokenHashSystem(context.Background(), hashOf("nope"))
	if err != nil {
		t.Fatalf("GetByTokenHashSystem: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v; want nil for unknown token", got)
	}
}

// TestInvitesStore_Postgres_ListActiveExcludesTerminal: only un-accepted,
// un-revoked invites surface in ListActive.
func TestInvitesStore_Postgres_ListActiveExcludesTerminal(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "inv-list")
	exp := time.Now().Add(7 * 24 * time.Hour)

	// pending, accepted, revoked — three distinct emails (active-unique is
	// per (org, lower(email)), so terminal rows don't block re-use here).
	mustInsertInvite(t, h, orgID, "pending@x.com", hashOf("t-pending"), exp, false, false)
	mustInsertInvite(t, h, orgID, "accepted@x.com", hashOf("t-accepted"), exp, true, false)
	mustInsertInvite(t, h, orgID, "revoked@x.com", hashOf("t-revoked"), exp, false, true)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	var list []domain.OrgInvite
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		list, e = tx.Invites.ListActive(ctx, orgID)
		return e
	}); err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListActive = %d rows; want 1 (only pending)", len(list))
	}
	if list[0].Email != "pending@x.com" {
		t.Errorf("active invite = %q; want pending@x.com", list[0].Email)
	}
}

// TestInvitesStore_Postgres_RevokeOutcomes: pending → OK, already-revoked →
// OK (idempotent), accepted → AlreadyAccepted, unknown id → NotFound.
func TestInvitesStore_Postgres_RevokeOutcomes(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "inv-revoke")
	exp := time.Now().Add(7 * 24 * time.Hour)

	pendingID := mustInsertInvite(t, h, orgID, "p@x.com", hashOf("rp"), exp, false, false)
	acceptedID := mustInsertInvite(t, h, orgID, "a@x.com", hashOf("ra"), exp, true, false)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	revoke := func(id string) db.RevokeOutcome {
		var out db.RevokeOutcome
		if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			var e error
			out, e = tx.Invites.Revoke(ctx, orgID, id)
			return e
		}); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		return out
	}

	if got := revoke(pendingID); got != db.RevokeOK {
		t.Errorf("revoke pending = %v; want RevokeOK", got)
	}
	if got := revoke(pendingID); got != db.RevokeOK {
		t.Errorf("re-revoke (idempotent) = %v; want RevokeOK", got)
	}
	if got := revoke(acceptedID); got != db.RevokeAlreadyAccepted {
		t.Errorf("revoke accepted = %v; want RevokeAlreadyAccepted", got)
	}
	if got := revoke(uuid.NewString()); got != db.RevokeNotFound {
		t.Errorf("revoke unknown = %v; want RevokeNotFound", got)
	}
}

// TestInvitesStore_Postgres_ActivePartialUnique: a second pending invite to
// the same (org, lower(email)) conflicts; re-inviting after revoke succeeds.
func TestInvitesStore_Postgres_ActivePartialUnique(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "inv-uniq")
	exp := time.Now().Add(7 * 24 * time.Hour)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	create := func(email, token string) error {
		return stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, e := tx.Invites.Create(ctx, domain.CreateInviteParams{
				OrgID: orgID, Email: email, Role: "member",
				TokenHash: hashOf(token), InvitedBy: userID, ExpiresAt: exp,
			})
			return e
		})
	}

	if err := create("dup@x.com", "tok-1"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Case-insensitive collision on the active partial-unique index.
	err := create("DUP@x.com", "tok-2")
	if err == nil {
		t.Fatal("second pending invite to same email succeeded; want unique violation")
	}

	// Revoke the pending one, then a fresh invite to the same address is
	// allowed (the partial index only covers active rows).
	var pendingID string
	if e := h.AdminDB.QueryRow(
		`SELECT id::text FROM org_invites WHERE org_id=$1 AND lower(email)='dup@x.com' AND revoked_at IS NULL`,
		orgID,
	).Scan(&pendingID); e != nil {
		t.Fatalf("find pending: %v", e)
	}
	if got := func() db.RevokeOutcome {
		var o db.RevokeOutcome
		_ = stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			var e error
			o, e = tx.Invites.Revoke(ctx, orgID, pendingID)
			return e
		})
		return o
	}(); got != db.RevokeOK {
		t.Fatalf("revoke = %v; want RevokeOK", got)
	}
	if err := create("dup@x.com", "tok-3"); err != nil {
		t.Fatalf("re-invite after revoke: %v", err)
	}
}

// TestInvitesStore_Postgres_ReinviteAfterExpiry: an *expired* pending invite
// doesn't block a re-invite — Create revokes it first (so the active-unique
// slot frees) and the new row lands, leaving exactly one active invite.
func TestInvitesStore_Postgres_ReinviteAfterExpiry(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "inv-expiry")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	create := func(email, token string, expiresAt time.Time) error {
		return stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, e := tx.Invites.Create(ctx, domain.CreateInviteParams{
				OrgID: orgID, Email: email, Role: "member",
				TokenHash: hashOf(token), InvitedBy: userID, ExpiresAt: expiresAt,
			})
			return e
		})
	}

	// First invite is already expired.
	if err := create("stale@x.com", "tok-old", time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	// Re-invite succeeds despite the expired row still being un-revoked.
	if err := create("stale@x.com", "tok-new", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("re-invite after expiry: %v", err)
	}

	// The expired row was revoked, the new one is the sole active invite.
	var active int
	if err := h.AdminDB.QueryRow(`
		SELECT count(*) FROM org_invites
		WHERE org_id=$1 AND lower(email)='stale@x.com'
		  AND accepted_at IS NULL AND revoked_at IS NULL
	`, orgID).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 1 {
		t.Errorf("active invites for stale@x.com = %d; want 1 (expired revoked, new pending)", active)
	}

	// And a live pending invite still blocks (the freeing is expiry-only).
	if err := create("stale@x.com", "tok-third", time.Now().Add(24*time.Hour)); err == nil {
		t.Error("re-invite against a live pending invite succeeded; want unique violation")
	}
}

// TestInvitesStore_Postgres_RLS_AdminVsNonAdmin: the org_invites policies
// gate every app-pool operation on org-admin. A plain member sees nothing,
// can't insert, can't update; an org admin can.
func TestInvitesStore_Postgres_RLS_AdminVsNonAdmin(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, teamID := pgtest.SeedOrgWithUser(t, h, "inv-rls")
	memberID := pgtest.SeedUser(t, h, "inv-rls-member")
	pgtest.AddOrgMember(t, h, memberID, orgID, teamID, "member", "member")
	exp := time.Now().Add(7 * 24 * time.Hour)

	inviteID := mustInsertInvite(t, h, orgID, "target@x.com", hashOf("rls-tok"), exp, false, false)

	// Non-admin member: SELECT returns nothing (RLS hides the row).
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM org_invites`).Scan(&n); e != nil {
			return e
		}
		if n != 0 {
			t.Errorf("member SELECT count = %d; want 0 (RLS hides invites)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member select tx: %v", err)
	}

	// Non-admin member: INSERT is rejected by the WITH CHECK.
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO org_invites (org_id, email, role, token_hash, expires_at)
			VALUES ($1, 'x@x.com', 'member', $2, now() + interval '1 day')
		`, orgID, hashOf("member-insert"))
		return e
	}); err == nil {
		t.Error("member INSERT succeeded; want RLS rejection")
	}

	// Non-admin member: UPDATE matches no rows (USING hides it).
	if err := h.WithUser(t, memberID, orgID, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE org_invites SET revoked_at = now() WHERE id = $1`, inviteID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("member UPDATE affected %d rows; want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("member update tx: %v", err)
	}

	// Org admin (owner): SELECT sees the row.
	if err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM org_invites`).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Errorf("admin SELECT count = %d; want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("admin select tx: %v", err)
	}
}

// TestInvitesStore_Postgres_IsOrgMemberSystem: true for a member, false for
// an outsider.
func TestInvitesStore_Postgres_IsOrgMemberSystem(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, _ := pgtest.SeedOrgWithUser(t, h, "inv-member")
	outsider := pgtest.SeedUser(t, h, "inv-outsider")

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	in, err := stores.Invites.IsOrgMemberSystem(ctx, ownerID, orgID)
	if err != nil {
		t.Fatalf("IsOrgMemberSystem(owner): %v", err)
	}
	if !in {
		t.Error("owner reported not a member")
	}
	out, err := stores.Invites.IsOrgMemberSystem(ctx, outsider, orgID)
	if err != nil {
		t.Fatalf("IsOrgMemberSystem(outsider): %v", err)
	}
	if out {
		t.Error("outsider reported a member")
	}
}

// mustInsertInvite seeds an org_invites row via the admin pool (fixture,
// bypasses RLS) and returns its id. accepted/revoked stamp the respective
// terminal columns.
func mustInsertInvite(t *testing.T, h *pgtest.Harness, orgID, email string, tokenHash []byte, expiresAt time.Time, accepted, revoked bool) string {
	t.Helper()
	var acceptedAt, revokedAt any
	if accepted {
		acceptedAt = time.Now()
	}
	if revoked {
		revokedAt = time.Now()
	}
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO org_invites (org_id, email, role, token_hash, expires_at, accepted_at, revoked_at)
		VALUES ($1, lower($2), 'member', $3, $4, $5, $6)
		RETURNING id::text
	`, orgID, email, tokenHash, expiresAt, acceptedAt, revokedAt).Scan(&id); err != nil {
		t.Fatalf("insert invite fixture: %v", err)
	}
	return id
}
