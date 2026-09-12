package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestWorkspaceSnapshotStore_Postgres runs the shared conformance suite against
// the Postgres WorkspaceSnapshotStore impl, wired against AdminDB (the
// production wiring is admin-pool only: every caller is an executor teardown or
// a system sweep). Skips cleanly when Docker isn't available (pgtest.Shared).
func TestWorkspaceSnapshotStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunWorkspaceSnapshotStoreConformance(t, func(t *testing.T) (db.WorkspaceSnapshotStore, string, dbtest.WorkspaceSnapshotSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "alice")
		seed := dbtest.WorkspaceSnapshotSeeder{
			Task: func(t *testing.T, _ string) string {
				t.Helper()
				return seedPgTask(t, h, orgID, userID)
			},
			DeleteTask: func(t *testing.T, taskID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DELETE FROM tasks WHERE id = $1`, taskID); err != nil {
					t.Fatalf("delete task: %v", err)
				}
			},
		}
		return stores.WorkspaceSnapshots, orgID, seed
	})
}

// TestWorkspaceSnapshotStore_Postgres_OrgScoped pins org_id defense-in-depth:
// the snapshot key is (org, task), so a read or a write scoped to
// another org must not reach this org's row even on the BYPASSRLS admin pool.
func TestWorkspaceSnapshotStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	task := seedPgTask(t, h, orgA, userA)
	claimID := uuid.New().String()

	if err := stores.WorkspaceSnapshots.BeginSnapshotSystem(ctx, orgA, task, claimID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	got, err := stores.WorkspaceSnapshots.GetSnapshotStateSystem(ctx, otherOrg, task)
	if err != nil {
		t.Fatalf("get under another org: %v", err)
	}
	if got != nil {
		t.Errorf("read under another org returned %+v, want nil", got)
	}
	if err := stores.WorkspaceSnapshots.DeleteSnapshotStateSystem(ctx, otherOrg, task); err != nil {
		t.Fatalf("delete under another org: %v", err)
	}
	still, err := stores.WorkspaceSnapshots.GetSnapshotStateSystem(ctx, orgA, task)
	if err != nil {
		t.Fatalf("get after the other org's delete: %v", err)
	}
	if still == nil {
		t.Error("another org's delete dropped this org's snapshot state")
	}
}
