package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestDashboardBackfillMarker_Postgres runs the shared TFAC-396 marker
// conformance against the Postgres impl. AdminDB serves both pool slots so the
// `...System` marker read/write exercise the admin-pool (BYPASSRLS) seam — the
// production path for the claims-free backfill worker. Skips cleanly when
// Docker isn't available (pgtest.Shared).
func TestDashboardBackfillMarker_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunDashboardBackfillMarkerConformance(t, func(t *testing.T) (db.UsersStore, func(t *testing.T) string) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		var n int
		seedUser := func(t *testing.T) string {
			t.Helper()
			n++
			return pgtest.SeedUser(t, h, fmt.Sprintf("dashbf-u%d", n))
		}
		return stores.Users, seedUser
	})
}
