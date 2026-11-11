package postgres_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestConversationFrameRecipients_Postgres runs the shared
// ConversationFrameRecipientsSystem suite against the Postgres impl.
// AdminDB serves both pool slots so the suite exercises the admin-pool read
// directly, which is the production seam: the emitters hold no request
// claims, and the audience crosses team boundaries the membership policies
// would hide.
func TestConversationFrameRecipients_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunConversationFrameRecipientsConformance(t, func(t *testing.T) (db.ConversationStore, dbtest.ConversationFrameRecipientsSeeder) {
		t.Helper()
		h.Reset(t)
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

		var n int // per-subtest uniqueness for display names and slugs
		seed := dbtest.ConversationFrameRecipientsSeeder{
			User: func(t *testing.T) string {
				t.Helper()
				n++
				return pgtest.SeedUser(t, h, fmt.Sprintf("convframe-u%d", n))
			},
			Org: func(t *testing.T, ownerID string) string {
				t.Helper()
				n++
				return pgtest.SeedOrg(t, h, fmt.Sprintf("convframe-org%d", n), ownerID)
			},
			Team: func(t *testing.T, orgID string) string {
				t.Helper()
				n++
				return pgtest.SeedTeam(t, h, orgID, fmt.Sprintf("convframe-team%d", n))
			},
			OrgMembership: func(t *testing.T, userID, orgID, role string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, $3::org_role)`,
					userID, orgID, role)
			},
			TeamMembership: func(t *testing.T, userID, teamID, role string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, $3)`,
					userID, teamID, role)
			},
			RemoveTeamMembership: func(t *testing.T, userID, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB,
					`DELETE FROM memberships WHERE user_id = $1 AND team_id = $2`, userID, teamID)
			},
			ArchiveTeam: func(t *testing.T, teamID string) {
				t.Helper()
				pgtest.MustExec(t, h.AdminDB, `UPDATE teams SET deleted_at = now() WHERE id = $1`, teamID)
			},
			Conversation: func(t *testing.T, orgID, teamID, creatorID, visibility string) string {
				t.Helper()
				id := uuid.NewString()
				pgtest.MustExec(t, h.AdminDB, `
					INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
					VALUES ($1, $2, $3, $4, 'manual', 'interactive', 'running', $5)
				`, id, orgID, teamID, creatorID, visibility)
				return id
			},
		}
		return stores.Conversations, seed
	})
}
