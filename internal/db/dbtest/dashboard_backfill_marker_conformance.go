package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// DashboardBackfillMarkerFactory returns a wired UsersStore plus a user-row
// seeder (row creation has no store method — it's an auth/provisioning
// concern). Each call must yield a fresh, isolated backend so subtests don't
// leak rows into one another.
type DashboardBackfillMarkerFactory func(t *testing.T) (users db.UsersStore, seedUser func(t *testing.T) string)

// RunDashboardBackfillMarkerConformance is the shared assertion suite for the
// TFAC-396 dashboard-backfill marker. It pins, identically across SQLite and
// Postgres:
//
//   - the marker starts unset on a fresh identity binding;
//   - MarkDashboardBackfilledSystem stamps it (admin-pool / claims-free write);
//   - a no-op re-bind (same login) preserves it, so an unchanged login never
//     re-fires the search burst;
//   - a re-bind to a DIFFERENT login clears it, so the new login's history is
//     re-seeded.
func RunDashboardBackfillMarkerConformance(t *testing.T, mk DashboardBackfillMarkerFactory) {
	t.Helper()
	ctx := context.Background()
	const host = "https://github.com"

	t.Run("StartsUnset", func(t *testing.T) {
		users, seedUser := mk(t)
		u := seedUser(t)
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "pat"); err != nil {
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
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host); err != nil {
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
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		// Re-bind with the SAME login (different source) — marker must survive.
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "connect_oauth"); err != nil {
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
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octocat", "pat"); err != nil {
			t.Fatalf("UpsertGitHubIdentity: %v", err)
		}
		if err := users.MarkDashboardBackfilledSystem(ctx, u, host); err != nil {
			t.Fatalf("MarkDashboardBackfilledSystem: %v", err)
		}
		// Re-bind to a DIFFERENT login — the prior login's backfill is stale, so
		// the marker must reset to NULL.
		if err := users.UpsertGitHubIdentity(ctx, u, host, "octonew", "connect_oauth"); err != nil {
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
}
