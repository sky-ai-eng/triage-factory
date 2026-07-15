package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// operators is keyed on email (no FK), so nothing needs seeding.
func TestOperatorStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunOperatorStoreConformance(t, func(t *testing.T) db.OperatorStore {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewOperatorStore(h.AdminDB)
	})
}
