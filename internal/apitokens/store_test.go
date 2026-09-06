package apitokens

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// newStoreForTest boots the shared container, wipes it, and returns a store
// plus one fully wired org (owner user, org, default team).
func newStoreForTest(t *testing.T) (*Store, *pgtest.Harness, string, string) {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "tokens")
	return NewStore(h.AdminDB), h, userID, orgID
}

// setCap writes the org's api_token_max_age_days. The org_settings row is
// minted here rather than by SeedOrgWithUser, so a test that never calls this
// exercises the uncapped no-row case.
func setCap(t *testing.T, h *pgtest.Harness, orgID string, days *int) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO org_settings (org_id, api_token_max_age_days) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE SET api_token_max_age_days = EXCLUDED.api_token_max_age_days
	`, orgID, days)
}

// backdate rewrites a token's created_at, which is how a test reaches an age
// the cap measures against without waiting for one.
func backdate(t *testing.T, h *pgtest.Harness, tokenID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB,
		`UPDATE user_api_tokens SET created_at = now() - $2::interval WHERE id = $1`,
		tokenID, age.String())
}

func days(n int) *int { return &n }

// expire pushes a token's stored expiry into the past. created_at moves with it
// because the table refuses a row that expired before it existed, which is the
// same shape a token minted long ago and left to lapse has.
func expire(t *testing.T, h *pgtest.Harness, tokenID string, ago time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		UPDATE user_api_tokens
		   SET created_at = now() - $2::interval - interval '1 day',
		       expires_at = now() - $2::interval
		 WHERE id = $1
	`, tokenID, ago.String())
}

// auditRows returns the access_change_log rows for an org, oldest first, with
// the detail decoded.
func auditRows(t *testing.T, h *pgtest.Harness, orgID string) []struct {
	Action string
	Actor  string
	Target string
	Detail map[string]any
} {
	t.Helper()
	rows, err := h.AdminDB.Query(`
		SELECT action, COALESCE(actor_user_id::text, ''), COALESCE(target_user_id::text, ''),
		       COALESCE(detail_json, '')
		  FROM access_change_log WHERE org_id = $1 ORDER BY created_at, action
	`, orgID)
	if err != nil {
		t.Fatalf("read access_change_log: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Action string
		Actor  string
		Target string
		Detail map[string]any
	}
	for rows.Next() {
		var r struct {
			Action string
			Actor  string
			Target string
			Detail map[string]any
		}
		var raw string
		if err := rows.Scan(&r.Action, &r.Actor, &r.Target, &raw); err != nil {
			t.Fatalf("scan access_change_log: %v", err)
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &r.Detail); err != nil {
				t.Fatalf("decode detail_json %q: %v", raw, err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate access_change_log: %v", err)
	}
	return out
}

func TestMintLookupRoundtrip(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "ci", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(plaintext, Prefix) || len(plaintext) != len(Prefix)+43 {
		t.Errorf("plaintext %q: want %q + 43 base64url chars", plaintext, Prefix)
	}
	if tok.Prefix != plaintext[:prefixLen] {
		t.Errorf("stored prefix %q, want %q", tok.Prefix, plaintext[:prefixLen])
	}
	if tok.Name != "ci" || tok.UserID != userID || tok.OrgID != orgID {
		t.Errorf("returned row %+v does not carry what was minted", tok)
	}
	if tok.ExpiresAt != nil || tok.EffectiveExpiresAt != nil {
		t.Errorf("token minted with no expiry and no cap: got expires %v / effective %v, want both nil",
			tok.ExpiresAt, tok.EffectiveExpiresAt)
	}

	// The row holds a hash, not the secret: nothing in it reproduces the
	// plaintext, and the prefix is the only fragment of it stored.
	var storedHash []byte
	var storedPrefix string
	if err := h.AdminDB.QueryRow(
		`SELECT token_hash, token_prefix FROM user_api_tokens WHERE id = $1`, tok.ID,
	).Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if strings.Contains(string(storedHash), plaintext) || len(storedHash) != 32 {
		t.Errorf("token_hash is %d bytes and must not contain the plaintext", len(storedHash))
	}
	if len(storedPrefix) != prefixLen {
		t.Errorf("token_prefix %q is %d chars, want %d", storedPrefix, len(storedPrefix), prefixLen)
	}

	id, err := store.LookupSystem(ctx, plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if id == nil {
		t.Fatal("Lookup returned nil for a freshly minted token")
	}
	if id.TokenID != tok.ID || id.UserID != userID || id.OrgID != orgID {
		t.Errorf("identity %+v, want token %s / user %s / org %s", id, tok.ID, userID, orgID)
	}
	if id.AllowedCIDRs != nil {
		t.Errorf("AllowedCIDRs = %v, want nil for a token with no allowlist", id.AllowedCIDRs)
	}

	// A token that was never minted, and the same token with one character
	// changed, are both simply absent.
	for _, raw := range []string{"tf_nope", plaintext[:len(plaintext)-1] + "X"} {
		got, err := store.LookupSystem(ctx, raw)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", raw, err)
		}
		if got != nil {
			t.Errorf("Lookup(%q) resolved to %+v, want a miss", raw, got)
		}
	}
}

func TestMintReturnsTheStoredRow(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	exp := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Microsecond)
	tok, _, err := store.MintSystem(ctx, userID, orgID, "deploy", &exp,
		[]string{"10.0.0.0/8", "192.168.1.5"}, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The list read is the point read for this row: Mint RETURNs the same
	// projection List SELECTs, which is the drift this pins.
	read := func() (*Token, error) {
		got, total, err := store.ListForUserSystem(ctx, userID, &orgID, db.Unwindowed)
		if err != nil {
			return nil, err
		}
		if total != 1 || len(got) != 1 {
			return nil, nil
		}
		return &got[0], nil
	}
	dbtest.AssertWriteReturnedStoredRow(t, "MintSystem", tok, read)

	// A bare address is stored as the single host it names.
	want := []string{"10.0.0.0/8", "192.168.1.5/32"}
	if len(tok.AllowedCIDRs) != len(want) {
		t.Fatalf("AllowedCIDRs = %v, want %v", tok.AllowedCIDRs, want)
	}
	for i := range want {
		if tok.AllowedCIDRs[i] != want[i] {
			t.Errorf("AllowedCIDRs[%d] = %q, want %q", i, tok.AllowedCIDRs[i], want[i])
		}
	}
}

// TestRenameReturnsTheStoredRow pins the rename's write shape against the list
// read the same way Mint's is, and the two refusals: another user's token and a
// revoked one are both ErrNoSuchToken, since the statement's predicate is what
// keeps a caller to their own live rows.
func TestRenameReturnsTheStoredRow(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	tok, _, err := store.MintSystem(ctx, userID, orgID, "before", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	renamed, err := store.RenameSystem(ctx, userID, tok.ID, "after")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "after" || renamed.ID != tok.ID || renamed.Prefix != tok.Prefix {
		t.Errorf("renamed = %+v, want name=after on the same row", renamed)
	}
	read := func() (*Token, error) {
		got, total, err := store.ListForUserSystem(ctx, userID, &orgID, db.Unwindowed)
		if err != nil {
			return nil, err
		}
		if total != 1 || len(got) != 1 {
			return nil, nil
		}
		return &got[0], nil
	}
	dbtest.AssertWriteReturnedStoredRow(t, "RenameSystem", renamed, read)

	// Someone else's user id names nothing, and leaves the row alone.
	other := "00000000-0000-4000-8000-00000000beef"
	if _, err := store.RenameSystem(ctx, other, tok.ID, "stolen"); !errors.Is(err, ErrNoSuchToken) {
		t.Errorf("rename as another user: err = %v, want ErrNoSuchToken", err)
	}
	if got, _ := read(); got == nil || got.Name != "after" {
		t.Errorf("row after a refused rename = %+v, want name=after untouched", got)
	}

	// A revoked token is not renameable — it is not a token any more.
	if err := store.RevokeSystem(ctx, userID, tok.ID, userID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.RenameSystem(ctx, userID, tok.ID, "zombie"); !errors.Is(err, ErrNoSuchToken) {
		t.Errorf("rename after revoke: err = %v, want ErrNoSuchToken", err)
	}
}

func TestMintRejectsUnusableCIDRs(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	for _, bad := range []string{"not-an-ip", "10.0.0.1/8", "10.0.0.0/33", ""} {
		if _, _, err := store.MintSystem(ctx, userID, orgID, "n", nil, []string{bad}, userID); !errors.Is(err, ErrInvalidCIDR) {
			t.Errorf("Mint with allowed_cidrs %q: got %v, want ErrInvalidCIDR", bad, err)
		}
	}
	tooMany := make([]string, MaxAllowedCIDRs+1)
	for i := range tooMany {
		tooMany[i] = "10.0.0.0/8"
	}
	if _, _, err := store.MintSystem(ctx, userID, orgID, "n", nil, tooMany, userID); !errors.Is(err, ErrTooManyCIDRs) {
		t.Errorf("Mint with %d CIDRs: got %v, want ErrTooManyCIDRs", len(tooMany), err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM user_api_tokens`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d token(s) written by refused mints, want 0", n)
	}
}

func TestHashUniqueness(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	_, plaintext, err := store.MintSystem(ctx, userID, orgID, "one", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The unique index is what makes lookup an index probe rather than a
	// scan-and-compare; a second row under the same hash would make a lookup
	// ambiguous about which token authenticated.
	_, err = h.AdminDB.Exec(`
		INSERT INTO user_api_tokens (user_id, org_id, name, token_hash, token_prefix)
		SELECT user_id, org_id, 'clone', token_hash, token_prefix FROM user_api_tokens LIMIT 1
	`)
	if err == nil {
		t.Fatal("a second row with the same token_hash was accepted")
	}
	if !strings.Contains(err.Error(), "user_api_tokens_hash_uniq") {
		t.Errorf("duplicate hash rejected by %v, want the hash unique index", err)
	}
	if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
		t.Fatalf("Lookup after the refused duplicate: %v / %+v", err, got)
	}
}

func TestExpiry(t *testing.T) {
	ctx := context.Background()

	t.Run("stored expires_at is honored", func(t *testing.T) {
		store, h, userID, orgID := newStoreForTest(t)
		exp := time.Now().Add(time.Hour)
		tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "short", &exp, nil, userID)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if tok.EffectiveExpiresAt == nil {
			t.Fatal("EffectiveExpiresAt is nil for a token with a stored expiry")
		}
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
			t.Fatalf("Lookup before expiry: %v / %+v", err, got)
		}
		expire(t, h, tok.ID, time.Minute)
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got != nil {
			t.Errorf("Lookup after expiry: %v / %+v, want a miss", err, got)
		}
	})

	t.Run("cap alone expires a token with no stored expiry", func(t *testing.T) {
		store, h, userID, orgID := newStoreForTest(t)
		setCap(t, h, orgID, days(30))
		tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "capped", nil, nil, userID)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if tok.ExpiresAt != nil {
			t.Errorf("ExpiresAt = %v, want nil — the cap is not folded into the row", tok.ExpiresAt)
		}
		if tok.EffectiveExpiresAt == nil {
			t.Fatal("EffectiveExpiresAt is nil under a 30-day cap")
		}
		backdate(t, h, tok.ID, 31*24*time.Hour)
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got != nil {
			t.Errorf("Lookup past the cap: %v / %+v, want a miss", err, got)
		}
	})

	t.Run("tightening the cap expires live tokens immediately", func(t *testing.T) {
		store, h, userID, orgID := newStoreForTest(t)
		tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "live", nil, nil, userID)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		backdate(t, h, tok.ID, 10*24*time.Hour)
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
			t.Fatalf("Lookup with no cap: %v / %+v", err, got)
		}
		setCap(t, h, orgID, days(7))
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got != nil {
			t.Errorf("Lookup after the cap tightened below the token's age: %v / %+v, want a miss", err, got)
		}
		// And loosening it brings the same token back — the cap is evaluated
		// at use, never stamped into the row.
		setCap(t, h, orgID, days(90))
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
			t.Errorf("Lookup after the cap loosened: %v / %+v, want the token back", err, got)
		}
	})

	t.Run("an earlier stored expiry wins over a looser cap", func(t *testing.T) {
		store, h, userID, orgID := newStoreForTest(t)
		setCap(t, h, orgID, days(365))
		exp := time.Now().Add(24 * time.Hour)
		tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "short", &exp, nil, userID)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if tok.EffectiveExpiresAt == nil || !tok.EffectiveExpiresAt.Equal(tok.ExpiresAt.UTC()) {
			t.Errorf("EffectiveExpiresAt = %v, want the stored expiry %v", tok.EffectiveExpiresAt, tok.ExpiresAt)
		}
		expire(t, h, tok.ID, time.Minute)
		if got, err := store.LookupSystem(ctx, plaintext); err != nil || got != nil {
			t.Errorf("Lookup past the stored expiry under a year-long cap: %v / %+v, want a miss", err, got)
		}
	})
}

func TestRevoke(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "doomed", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.RevokeSystem(ctx, userID, tok.ID, userID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got, err := store.LookupSystem(ctx, plaintext); err != nil || got != nil {
		t.Errorf("Lookup after revoke: %v / %+v, want a miss", err, got)
	}
	if err := store.RevokeSystem(ctx, userID, tok.ID, userID); !errors.Is(err, ErrNoSuchToken) {
		t.Errorf("second Revoke: got %v, want ErrNoSuchToken", err)
	}
	// A token id that isn't a UUID at all is the same answer, not a 500.
	if err := store.RevokeSystem(ctx, userID, "not-a-uuid", userID); !errors.Is(err, ErrNoSuchToken) {
		t.Errorf("Revoke of a malformed id: got %v, want ErrNoSuchToken", err)
	}
	// Revoked rows leave the list; the token is not something to act on.
	got, total, err := store.ListForUserSystem(ctx, userID, nil, db.Unwindowed)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 0 || len(got) != 0 {
		t.Errorf("list after revoke returned %d row(s) / total %d, want none", len(got), total)
	}
}

func TestGetForUserSystem(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	tok, _, err := store.MintSystem(ctx, userID, orgID, "readable", nil, []string{"10.0.0.0/8"}, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	got, err := store.GetForUserSystem(ctx, userID, tok.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned no row for the owner's own live token")
	}
	// The point read projects the same columns the list does, which is what
	// lets a caller decide on the org before writing.
	if got.OrgID != orgID || got.Name != "readable" || got.Prefix != tok.Prefix {
		t.Errorf("Get = %+v, want the minted row %+v", *got, tok)
	}
	if len(got.AllowedCIDRs) != 1 || got.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("allowed_cidrs = %v, want [10.0.0.0/8]", got.AllowedCIDRs)
	}

	// Every reason the caller may not act on it collapses to the same miss.
	other := pgtest.SeedUser(t, h, "not-the-owner")
	if got, err := store.GetForUserSystem(ctx, other, tok.ID); err != nil || got != nil {
		t.Errorf("Get of another user's token: %v / %+v, want a miss", err, got)
	}
	if got, err := store.GetForUserSystem(ctx, userID, "not-a-uuid"); err != nil || got != nil {
		t.Errorf("Get of a malformed id: %v / %+v, want a miss, not an error", err, got)
	}
	if err := store.RevokeSystem(ctx, userID, tok.ID, userID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got, err := store.GetForUserSystem(ctx, userID, tok.ID); err != nil || got != nil {
		t.Errorf("Get after revoke: %v / %+v, want a miss — a revoked token is not there", err, got)
	}
}

func TestRevokeRefusesAnotherUsersToken(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	other := pgtest.SeedUser(t, h, "intruder")
	tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "mine", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.RevokeSystem(ctx, other, tok.ID, other); !errors.Is(err, ErrNoSuchToken) {
		t.Errorf("cross-user Revoke: got %v, want ErrNoSuchToken", err)
	}
	if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
		t.Errorf("token after a refused cross-user revoke: %v / %+v, want it alive", err, got)
	}
}

func TestRevokeForUserInOrgLeavesOtherOrgsAlone(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	otherOrg := pgtest.SeedOrg(t, h, "second-org", userID)
	admin := pgtest.SeedUser(t, h, "org-admin")

	_, hereA, err := store.MintSystem(ctx, userID, orgID, "here-a", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, hereB, err := store.MintSystem(ctx, userID, orgID, "here-b", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, elsewhere, err := store.MintSystem(ctx, userID, otherOrg, "elsewhere", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	n, err := store.RevokeForUserInOrgSystem(ctx, userID, orgID, admin)
	if err != nil {
		t.Fatalf("RevokeForUserInOrg: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d token(s), want 2", n)
	}
	for _, dead := range []string{hereA, hereB} {
		if got, err := store.LookupSystem(ctx, dead); err != nil || got != nil {
			t.Errorf("token in the removed org: %v / %+v, want a miss", err, got)
		}
	}
	if got, err := store.LookupSystem(ctx, elsewhere); err != nil || got == nil {
		t.Errorf("token in another org: %v / %+v, want it untouched", err, got)
	}
	// Re-running is a no-op rather than a second sweep of audit rows.
	if n, err := store.RevokeForUserInOrgSystem(ctx, userID, orgID, admin); err != nil || n != 0 {
		t.Errorf("second RevokeForUserInOrg: %d / %v, want 0 / nil", n, err)
	}
}

func TestList(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	otherOrg := pgtest.SeedOrg(t, h, "list-second", userID)
	stranger := pgtest.SeedUser(t, h, "stranger")
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := store.MintSystem(ctx, userID, orgID, name, nil, nil, userID); err != nil {
			t.Fatalf("Mint %s: %v", name, err)
		}
	}
	if _, _, err := store.MintSystem(ctx, userID, otherOrg, "other", nil, nil, userID); err != nil {
		t.Fatalf("Mint other: %v", err)
	}
	if _, _, err := store.MintSystem(ctx, stranger, orgID, "theirs", nil, nil, stranger); err != nil {
		t.Fatalf("Mint stranger: %v", err)
	}

	all, total, err := store.ListForUserSystem(ctx, userID, nil, db.Unwindowed)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 4 || len(all) != 4 {
		t.Fatalf("list returned %d row(s) / total %d, want 4 — and never another user's", len(all), total)
	}
	// Newest first.
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Errorf("row %d was created before row %d; want newest first", i-1, i)
		}
	}

	scoped, total, err := store.ListForUserSystem(ctx, userID, &otherOrg, db.Unwindowed)
	if err != nil {
		t.Fatalf("List(orgFilter): %v", err)
	}
	if total != 1 || len(scoped) != 1 || scoped[0].Name != "other" {
		t.Errorf("org-filtered list = %+v / total %d, want the one token in that org", scoped, total)
	}

	// A page and a count-only read answer under the same filters.
	page, total, err := store.ListForUserSystem(ctx, userID, nil, db.ListOpts{Limit: 2})
	if err != nil {
		t.Fatalf("List(page): %v", err)
	}
	if len(page) != 2 || total != 4 {
		t.Errorf("page = %d row(s) / total %d, want 2 / 4", len(page), total)
	}
	counted, total, err := store.ListForUserSystem(ctx, userID, nil, db.ListOpts{CountOnly: true})
	if err != nil {
		t.Fatalf("List(count-only): %v", err)
	}
	if len(counted) != 0 || total != 4 {
		t.Errorf("count-only = %d row(s) / total %d, want 0 / 4", len(counted), total)
	}
}

func TestTokenLimit(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	for i := 0; i < MaxPerUserOrg; i++ {
		if _, _, err := store.MintSystem(ctx, userID, orgID, "bulk", nil, nil, userID); err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
	}
	if _, _, err := store.MintSystem(ctx, userID, orgID, "one too many", nil, nil, userID); !errors.Is(err, ErrTokenLimit) {
		t.Errorf("mint past the cap: got %v, want ErrTokenLimit", err)
	}
	// The cap is per (user, org): the same user in another org is unaffected.
	otherOrg := pgtest.SeedOrg(t, h, "limit-second", userID)
	if _, _, err := store.MintSystem(ctx, userID, otherOrg, "elsewhere", nil, nil, userID); err != nil {
		t.Errorf("mint in a second org at the first org's cap: %v", err)
	}
	// Revoking makes room again.
	got, _, err := store.ListForUserSystem(ctx, userID, &orgID, db.ListOpts{Limit: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("List: %v / %d rows", err, len(got))
	}
	if err := store.RevokeSystem(ctx, userID, got[0].ID, userID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := store.MintSystem(ctx, userID, orgID, "replacement", nil, nil, userID); err != nil {
		t.Errorf("mint after freeing a slot: %v", err)
	}
}

func TestConcurrentMintsDoNotOvershootTheLimit(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	for i := 0; i < MaxPerUserOrg-1; i++ {
		if _, _, err := store.MintSystem(ctx, userID, orgID, "bulk", nil, nil, userID); err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
	}

	// Eight mints racing for the single remaining slot. Without the advisory
	// lock they all read 49 and they all insert.
	//
	// The racers are released from a barrier, and each holds a connection open
	// until every one of them has one: a mint that pays for a fresh connection
	// while another is already committing isn't racing it, and the whole
	// contention window here is the microseconds between the count and the
	// insert.
	const racers = 8
	store.db.SetMaxIdleConns(racers + 1)
	var (
		wg      sync.WaitGroup
		ready   sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	start := make(chan struct{})
	ready.Add(racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := store.db.Conn(ctx)
			if err != nil {
				t.Errorf("warm a connection: %v", err)
				ready.Done()
				return
			}
			ready.Done()
			<-start
			// Back to the pool warm, so MintSystem's BeginTx picks it up
			// without a round trip of its own.
			conn.Close()

			_, _, err = store.MintSystem(ctx, userID, orgID, "racer", nil, nil, userID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted++
			case errors.Is(err, ErrTokenLimit):
			default:
				t.Errorf("racing Mint: %v", err)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	if granted != 1 {
		t.Errorf("%d of %d racing mints succeeded, want exactly 1", granted, racers)
	}
	var live int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM user_api_tokens WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL
	`, userID, orgID).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != MaxPerUserOrg {
		t.Errorf("%d live tokens, want %d", live, MaxPerUserOrg)
	}
}

func TestAuditRows(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()
	setCap(t, h, orgID, days(90))

	exp := time.Now().Add(48 * time.Hour)
	tok, _, err := store.MintSystem(ctx, userID, orgID, "audited", &exp, []string{"10.0.0.0/8"}, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	rows := auditRows(t, h, orgID)
	if len(rows) != 1 {
		t.Fatalf("%d audit row(s) after a mint, want 1", len(rows))
	}
	created := rows[0]
	if created.Action != domain.AccessActionAPITokenCreated || created.Actor != userID {
		t.Errorf("mint audit row: action %q actor %q, want %q / %q",
			created.Action, created.Actor, domain.AccessActionAPITokenCreated, userID)
	}
	if created.Detail["token_id"] != tok.ID || created.Detail["name"] != "audited" || created.Detail["prefix"] != tok.Prefix {
		t.Errorf("mint detail %v does not identify the token", created.Detail)
	}
	if created.Detail["max_age_days"] != float64(90) {
		t.Errorf("mint detail max_age_days = %v, want the cap in force (90)", created.Detail["max_age_days"])
	}
	if created.Detail["expires_at"] == nil {
		t.Errorf("mint detail %v carries no expires_at", created.Detail)
	}
	if cidrs, _ := created.Detail["allowed_cidrs"].([]any); len(cidrs) != 1 || cidrs[0] != "10.0.0.0/8" {
		t.Errorf("mint detail allowed_cidrs = %v, want the allowlist", created.Detail["allowed_cidrs"])
	}

	if err := store.RevokeSystem(ctx, userID, tok.ID, userID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	rows = auditRows(t, h, orgID)
	if len(rows) != 2 {
		t.Fatalf("%d audit row(s) after a revoke, want 2", len(rows))
	}
	revoked := rows[1]
	if revoked.Action != domain.AccessActionAPITokenRevoked || revoked.Detail["token_id"] != tok.ID {
		t.Errorf("revoke audit row %+v, want %q naming the token", revoked, domain.AccessActionAPITokenRevoked)
	}
	if _, ok := revoked.Detail["source"]; ok {
		t.Errorf("a deliberate revoke carries source %v; want none — that word is reserved for the removal hook",
			revoked.Detail["source"])
	}
	if revoked.Target != "" {
		t.Errorf("a self-revoke names target %q; the actor is the owner", revoked.Target)
	}
}

func TestAuditRowsForMembershipRemoval(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()
	admin := pgtest.SeedUser(t, h, "remover")

	for _, name := range []string{"one", "two"} {
		if _, _, err := store.MintSystem(ctx, userID, orgID, name, nil, nil, userID); err != nil {
			t.Fatalf("Mint %s: %v", name, err)
		}
	}
	if _, err := store.RevokeForUserInOrgSystem(ctx, userID, orgID, admin); err != nil {
		t.Fatalf("RevokeForUserInOrg: %v", err)
	}

	var revokes int
	for _, r := range auditRows(t, h, orgID) {
		if r.Action != domain.AccessActionAPITokenRevoked {
			continue
		}
		revokes++
		if r.Actor != admin {
			t.Errorf("auto-revoke actor %q, want the removing admin %q", r.Actor, admin)
		}
		if r.Target != userID {
			t.Errorf("auto-revoke target %q, want the token owner %q", r.Target, userID)
		}
		if r.Detail["source"] != domain.AccessSourceMembershipRemoved {
			t.Errorf("auto-revoke source %v, want %q", r.Detail["source"], domain.AccessSourceMembershipRemoved)
		}
	}
	if revokes != 2 {
		t.Errorf("%d revoke audit row(s), want one per token (2)", revokes)
	}
}

func TestAuditFailureRollsBackTheWrite(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	// An actor that isn't a UUID fails the audit INSERT's cast. The token
	// write is in the same transaction, so it must not survive.
	if _, _, err := store.MintSystem(ctx, userID, orgID, "orphan", nil, nil, "not-a-uuid"); err == nil {
		t.Fatal("Mint with an unwritable audit actor succeeded")
	}
	var tokens, audits int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM user_api_tokens`).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM access_change_log`).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if tokens != 0 || audits != 0 {
		t.Errorf("after a failed audit write: %d token(s) / %d audit row(s), want 0 / 0", tokens, audits)
	}

	// Same for the revoke path: the row stays live rather than dying
	// unrecorded.
	tok, plaintext, err := store.MintSystem(ctx, userID, orgID, "keeper", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.RevokeSystem(ctx, userID, tok.ID, "not-a-uuid"); err == nil {
		t.Fatal("Revoke with an unwritable audit actor succeeded")
	}
	if got, err := store.LookupSystem(ctx, plaintext); err != nil || got == nil {
		t.Errorf("token after a rolled-back revoke: %v / %+v, want it alive", err, got)
	}
}

func TestReaper(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()
	const retention = 30 * 24 * time.Hour

	longRevoked, _, err := store.MintSystem(ctx, userID, orgID, "long-revoked", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	justRevoked, _, err := store.MintSystem(ctx, userID, orgID, "just-revoked", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	longExpired, _, err := store.MintSystem(ctx, userID, orgID, "long-expired", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	active, _, err := store.MintSystem(ctx, userID, orgID, "active", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, id := range []string{longRevoked.ID, justRevoked.ID} {
		pgtest.MustExec(t, h.AdminDB, `UPDATE user_api_tokens SET revoked_at = now() WHERE id = $1`, id)
	}
	pgtest.MustExec(t, h.AdminDB,
		`UPDATE user_api_tokens SET revoked_at = now() - interval '31 days' WHERE id = $1`, longRevoked.ID)
	expire(t, h, longExpired.ID, 31*24*time.Hour)

	n, err := store.ReapExpiredSystem(ctx, retention)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d row(s), want 2 (the long-revoked and the long-expired)", n)
	}
	alive := map[string]bool{}
	rows, err := h.AdminDB.Query(`SELECT id::text FROM user_api_tokens`)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		alive[id] = true
	}
	if !alive[active.ID] {
		t.Error("the active token was reaped")
	}
	if !alive[justRevoked.ID] {
		t.Error("a token revoked inside the retention window was reaped; the audit trail needs it")
	}
	if alive[longRevoked.ID] || alive[longExpired.ID] {
		t.Error("a token dead past the retention window survived the reap")
	}

	// A cap-expired token is reaped on the same rule, since the cap is what
	// decides when it stopped working.
	setCap(t, h, orgID, days(1))
	backdate(t, h, active.ID, 40*24*time.Hour)
	if n, err := store.ReapExpiredSystem(ctx, retention); err != nil || n != 1 {
		t.Errorf("reap of a cap-expired token: %d / %v, want 1 / nil", n, err)
	}
}

func TestLookupCarriesIdentityEmailAndCIDRs(t *testing.T) {
	store, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	// An unverified address first, then the verified one: the verified login
	// identity is the answer whichever order the planner reaches them in.
	pgtest.SeedIdentity(t, h, userID, "shadow@test", false)
	pgtest.SeedIdentity(t, h, userID, "real@test", true)

	_, plaintext, err := store.MintSystem(ctx, userID, orgID, "id", nil, []string{"2001:db8::/32"}, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	id, err := store.LookupSystem(ctx, plaintext)
	if err != nil || id == nil {
		t.Fatalf("Lookup: %v / %+v", err, id)
	}
	if id.Email != "real@test" {
		t.Errorf("Email = %q, want the verified identity's address", id.Email)
	}
	if len(id.AllowedCIDRs) != 1 || id.AllowedCIDRs[0] != "2001:db8::/32" {
		t.Errorf("AllowedCIDRs = %v, want the token's allowlist", id.AllowedCIDRs)
	}
}

func TestTouchLastUsed(t *testing.T) {
	store, _, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	tok, _, err := store.MintSystem(ctx, userID, orgID, "touched", nil, nil, userID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok.LastUsedAt != nil {
		t.Errorf("LastUsedAt = %v on a token nobody has used", tok.LastUsedAt)
	}
	if err := store.TouchLastUsedSystem(ctx, tok.ID); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	got, _, err := store.ListForUserSystem(ctx, userID, nil, db.Unwindowed)
	if err != nil || len(got) != 1 {
		t.Fatalf("List: %v / %d rows", err, len(got))
	}
	if got[0].LastUsedAt == nil {
		t.Error("LastUsedAt is still nil after a touch")
	}
	// A malformed id is a no-op, not an error the request path has to handle.
	if err := store.TouchLastUsedSystem(ctx, "not-a-uuid"); err != nil {
		t.Errorf("TouchLastUsed with a malformed id: %v, want nil", err)
	}
}

// TestIsLiveSystem pins the id-keyed liveness probe against the same four
// deaths LookupSystem answers to, since the websocket sweep that calls it has
// only an id and must reach the identical verdict.
func TestIsLiveSystem(t *testing.T) {
	s, h, userID, orgID := newStoreForTest(t)
	ctx := context.Background()

	live := func(id string) bool {
		t.Helper()
		got, err := s.IsLiveSystem(ctx, id)
		if err != nil {
			t.Fatalf("IsLiveSystem: %v", err)
		}
		return got
	}

	fresh, _, err := s.MintSystem(ctx, userID, orgID, "fresh", nil, nil, userID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !live(fresh.ID) {
		t.Error("a freshly minted token reads as dead")
	}

	revoked, _, err := s.MintSystem(ctx, userID, orgID, "revoked", nil, nil, userID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := s.RevokeSystem(ctx, userID, revoked.ID, userID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if live(revoked.ID) {
		t.Error("a revoked token reads as live")
	}

	lapsed, _, err := s.MintSystem(ctx, userID, orgID, "lapsed", nil, nil, userID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	expire(t, h, lapsed.ID, time.Hour)
	if live(lapsed.ID) {
		t.Error("an expired token reads as live")
	}

	// The org's cap applies here exactly as it does at use: tighten it and a
	// token that was live a moment ago is not.
	capped, _, err := s.MintSystem(ctx, userID, orgID, "capped", nil, nil, userID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	backdate(t, h, capped.ID, 40*24*time.Hour)
	if !live(capped.ID) {
		t.Fatal("an old token in an uncapped org reads as dead")
	}
	setCap(t, h, orgID, days(30))
	if live(capped.ID) {
		t.Error("a token past the org's cap reads as live")
	}

	// Ids that name nothing are an answer, not an error — including one
	// Postgres would refuse to cast.
	if live("00000000-0000-0000-0000-000000000000") {
		t.Error("an unknown id reads as live")
	}
	if got, err := s.IsLiveSystem(ctx, "not-a-uuid"); err != nil || got {
		t.Errorf("IsLiveSystem(non-uuid) = (%v, %v), want (false, nil)", got, err)
	}
}
