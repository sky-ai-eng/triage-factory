package postgres_test

import (
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestOrgMemberList_Postgres runs the org roster's filter suite. AdminDB serves
// both pool slots, so it pins the query's own semantics; the RLS arm and the
// email gate are the handler tests', which run under real claims.
func TestOrgMemberList_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunOrgMemberListConformance(t, func(t *testing.T) (db.OrgMembershipsStore, dbtest.OrgMemberListSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for auth emails and slugs
		seed := dbtest.OrgMemberListSeeder{
			User: func(t *testing.T, displayName string) string {
				t.Helper()
				n++
				id := pgtest.SeedUser(t, h, fmt.Sprintf("org-list-u%d", n))
				pgtest.MustExec(t, h.AdminDB,
					`UPDATE users SET display_name = $2 WHERE id = $1`, id, displayName)
				return id
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				org := pgtest.SeedOrg(t, h, fmt.Sprintf("org-list-org%d", n), ownerID)
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, ownerID, org)
				return org
			},
			OrgMember: func(t *testing.T, userID, orgID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, userID, orgID)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("org-list-team%d", n))
			},
			TeamMember: func(t *testing.T, userID, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, userID, teamID)
			},
			SignInEmail: func(t *testing.T, userID, email string, verified bool) {
				t.Helper()
				pgtest.SeedIdentity(t, h, userID, email, verified)
			},
		}
		return stores.OrgMemberships, seed
	})
}
