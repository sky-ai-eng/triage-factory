package pgtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Store conformance, run as the role production runs as.
//
// Every other Postgres conformance run in this repo wires its store to a
// superuser connection, which passes whatever the grant list says — so a table
// on the executor's write path could (and did) ship with no tf_system grant at
// all, green the whole way, and fail only on a real executor with a 42501 an
// operator eventually noticed in a log. The runs below bind the same shared
// suites to SystemDB (tf_system, the executor's admin pool) so the grant is
// part of what the mandatory conformance case proves.
//
// Which stores belong here: the ones an executor pod actually drives. A store
// whose suite runs only superuser-bound has no backstop, so a new
// executor-path store should be added here in the same change that introduces
// it. The remaining executor surface — the claim CAS and requeue, the fleet
// registry, cross-pod signals, worktrees, memory, artifacts — is already
// exercised role-bound by TestTfSystem_ExecutorSurfaceConformance in
// tf_system_test.go, which walks those call sites directly rather than through
// a shared suite (they have none).

// TestTfSystem_SandboxStatStoreConformance is the guard the missing
// sandbox_stats grant walked past: the per-jail sampler's INSERT, the series
// read, and the retention reaper's DELETE, all as tf_system.
func TestTfSystem_SandboxStatStoreConformance(t *testing.T) {
	h := Shared(t)
	dbtest.RunSandboxStatStoreConformance(t, func(t *testing.T) db.SandboxStatStore {
		t.Helper()
		h.Reset(t)
		return pgstore.NewSandboxStatStore(h.SystemDB)
	})
}

// TestTfSystem_InstanceStatStoreConformance covers sandbox_stats' twin. It has
// had its grant since it shipped; running it role-bound keeps the pair
// symmetric, so a later change that touches the family's grants can't fix one
// and regress the other unnoticed.
func TestTfSystem_InstanceStatStoreConformance(t *testing.T) {
	h := Shared(t)
	dbtest.RunInstanceStatStoreConformance(t, func(t *testing.T) db.InstanceStatStore {
		t.Helper()
		h.Reset(t)
		return pgstore.NewInstanceStatStore(h.SystemDB)
	})
}

// TestTfSystem_WorkspaceSnapshotStoreConformance covers the workspace-snapshot
// lifecycle row as the role that actually writes it: the executor's teardown
// begins and closes out every snapshot, its resume path reads the state back,
// and its retention reaper deletes it — so the whole surface is on this role's
// path and none of it has another pod whose success could stand in for it.
// Seeding stays on AdminDB: staging a blueprint run is fixture setup, not the
// flow under test, and tf_system holds no INSERT on those tables by design.
func TestTfSystem_WorkspaceSnapshotStoreConformance(t *testing.T) {
	h := Shared(t)
	dbtest.RunWorkspaceSnapshotStoreConformance(t, func(t *testing.T) (db.WorkspaceSnapshotStore, string, dbtest.WorkspaceSnapshotSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, _ := SeedOrgWithUser(t, h, "snapshot-executor-org")
		seed := dbtest.WorkspaceSnapshotSeeder{
			BlueprintRun: func(t *testing.T, suffix string) string {
				t.Helper()
				entityID := seedEntity(t, h, orgID, "github", "octo/repo#"+suffix)
				taskID := seedTask(t, h, orgID, userID, entityID, "github:pr:opened")
				return seedBlueprintRun(t, h, orgID, userID, taskID)
			},
			DeleteBlueprintRun: func(t *testing.T, blueprintRunID string) {
				t.Helper()
				MustExec(t, h.AdminDB, `DELETE FROM blueprint_runs WHERE id = $1`, blueprintRunID)
			},
		}
		// The store bundle whose admin half is tf_system — the shape a
		// TF_ROLE=executor process wires in internal/app/stores.go.
		return pgstore.New(h.SystemDB, h.AppDB, SecretKey).WorkspaceSnapshots, orgID, seed
	})
}

// TestTfSystem_MissingGrantFailsTheWayProductionDid proves the guard above
// actually sees a missing grant, by taking one away: revoke the sampler's
// INSERT on sandbox_stats and the very first write the conformance suite makes
// fails with the operator's own error — SQLSTATE 42501, wrapped, naming the
// table.
//
// It exercises that write directly rather than re-running the suite under the
// revoke: a suite that fails is reported through its own *testing.T, which a
// caller cannot assert on. Same call, same connection, same grant state — the
// difference is only that the failure is inspectable here.
func TestTfSystem_MissingGrantFailsTheWayProductionDid(t *testing.T) {
	h := Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// Restore before anything else runs against the shared container — this
	// test is the only thing in the process allowed to see the gap.
	MustExec(t, h.AdminDB, `REVOKE INSERT ON TABLE public.sandbox_stats FROM tf_system`)
	t.Cleanup(func() {
		MustExec(t, h.AdminDB, `GRANT INSERT ON TABLE public.sandbox_stats TO tf_system`)
	})

	store := pgstore.NewSandboxStatStore(h.SystemDB)
	mem := 310
	err := store.RecordBatch(ctx, []domain.SandboxStat{{
		ClaimID:      "33333333-3333-4333-8333-333333333333",
		At:           time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		MemCurrentMB: &mem,
	}})
	if err == nil {
		t.Fatal("RecordBatch succeeded with INSERT revoked — the role-bound run would not catch a missing grant")
	}
	assertPgCode(t, err, "42501", "RecordBatch with INSERT revoked")
	if !strings.Contains(err.Error(), "sandbox_stats") {
		t.Errorf("the failure does not name the table an operator would have to report: %v", err)
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Errorf("the failure reports the table as unknown, which is what sent this bug looking: %v", err)
	}

	// And the reaper's DELETE, still granted, is unaffected: a revoked INSERT
	// must not read as "the whole table is unreachable".
	if _, err := store.Reap(ctx, time.Now().UTC()); err != nil {
		t.Errorf("Reap should be unaffected by a revoked INSERT: %v", err)
	}
}
