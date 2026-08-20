package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
			BlueprintRun: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgBlueprintRunForSnapshot(t, h, orgID, userID, suffix)
			},
			DeleteBlueprintRun: func(t *testing.T, blueprintRunID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`DELETE FROM blueprint_runs WHERE id = $1`, blueprintRunID); err != nil {
					t.Fatalf("delete blueprint_run: %v", err)
				}
			},
		}
		return stores.WorkspaceSnapshots, orgID, seed
	})
}

// TestWorkspaceSnapshotStore_Postgres_OrgScoped pins org_id defense-in-depth:
// the snapshot key is (org, blueprint_run), so a read or a write scoped to
// another org must not reach this org's row even on the BYPASSRLS admin pool.
func TestWorkspaceSnapshotStore_Postgres_OrgScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, userA, _ := pgtest.SeedOrgWithUser(t, h, "alice")
	otherOrg, _, _ := pgtest.SeedOrgWithUser(t, h, "bob")
	br := seedPgBlueprintRunForSnapshot(t, h, orgA, userA, "scoped")
	claimID := uuid.New().String()

	if err := stores.WorkspaceSnapshots.BeginSnapshotSystem(ctx, orgA, br, claimID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	got, err := stores.WorkspaceSnapshots.GetSnapshotStateSystem(ctx, otherOrg, br)
	if err != nil {
		t.Fatalf("get under another org: %v", err)
	}
	if got != nil {
		t.Errorf("read under another org returned %+v, want nil", got)
	}
	if err := stores.WorkspaceSnapshots.DeleteSnapshotStateSystem(ctx, otherOrg, br); err != nil {
		t.Fatalf("delete under another org: %v", err)
	}
	still, err := stores.WorkspaceSnapshots.GetSnapshotStateSystem(ctx, orgA, br)
	if err != nil {
		t.Fatalf("get after the other org's delete: %v", err)
	}
	if still == nil {
		t.Error("another org's delete dropped this org's snapshot state")
	}
}

// seedPgBlueprintRunForSnapshot mints the entity + event + task + blueprint
// chain a blueprint_runs row needs and returns the run id — the other half of
// the snapshot key.
func seedPgBlueprintRunForSnapshot(t *testing.T, h *pgtest.Harness, orgID, userID, suffix string) string {
	t.Helper()
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	blueprintID := "blueprint-snap-" + suffix + "-" + uuid.New().String()[:8]
	stepPromptID := "step-snap-" + suffix + "-" + uuid.New().String()[:8]
	seedPgBlueprint(t, h, orgID, userID, blueprintID)
	seedPgPrompt(t, h, orgID, userID, stepPromptID)
	taskID := seedPgTask(t, h, orgID, userID)

	brID, err := stores.Blueprints.CreateRun(ctx, orgID, domain.BlueprintRun{
		BlueprintID: blueprintID, TaskID: taskID, TriggerType: domain.BlueprintTriggerManual,
		WorktreePath: "/tmp/wt-snap-" + suffix,
		StepPlan:     []domain.BlueprintPlanStep{{StepIndex: 0, PromptID: stepPromptID, PromptName: "S", PromptBody: "b", Source: "user"}},
	})
	if err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}
