package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestUsersStore_Postgres_UpdateSettings_ReturnsUnderRLS runs the settings
// upsert on the app pool under the owner's own claims — the connection a
// request actually gets.
//
// The claims are the point. The shared settings conformance suite wires both
// pool slots to AdminDB, which is BYPASSRLS, and a RETURNING clause on a
// BYPASSRLS connection hands back its row unconditionally. Under RLS it does
// not: the upsert's conflict arm has to satisfy user_settings_select for the
// row it returns, so a policy admitting the write but not the read-back yields
// zero rows from a statement that updated one. Both arms run here, the second
// landing on the row the first created.
func TestUsersStore_Postgres_UpdateSettings_ReturnsUnderRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "user-settings-rls")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Truncated to the second: timestamptz stores microseconds, so a finer
	// value would compare unequal for the column's resolution rather than for
	// anything this write does.
	seen := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		users := pgstore.NewForTx(tx, pgtest.SecretKey).Users

		created, err := users.UpdateSettings(ctx, userID, domain.UserSettings{OverviewSeenAt: &seen})
		if err != nil {
			t.Fatalf("UpdateSettings (insert): %v", err)
		}
		if created.OverviewSeenAt == nil || !created.OverviewSeenAt.Equal(seen) {
			t.Fatalf("insert returned OverviewSeenAt = %v; want %v", created.OverviewSeenAt, seen)
		}

		later := seen.Add(time.Hour)
		updated, err := users.UpdateSettings(ctx, userID, domain.UserSettings{OverviewSeenAt: &later})
		if err != nil {
			t.Fatalf("UpdateSettings (conflict arm): %v", err)
		}
		if updated.OverviewSeenAt == nil || !updated.OverviewSeenAt.Equal(later) {
			t.Fatalf("conflict arm returned OverviewSeenAt = %v; want %v", updated.OverviewSeenAt, later)
		}

		read, err := users.GetSettings(ctx, userID)
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		if read.OverviewSeenAt == nil || !read.OverviewSeenAt.Equal(later) {
			t.Errorf("GetSettings = %v; want the row the write returned, %v", read.OverviewSeenAt, later)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestUsersStore_Postgres_Settings_IsolatePerUser pins the tenancy boundary
// the marker sits behind: user_settings is keyed by user alone, and the
// policies gate on tf.current_user_id(), so one person's Overview marker is
// unreachable from another's session even inside the same org.
func TestUsersStore_Postgres_Settings_IsolatePerUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, ownerID, _ := pgtest.SeedOrgWithUser(t, h, "seen-owner")
	otherID := pgtest.SeedUser(t, h, "seen-other")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, otherID, orgID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seen := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if err := h.WithUser(t, ownerID, orgID, func(tx *sql.Tx) error {
		_, err := pgstore.NewForTx(tx, pgtest.SecretKey).Users.UpdateSettings(
			ctx, ownerID, domain.UserSettings{OverviewSeenAt: &seen})
		return err
	}); err != nil {
		t.Fatalf("seed the owner's marker: %v", err)
	}

	if err := h.WithUser(t, otherID, orgID, func(tx *sql.Tx) error {
		users := pgstore.NewForTx(tx, pgtest.SecretKey).Users

		// The other member's own row is absent, which reads as the zero value.
		mine, err := users.GetSettings(ctx, otherID)
		if err != nil {
			return err
		}
		if mine.OverviewSeenAt != nil {
			t.Errorf("a member with no settings row reads OverviewSeenAt = %v; want nil", mine.OverviewSeenAt)
		}

		// And the owner's row is invisible rather than readable — the select
		// policy gates on the session's user, not on the id passed in.
		theirs, err := users.GetSettings(ctx, ownerID)
		if err != nil {
			return err
		}
		if theirs.OverviewSeenAt != nil {
			t.Errorf("read another member's OverviewSeenAt = %v; want nil", theirs.OverviewSeenAt)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}
