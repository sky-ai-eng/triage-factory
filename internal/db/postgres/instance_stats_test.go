package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// instance_stats has no org FK (instance_id is a plain text column, the
// instances-family convention), so no orgs row needs seeding.
func TestInstanceStatStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunInstanceStatStoreConformance(t, func(t *testing.T) db.InstanceStatStore {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewInstanceStatStore(h.AdminDB)
	})
}
