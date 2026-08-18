package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRepoReferences_Postgres runs the shared repository-reference conformance
// suite against the Postgres backend. Both pools are wired against AdminDB:
// the suite exercises tracked-set writes and system reads whose gating is the
// subject of the RLS tests, not of this one.
func TestRepoReferences_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunRepoReferenceConformance(t, func(t *testing.T) (db.Stores, string, string, func(*testing.T, string) string) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "reporef")
		conversation := func(t *testing.T, suffix string) string {
			t.Helper()
			return seedPgArtifactConversation(t, h, orgID, teamID, userID)
		}
		return stores, orgID, teamID, conversation
	})
}
