package postgres_test

import (
	"sort"
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
				return seedPgArtifactConversation(t, h, orgID, teamID, userID)
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
			ReferenceRows: func(t *testing.T) []string {
				t.Helper()
				return pgCategoryAReferences(t, h, orgID)
			},
			RawActionURL: func(t *testing.T, dedupKey string) (string, string) {
				t.Helper()
				var u, cur string
				if err := h.AdminDB.QueryRow(
					`SELECT COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions WHERE org_id = $1 AND dedup_key = $2`,
					orgID, dedupKey,
				).Scan(&u, &cur); err != nil {
					t.Fatalf("read action row %q: %v", dedupKey, err)
				}
				return u, cur
			},
		}
		return stores, orgID, seed
	})
}

// pgCategoryAReferences reads the raw stored repository reference out of each
// table that carries one, so the rename suite can assert none of them moved.
// Raw SQL on purpose: a store read would resolve the id back to a slug, which
// is exactly the layer that would hide a rewrite.
func pgCategoryAReferences(t *testing.T, h *pgtest.Harness, orgID string) []string {
	t.Helper()
	queries := []struct{ label, query string }{
		{"team_github_repos", `
			SELECT g.team_id::text || '|' || g.repository_id::text
			  FROM team_github_repos g
			  JOIN teams tm ON tm.id = g.team_id
			 WHERE tm.org_id = $1`},
		{"conversation_worktrees", `
			SELECT conversation_id::text || '|' || repository_id::text || '|' || ref
			  FROM conversation_worktrees WHERE org_id = $1`},
	}
	var out []string
	for _, q := range queries {
		rows, err := h.AdminDB.Query(q.query, orgID)
		if err != nil {
			t.Fatalf("read %s references: %v", q.label, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatalf("scan %s reference: %v", q.label, err)
			}
			out = append(out, q.label+":"+row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("read %s references: %v", q.label, err)
		}
		rows.Close()
	}
	sort.Strings(out)
	return out
}
