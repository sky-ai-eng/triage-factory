package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestPlacementOverrideStore_Postgres_Conformance runs the shared contract
// against the Postgres impl. An override references its repository's
// registry row, which references its org, so each case seeds an org and
// registers repositories through the admin-pool repository store.
func TestPlacementOverrideStore_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunPlacementOverrideStoreConformance(t, func(t *testing.T) (db.PlacementOverrideStore, db.RepositoryStore, string) {
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "placement")
		return pgstore.NewPlacementOverrideStore(h.AdminDB), stores.Repos, orgID
	})
}
