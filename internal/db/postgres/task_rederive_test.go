package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// TestTaskReDeriveStore_Postgres runs the shared conformance suite against
// the Postgres TaskReDeriveStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path; the task
// chains come from the pending-firings seeder, since a completion's effect
// is a firing admission and needs the same parents. Skips without Docker
// like every pgtest file.
func TestTaskReDeriveStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunTaskReDeriveStoreConformance(t, func(t *testing.T) dbtest.TaskReDeriveFixture {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		seed := newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)
		return dbtest.TaskReDeriveFixture{
			Store:   stores.TaskReDerive,
			Scores:  stores.Scores,
			Firings: stores.PendingFirings,
			OrgID:   orgID,
			Tuple:   seed.Tuple,
			QueueRows: func(t *testing.T, taskID string) []dbtest.ReDeriveQueueRow {
				t.Helper()
				return readPgReDeriveRows(t, h.AdminDB, taskID)
			},
			ExpireLease: func(t *testing.T, id int64) {
				t.Helper()
				// Rewound against the server clock — the one the claim
				// stamped the lease from and the guard compares against.
				pgExecOne(t, h, "expire lease",
					`UPDATE task_rederive_queue SET lease_expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1 AND status = 'leased'`, id)
			},
		}
	})
}

// TestTaskReDeriveWorkItem_Postgres runs the table-agnostic half of the
// work-item conformance suite against the production task_rederive_queue
// table, as the baseline builds it, under the kind production declares.
// Each admission needs a task for the table's foreign key, which the
// pending-firings seeder's tuple helper inserts along with the rest of its
// chain.
func TestTaskReDeriveWorkItem_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		seed := newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)
		cols := func(int) map[string]any {
			return db.TaskReDeriveRowCols(seed.Tuple(t).TaskID)
		}
		return h.AdminDB, workkinds.TaskReDerive(workitem.Postgres), orgID, cols
	})
}
