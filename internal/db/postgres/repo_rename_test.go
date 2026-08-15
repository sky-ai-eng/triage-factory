package postgres_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestRepoRename_Postgres runs the shared rename conformance suite against the
// Postgres backend. Both pools are wired against AdminDB: the rewrite is an
// admin-pool operation by design (it spans tables owned by every team in the
// org), so the app pool has nothing to say about it.
func TestRepoRename_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunRepoRenameConformance(t, func(t *testing.T) (db.Stores, string, dbtest.RepoRenameSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "rename")
		seed := dbtest.RepoRenameSeeder{
			TeamID: teamID,
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedPgArtifactRun(t, h, orgID, teamID, userID)
			},
			Task: func(t *testing.T, entityID, suffix string) string {
				t.Helper()
				const eventType = "github:pr:opened"
				eventID := uuid.New().String()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json)
					VALUES ($1, $2, $3, $4, $5, '{}'::jsonb)
				`, eventID, orgID, entityID, eventType, suffix); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				taskID := uuid.New().String()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id,
					                   event_type, dedup_key, primary_event_id, status, scoring_status)
					VALUES ($1, $2, $3, $4, 'team', $5, $6, $7, $8, 'queued', 'pending')
				`, taskID, orgID, userID, teamID, entityID, eventType, suffix, eventID); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				return taskID
			},
			CountTasks: func(t *testing.T) int {
				t.Helper()
				var n int
				if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE org_id = $1`, orgID).Scan(&n); err != nil {
					t.Fatalf("count tasks: %v", err)
				}
				return n
			},
		}
		return stores, orgID, seed
	})
}
