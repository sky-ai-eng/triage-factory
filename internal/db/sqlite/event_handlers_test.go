package sqlite_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEventHandlerStore_SQLite runs the shared conformance suite
// against the SQLite impl. Trigger fixtures FK to blueprints(id, org_id);
// the seedBlueprints hook inserts the named rows via BlueprintStore so the
// harness stays schema-blind.
func TestEventHandlerStore_SQLite(t *testing.T) {
	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.BlueprintSeeder, dbtest.ShippedHandlerIDBySlug) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		orgID := runmode.LocalDefaultOrgID
		teamID := runmode.LocalDefaultTeamID
		seed := func(t *testing.T, slugs ...string) map[string]string {
			t.Helper()
			out := make(map[string]string, len(slugs))
			for _, slug := range slugs {
				// The trigger→blueprint FK only needs a same-team blueprint row to
				// exist; a plain user blueprint satisfies it (the seeded system-slug
				// shape is exercised in the shipped-defaults sync suite).
				id := uuid.New().String()
				if _, err := stores.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{
					ID: id, Name: slug, Source: "user",
				}); err != nil {
					t.Fatalf("seed blueprint %s: %v", slug, err)
				}
				out[slug] = id
			}
			return out
		}
		return stores.EventHandlers, orgID, teamID, seed, shippedHandlerIDBySlug(conn, orgID)
	})
}

// TestTriggerDelete_SQLite runs the trigger-delete conformance suite against
// the SQLite impl. The conversation is staged with dbtest.SeedConversation and
// its trigger_id read back raw.
func TestTriggerDelete_SQLite(t *testing.T) {
	dbtest.RunTriggerDeleteConformance(t, func(t *testing.T) dbtest.TriggerDeleteFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		orgID := runmode.LocalDefaultOrgID
		teamID := runmode.LocalDefaultTeamID
		return dbtest.TriggerDeleteFixture{
			Handlers: stores.EventHandlers,
			OrgID:    orgID,
			TeamID:   teamID,
			Blueprint: func(t *testing.T) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := stores.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{
					ID: id, Name: "trigger-delete", Source: "user",
				}); err != nil {
					t.Fatalf("seed blueprint: %v", err)
				}
				return id
			},
			Run: func(t *testing.T, triggerID string) string {
				t.Helper()
				id := uuid.New().String()
				dbtest.SeedConversation(t, conn, domain.Conversation{
					ID: id, TriggerType: "event", TriggerID: triggerID, Status: dbtest.SeedConcluded,
				})
				return id
			},
			RunTriggerID: func(t *testing.T, conversationID string) string {
				t.Helper()
				var triggerID string
				if err := conn.QueryRowContext(t.Context(),
					`SELECT COALESCE(trigger_id, '') FROM conversations WHERE id = ?`, conversationID,
				).Scan(&triggerID); err != nil {
					t.Fatalf("read trigger_id of conversation %s: %v", conversationID, err)
				}
				return triggerID
			},
		}
	})
}

// TestArchivedTeamHandlers_SQLite runs the archived-team handler suite against
// the SQLite impl. Local mode never archives its team, but the store's read
// answers the same, so a second team is seeded raw.
func TestArchivedTeamHandlers_SQLite(t *testing.T) {
	dbtest.RunArchivedTeamHandlersConformance(t, func(t *testing.T) dbtest.ArchivedTeamHandlersFixture {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		orgID := runmode.LocalDefaultOrgID
		return dbtest.ArchivedTeamHandlersFixture{
			Handlers: stores.EventHandlers,
			Teams:    stores.Teams,
			OrgID:    orgID,
			TeamID:   runmode.LocalDefaultTeamID,
			SeedTeam: func(t *testing.T) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.ExecContext(t.Context(),
					`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`, id, orgID, "archived-"+id[:8], "Archived",
				); err != nil {
					t.Fatalf("seed team: %v", err)
				}
				return id
			},
			SeedRule: func(t *testing.T, teamID string) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.ExecContext(t.Context(), `
					INSERT INTO event_handlers
						(id, org_id, team_id, creator_user_id, kind, event_type, enabled, source,
						 name, default_priority, sort_order, created_at, updated_at)
					VALUES (?, ?, ?, ?, 'rule', ?, 1, 'user', ?, 0.5, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
				`, id, orgID, teamID, runmode.LocalDefaultUserID, domain.EventGitHubPRCICheckFailed, "rule "+id[:8]); err != nil {
					t.Fatalf("seed rule on team %s: %v", teamID, err)
				}
				return id
			},
		}
	})
}

// shippedHandlerIDBySlug is the SQLite dbtest.ShippedHandlerIDBySlug: a raw
// read of event_handlers by (org_id, team_id, system_slug), live rows only.
func shippedHandlerIDBySlug(conn *sql.DB, orgID string) dbtest.ShippedHandlerIDBySlug {
	return func(t *testing.T, teamID, slug string) string {
		t.Helper()
		var id string
		err := conn.QueryRowContext(t.Context(), `
			SELECT id FROM event_handlers
			WHERE org_id = ? AND team_id = ? AND system_slug = ? AND deleted_at IS NULL
		`, orgID, teamID, slug).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("resolve shipped handler %q: %v", slug, err)
		}
		return id
	}
}
