package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestConversationFrameRecipients_SQLite runs the shared
// ConversationFrameRecipientsSystem suite against the SQLite impl, which runs
// the identical query to Postgres so the suite pins both backends to one
// result.
func TestConversationFrameRecipients_SQLite(t *testing.T) {
	dbtest.RunConversationFrameRecipientsConformance(t, func(t *testing.T) (db.ConversationStore, dbtest.ConversationFrameRecipientsSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		exec := func(t *testing.T, query string, args ...any) {
			t.Helper()
			if _, err := conn.Exec(query, args...); err != nil {
				t.Fatalf("exec %q: %v", query, err)
			}
		}
		seed := dbtest.ConversationFrameRecipientsSeeder{
			User: func(t *testing.T) string {
				id := uuid.NewString()
				exec(t, `INSERT INTO users (id, display_name) VALUES (?, ?)`, id, "user-"+id[:8])
				return id
			},
			Org: func(t *testing.T, _ string) string {
				// SQLite orgs has no owner column, so ownerID is unused; the
				// uuid doubles as the globally-unique slug.
				id := uuid.NewString()
				exec(t, `INSERT INTO orgs (id, slug, name) VALUES (?, ?, ?)`, id, id, "org-"+id[:8])
				return id
			},
			Team: func(t *testing.T, orgID string) string {
				id := uuid.NewString()
				exec(t, `INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, orgID, id, "team-"+id[:8])
				return id
			},
			OrgMembership: func(t *testing.T, userID, orgID, role string) {
				exec(t, `INSERT INTO org_memberships (user_id, org_id, role) VALUES (?, ?, ?)`, userID, orgID, role)
			},
			RemoveOrgMembership: func(t *testing.T, userID, orgID string) {
				exec(t, `DELETE FROM org_memberships WHERE user_id = ? AND org_id = ?`, userID, orgID)
			},
			TeamMembership: func(t *testing.T, userID, teamID, role string) {
				exec(t, `INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, ?)`, userID, teamID, role)
			},
			RemoveTeamMembership: func(t *testing.T, userID, teamID string) {
				exec(t, `DELETE FROM memberships WHERE user_id = ? AND team_id = ?`, userID, teamID)
			},
			ArchiveTeam: func(t *testing.T, teamID string) {
				exec(t, `UPDATE teams SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, teamID)
			},
			Conversation: func(t *testing.T, orgID, teamID, creatorID, visibility string) string {
				id := uuid.NewString()
				exec(t, `
					INSERT INTO conversations (id, org_id, team_id, creator_user_id, trigger_type, origin, status, visibility)
					VALUES (?, ?, ?, ?, 'manual', 'interactive', 'running', ?)
				`, id, orgID, teamID, creatorID, visibility)
				return id
			},
		}
		return stores.Conversations, seed
	})
}
