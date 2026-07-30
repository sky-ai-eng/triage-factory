package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// sandbox_stats has no FK to claims (the telemetry-family convention — a
// best-effort batch must never be rejected by referential integrity), so no
// claims or orgs rows need seeding.
func TestSandboxStatStore_Postgres_Conformance(t *testing.T) {
	dbtest.RunSandboxStatStoreConformance(t, func(t *testing.T) db.SandboxStatStore {
		h := pgtest.Shared(t)
		h.Reset(t)
		return pgstore.NewSandboxStatStore(h.AdminDB)
	})
}
