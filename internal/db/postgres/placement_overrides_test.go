package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestPlacementOverrideStore_Postgres_Conformance runs the shared contract
// against the Postgres impl. placement_overrides has no org FK (org_id is a
// plain text column, instances-family convention), so a synthetic org id is
// fine — no orgs row needs seeding.
func TestPlacementOverrideStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunPlacementOverrideStoreConformance(t, func(t *testing.T) (db.PlacementOverrideStore, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewPlacementOverrideStore(h.AdminDB), "22222222-2222-2222-2222-222222222222"
	})
}
