package pg_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// seedWorkspaceRow inserts a minimal org_slack_workspaces row for
// workspaceID via the admin pool. user_slack_identities carries no FK to
// this table (TFAC-533 — workspace_id alone is no longer unique there once
// a workspace can host several apps), so this is purely a realistic
// fixture, not a constraint requirement; kept anyway so identity-store
// tests read like production data. The identity-store tests don't exercise
// org scoping, so a fresh throwaway org and an arbitrary app id per call
// are both fine.
func seedWorkspaceRow(t *testing.T, h *pgtest.Harness, workspaceID string) {
	t.Helper()
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "slack-ident-org-"+workspaceID)
	apiAppID := "A0" + workspaceID
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO org_slack_workspaces (workspace_id, api_app_id, org_id, transport, bot_token_ref)
		VALUES ($1, $2, $3, 'socket', $4)
	`, workspaceID, apiAppID, orgID, "slack_ws_"+workspaceID+"_"+apiAppID+"_bot_token")
}

// TestSlackIdentityStore_Postgres_LookupSystem_NoRow: a sender never
// looked up yet returns (nil, nil), not an error — the "never attempted"
// state the resolver treats as "go ahead and call users.info".
func TestSlackIdentityStore_Postgres_LookupSystem_NoRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	got, err := slackstore.FromStores(stores).Identities.LookupSystem(context.Background(), "T0NOROW01", "U0NOROW01")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}
	if got != nil {
		t.Errorf("LookupSystem = %+v; want nil for a never-attempted sender", got)
	}
}

// TestSlackIdentityStore_Postgres_UpsertResolvedSystem_RoundTrip: a
// resolved write round-trips through LookupSystem with the user id and
// display name intact.
func TestSlackIdentityStore_Postgres_UpsertResolvedSystem_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	identities := slackstore.FromStores(stores).Identities

	seedWorkspaceRow(t, h, "T0RESOLV1")
	userID := pgtest.SeedUser(t, h, "slack-ident-resolved")
	if err := identities.UpsertResolvedSystem(ctx, "T0RESOLV1", "U0RESOLV1", userID, "Ada Lovelace"); err != nil {
		t.Fatalf("UpsertResolvedSystem: %v", err)
	}

	got, err := identities.LookupSystem(ctx, "T0RESOLV1", "U0RESOLV1")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}
	if got == nil {
		t.Fatal("LookupSystem = nil; want the just-upserted row")
	}
	if got.UserID == nil || *got.UserID != userID {
		t.Errorf("UserID = %v; want %q", got.UserID, userID)
	}
	if got.SlackDisplayName != "Ada Lovelace" {
		t.Errorf("SlackDisplayName = %q; want %q", got.SlackDisplayName, "Ada Lovelace")
	}
	if got.Source != "auto_email" {
		t.Errorf("Source = %q; want auto_email", got.Source)
	}
}

// TestSlackIdentityStore_Postgres_MarkAttemptSystem_CreatesNegativeCache:
// a fresh MarkAttemptSystem call creates the negative-cache row (nil
// user_id) rather than erroring on "no existing row".
func TestSlackIdentityStore_Postgres_MarkAttemptSystem_CreatesNegativeCache(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	identities := slackstore.FromStores(stores).Identities

	seedWorkspaceRow(t, h, "T0NEGATV1")
	if err := identities.MarkAttemptSystem(ctx, "T0NEGATV1", "U0NEGATV1", "Grace Hopper"); err != nil {
		t.Fatalf("MarkAttemptSystem: %v", err)
	}

	got, err := identities.LookupSystem(ctx, "T0NEGATV1", "U0NEGATV1")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}
	if got == nil {
		t.Fatal("LookupSystem = nil; want the negative-cache row")
	}
	if got.UserID != nil {
		t.Errorf("UserID = %v; want nil (unresolved)", *got.UserID)
	}
	if got.SlackDisplayName != "Grace Hopper" {
		t.Errorf("SlackDisplayName = %q; want %q", got.SlackDisplayName, "Grace Hopper")
	}
	if time.Since(got.LastAttemptAt) > time.Minute {
		t.Errorf("LastAttemptAt = %v; want ~now", got.LastAttemptAt)
	}
}

// TestSlackIdentityStore_Postgres_MarkAttemptSystem_BumpsUnresolvedRow: a
// second MarkAttemptSystem on an already-unresolved key updates the
// display name and bumps last_attempt_at — the "retry after TTL" path.
func TestSlackIdentityStore_Postgres_MarkAttemptSystem_BumpsUnresolvedRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	identities := slackstore.FromStores(stores).Identities

	seedWorkspaceRow(t, h, "T0BUMP0001")
	if err := identities.MarkAttemptSystem(ctx, "T0BUMP0001", "U0BUMP0001", "Old Name"); err != nil {
		t.Fatalf("first MarkAttemptSystem: %v", err)
	}
	first, err := identities.LookupSystem(ctx, "T0BUMP0001", "U0BUMP0001")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}

	// A short sleep guarantees the second call's now() is strictly later
	// than first's, regardless of the underlying timestamp resolution —
	// deterministic, unlike relying on incidental round-trip latency
	// between the two calls to provide separation.
	time.Sleep(5 * time.Millisecond)

	if err := identities.MarkAttemptSystem(ctx, "T0BUMP0001", "U0BUMP0001", "New Name"); err != nil {
		t.Fatalf("second MarkAttemptSystem: %v", err)
	}
	second, err := identities.LookupSystem(ctx, "T0BUMP0001", "U0BUMP0001")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}

	if second.SlackDisplayName != "New Name" {
		t.Errorf("SlackDisplayName after second attempt = %q; want %q", second.SlackDisplayName, "New Name")
	}
	if !second.LastAttemptAt.After(first.LastAttemptAt) {
		t.Errorf("LastAttemptAt did not advance: first=%v second=%v", first.LastAttemptAt, second.LastAttemptAt)
	}
	if second.UserID != nil {
		t.Errorf("UserID = %v; want nil (still unresolved)", *second.UserID)
	}
}

// TestSlackIdentityStore_Postgres_MarkAttemptSystem_NeverClobbersResolved
// pins the ON-CONFLICT-WHERE guard: once a sender is resolved,
// MarkAttemptSystem must be a complete no-op for that row (user_id AND
// last_attempt_at both untouched) — a stale negative-cache write racing
// behind a resolution must never un-resolve it.
func TestSlackIdentityStore_Postgres_MarkAttemptSystem_NeverClobbersResolved(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	identities := slackstore.FromStores(stores).Identities

	seedWorkspaceRow(t, h, "T0GUARD001")
	userID := pgtest.SeedUser(t, h, "slack-ident-guarded")
	if err := identities.UpsertResolvedSystem(ctx, "T0GUARD001", "U0GUARD001", userID, "Resolved Name"); err != nil {
		t.Fatalf("UpsertResolvedSystem: %v", err)
	}
	before, err := identities.LookupSystem(ctx, "T0GUARD001", "U0GUARD001")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}

	if err := identities.MarkAttemptSystem(ctx, "T0GUARD001", "U0GUARD001", "Attempted Overwrite"); err != nil {
		t.Fatalf("MarkAttemptSystem on a resolved row must not error: %v", err)
	}

	after, err := identities.LookupSystem(ctx, "T0GUARD001", "U0GUARD001")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}
	if after.UserID == nil || *after.UserID != userID {
		t.Errorf("UserID after MarkAttemptSystem = %v; want unchanged %q", after.UserID, userID)
	}
	if after.SlackDisplayName != "Resolved Name" {
		t.Errorf("SlackDisplayName after MarkAttemptSystem = %q; want unchanged %q", after.SlackDisplayName, "Resolved Name")
	}
	if !after.LastAttemptAt.Equal(before.LastAttemptAt) {
		t.Errorf("LastAttemptAt changed by a guarded MarkAttemptSystem: before=%v after=%v", before.LastAttemptAt, after.LastAttemptAt)
	}
}

// TestSlackIdentityStore_Postgres_UpsertResolvedSystem_OverwritesNegativeCache:
// a sender that was previously marked unresolved DOES get overwritten by a
// later successful resolution — the asymmetry with the guard above is the
// point (resolved always wins; negative-cache attempts never do).
func TestSlackIdentityStore_Postgres_UpsertResolvedSystem_OverwritesNegativeCache(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	identities := slackstore.FromStores(stores).Identities

	seedWorkspaceRow(t, h, "T0FLIP0001")
	if err := identities.MarkAttemptSystem(ctx, "T0FLIP0001", "U0FLIP0001", "Unresolved Name"); err != nil {
		t.Fatalf("MarkAttemptSystem: %v", err)
	}
	userID := pgtest.SeedUser(t, h, "slack-ident-flip")
	if err := identities.UpsertResolvedSystem(ctx, "T0FLIP0001", "U0FLIP0001", userID, "Now Resolved"); err != nil {
		t.Fatalf("UpsertResolvedSystem: %v", err)
	}

	got, err := identities.LookupSystem(ctx, "T0FLIP0001", "U0FLIP0001")
	if err != nil {
		t.Fatalf("LookupSystem: %v", err)
	}
	if got.UserID == nil || *got.UserID != userID {
		t.Errorf("UserID = %v; want %q (resolution must overwrite the negative cache)", got.UserID, userID)
	}
	if got.SlackDisplayName != "Now Resolved" {
		t.Errorf("SlackDisplayName = %q; want %q", got.SlackDisplayName, "Now Resolved")
	}
}

// TestRLS_UserSlackIdentitiesSelfAccess pins TFAC-531's RLS acceptance
// criteria, mirroring TestRLS_UserGitHubIdentitySelfAccess
// (internal/db/pgtest/baseline_test.go): a user can SELECT only their own
// RESOLVED row on the app pool; a NULL-user negative-cache row is invisible
// to everyone on the app pool (it matches no current_user_id()); and a
// cross-user row stays invisible too. Writes here go through h.AdminDB
// directly (BYPASSRLS) since capture is system-initiated — this test is
// about the read/write boundary the app pool sees, not the store (which
// never runs on the app pool at all).
func TestRLS_UserSlackIdentitiesSelfAccess(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	ownerUserID := pgtest.SeedUser(t, h, "slack-rls-owner")
	otherUserID := pgtest.SeedUser(t, h, "slack-rls-other")

	for _, ws := range []string{"T0RLSOWNR1", "T0RLSOTHR1", "T0RLSNULL1", "T0RLSSPOOF1"} {
		seedWorkspaceRow(t, h, ws)
	}

	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO user_slack_identities (workspace_id, slack_user_id, user_id, slack_display_name)
		VALUES ('T0RLSOWNR1', 'U0RLSOWNR1', $1, 'Owner Name')
	`, ownerUserID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO user_slack_identities (workspace_id, slack_user_id, user_id, slack_display_name)
		VALUES ('T0RLSOTHR1', 'U0RLSOTHR1', $1, 'Other Name')
	`, otherUserID)
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO user_slack_identities (workspace_id, slack_user_id, user_id, slack_display_name)
		VALUES ('T0RLSNULL1', 'U0RLSNULL1', NULL, 'Never Matched')
	`)

	// Owner sees exactly their own resolved row.
	if err := h.WithUser(t, ownerUserID, "", func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(context.Background(), `SELECT workspace_id, slack_user_id FROM user_slack_identities`)
		if e != nil {
			return e
		}
		defer rows.Close()
		var seen []string
		for rows.Next() {
			var ws, su string
			if e := rows.Scan(&ws, &su); e != nil {
				return e
			}
			seen = append(seen, ws+"/"+su)
		}
		if len(seen) != 1 || seen[0] != "T0RLSOWNR1/U0RLSOWNR1" {
			t.Errorf("owner sees %v; want exactly its own row", seen)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("owner select tx: %v", err)
	}

	// Other user's SELECT for the owner's row (naming the key explicitly)
	// returns no rows — RLS USING filters it out silently, not an error.
	if err := h.WithUser(t, otherUserID, "", func(tx *sql.Tx) error {
		var name string
		e := tx.QueryRowContext(context.Background(),
			`SELECT slack_display_name FROM user_slack_identities WHERE workspace_id = 'T0RLSOWNR1' AND slack_user_id = 'U0RLSOWNR1'`,
		).Scan(&name)
		if e == nil {
			t.Errorf("other user read owner's row = %q; want no rows", name)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-user read should filter to zero rows, not error: %v", err)
	}

	// The NULL-user negative-cache row is invisible to EVERY app-pool
	// user, including the row's own workspace/slack_user_id named
	// explicitly — it matches no current_user_id() by construction.
	if err := h.WithUser(t, ownerUserID, "", func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(context.Background(),
			`SELECT count(*) FROM user_slack_identities WHERE workspace_id = 'T0RLSNULL1' AND slack_user_id = 'U0RLSNULL1'`,
		).Scan(&n); e != nil {
			return e
		}
		if n != 0 {
			t.Errorf("NULL-user row visible on app pool: %d rows; want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("null-user visibility check tx: %v", err)
	}

	// A cross-user INSERT is refused by the modify policy's WITH CHECK.
	if err := h.WithUser(t, ownerUserID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO user_slack_identities (workspace_id, slack_user_id, user_id, slack_display_name)
			VALUES ('T0RLSSPOOF1', 'U0RLSSPOOF1', $1, 'spoofed')
		`, otherUserID)
		return e
	}); err == nil {
		t.Fatal("cross-user insert should violate WITH CHECK, but succeeded")
	}
}
