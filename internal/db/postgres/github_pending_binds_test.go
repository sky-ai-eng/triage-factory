package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubPendingBinds_Postgres runs the shared pending-bind conformance
// suite against the Postgres impl. AdminDB serves both pool slots: the table
// has RLS enabled with no policy at all, so the admin pool is the only role
// that can reach it — which is the store's own posture, not a test shortcut.
//
// An org and a user are seeded because org_id carries a real foreign key and
// both columns are uuid-typed; the user has no FK (the callback only ever
// compares user_id against the returning session's subject) but must still be a
// well-formed uuid.
func TestGitHubPendingBinds_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubPendingBindConformance(t, func(t *testing.T) dbtest.GitHubPendingBindBackend {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		userID := pgtest.SeedUser(t, h, "bind-suite")
		orgID := pgtest.SeedOrg(t, h, "bind-suite-org", userID)

		return dbtest.GitHubPendingBindBackend{
			Store:  stores.GitHubPendingBinds,
			OrgID:  orgID,
			UserID: userID,
		}
	})
}
