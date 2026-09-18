package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
)

// TestWorkItem_Postgres runs the shared work-item conformance suite against
// Postgres. The admin (BYPASSRLS) pool is deliberate: the fixture tables carry
// no policies, and the contract under test is lease fencing rather than the
// auth path.
//
// The org is a fresh uuid per subtest rather than a seeded row — the fixture
// carries no foreign key, so an adopting table's own references stay its
// business.
func TestWorkItem_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	workitemtest.Run(t, func(t *testing.T) (*sql.DB, workitem.Dialect, string) {
		t.Helper()
		h.Reset(t)
		return h.AdminDB, workitem.Postgres, uuid.NewString()
	})
}
