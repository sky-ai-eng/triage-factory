package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// DashboardBackfillMarkerFactory returns a wired UsersStore plus a user-row
// seeder (row creation has no store method — it's an auth/provisioning
// concern). Each call must yield a fresh, isolated backend so subtests don't
// leak rows into one another.
type DashboardBackfillMarkerFactory func(t *testing.T) (users db.UsersStore, seedUser func(t *testing.T) string)

// RunDashboardBackfillMarkerConformance is the shared assertion suite for the
// dashboard-backfill marker. It pins, identically across SQLite and
// Postgres:
//
//   - the marker starts unset on a fresh identity binding;
//   - MarkDashboardBackfilledSystem stamps it (admin-pool / claims-free write);
//   - the stamp is login-guarded: marking with a login that no longer matches
//     the row no-ops, so an in-flight backfill for a since-replaced login can't
//     strand the new login as "already backfilled";
//   - a no-op re-bind (same login, same account) preserves it, so an
//     unchanged login never re-fires the search burst;
//   - a re-bind to a DIFFERENT login clears it, the same account renamed
//     included, so the new login's history is re-seeded, and so does a
//     different account under the same login.
func RunDashboardBackfillMarkerConformance(t *testing.T, mk DashboardBackfillMarkerFactory) {
	t.Helper()
	ctx := context.Background()
	const host = "https://github.com"

	t.Run("StartsUnset", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		if at != nil {
			t.Errorf("marker = %v on fresh binding; want nil", at)
		}
	})

	t.Run("MarkThenRead", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host, "octocat"); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		if at == nil {
			t.Error("marker = nil after MarkDashboardBackfilledSystem; want a timestamp")
		}
	})

	t.Run("SameLoginRebindPreservesMarker", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host, "octocat"); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		// Re-bind with the SAME login (different source) — marker must survive.
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "connect_oauth"); err != nil {
			t.Fatalf("re-bind same login: %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		if at == nil {
			t.Error("marker cleared by a no-op (same-login) re-bind; want preserved")
		}
	})

	t.Run("LoginChangeResetsMarker", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host, "octocat"); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		// Re-bind to a DIFFERENT login — the prior login's backfill is stale, so
		// the marker must reset to NULL.
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octonew", "", "", "connect_oauth"); err != nil {
			t.Fatalf("re-bind new login: %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		if at != nil {
			t.Errorf("marker = %v after login change; want nil (reset)", at)
		}
	})

	// marked binds u to (login, id), stamps the marker, and re-binds to
	// (newLogin, newID), returning the marker the re-bind leaves.
	marked := func(t *testing.T, login, id, newLogin, newID string) *time.Time {
		t.Helper()
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, login, id, "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host, login); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		if err := users.UpsertGitHubIdentity(ctx, u, host, newLogin, newID, "", "connect_oauth"); err != nil {
			t.Fatalf("re-bind: %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		return at
	}

	for _, c := range []struct {
		name, newLogin, newID string
		kept                  bool
	}{
		// The backfill searched author:<login>, so the same account under a
		// new login still owes the search its new login answers.
		{"RenameOfTheSameAccountResetsMarker", "octonew", "583231", false},
		{"AnotherAccountUnderTheSameLoginResetsMarker", "octocat", "999", false},
		{"SameAccountCaptureWithoutAnIDPreservesMarker", "octocat", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if at := marked(t, "octocat", "583231", c.newLogin, c.newID); (at != nil) != c.kept {
				t.Errorf("marker = %v after re-binding octocat/583231 to %s/%q; want kept=%v", at, c.newLogin, c.newID, c.kept)
			}
		})
	}

	t.Run("MarkWithMismatchedLoginNoOps", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "", "", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		// Models the in-flight race: a detached backfill that searched a
		// since-replaced login ("octostale") tries to stamp, but the row now
		// carries "octocat". The login-guarded write must NOT mark it done —
		// otherwise "octocat" is treated as backfilled when its history was never
		// seeded, and it never re-seeds.
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host, "octostale"); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem (mismatched login): %v", err)
		}
		at, err := users.DashboardBackfilledAtSystem(ctx, u, host)
		if err != nil {
			t.Fatalf("DashboardBackfilledAtSystem: %v", err)
		}
		if at != nil {
			t.Errorf("marker = %v after stamping a mismatched login; want nil (guarded no-op)", at)
		}
	})
}
