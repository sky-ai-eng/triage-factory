package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
)

// TestPendingFiringsWorkItem_Postgres runs the table-agnostic half of the
// work-item conformance suite against the production pending_firings table,
// as the baseline builds it, under the kind production declares. Each
// admission needs the entity, task, handler and event rows the table's
// foreign keys name, which the conformance seeder's tuple helper inserts.
// Skips without Docker like every pgtest file.
func TestPendingFiringsWorkItem_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		seed := newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)
		cols := func(int) map[string]any {
			tup := seed.Tuple(t)
			return map[string]any{
				"entity_id":           tup.EntityID,
				"task_id":             tup.TaskID,
				"trigger_id":          tup.TriggerID,
				"triggering_event_id": tup.EventID,
			}
		}
		return h.AdminDB, workkinds.PendingFirings(workitem.Postgres), orgID, cols
	})
}
