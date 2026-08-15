package postgres_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestGitHubNumericIDs_Postgres runs the shared numeric-id conformance suite
// against the Postgres impl. AdminDB serves both pool slots: the installation
// mirror is admin-pool-only by RLS (tf_app is denied every write to it), and
// the identity upsert's app-pool policy gates on tf.current_user_id(), which
// this suite has no session claims for. The seeder fixtures go through
// h.AdminDB via the pgtest helpers, and read github_user_id there too — no
// store method surfaces that column.
func TestGitHubNumericIDs_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunGitHubNumericIDConformance(t, func(t *testing.T) (dbtest.GitHubNumericIDStores, dbtest.GitHubNumericIDSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for display names / slugs
		seed := dbtest.GitHubNumericIDSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("ghids-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("ghids-org%d", n), ownerID)
			},
			GitHubUserID: func(t *testing.T, userID, githubBaseURL string) string {
				t.Helper()
				var id sql.NullString
				err := h.AdminDB.QueryRow(
					`SELECT github_user_id FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2`,
					userID, db.NormalizeGitHubHost(githubBaseURL),
				).Scan(&id)
				if errors.Is(err, sql.ErrNoRows) {
					return ""
				}
				if err != nil {
					t.Fatalf("read github_user_id: %v", err)
				}
				return id.String
			},
		}
		return dbtest.GitHubNumericIDStores{Users: stores.Users, GitHubApps: stores.GitHubApps}, seed
	})
}
