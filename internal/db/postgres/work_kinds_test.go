package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestWorkKindRegistry_Postgres runs the registry assertions over the bundle
// the Postgres constructor builds. Skips without Docker like every pgtest
// file.
func TestWorkKindRegistry_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	dbtest.AssertWorkKindRegistry(t, pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).WorkKinds)
}
