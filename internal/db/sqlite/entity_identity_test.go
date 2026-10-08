package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityKey_SQLite runs the key half of the entity identity model against
// the SQLite EntityStore, a fresh database per subtest.
func TestEntityKey_SQLite(t *testing.T) {
	dbtest.RunEntityKeyConformance(t, func(t *testing.T) (db.EntityStore, string) {
		t.Helper()
		return sqlitestore.New(newSQLiteForEntityTest(t)).Entities, runmode.LocalDefaultOrgID
	})
}

// TestEntityIdentity_SQLite runs the identity writes — the id lookup, the id
// stamp and the rename — against the SQLite backend.
func TestEntityIdentity_SQLite(t *testing.T) {
	dbtest.RunEntityIdentityConformance(t, func(t *testing.T) (db.Stores, string, dbtest.EntityIdentitySeeder) {
		t.Helper()
		conn := newSQLiteForEntityTest(t)
		seed := dbtest.EntityIdentitySeeder{
			TeamID: runmode.LocalDefaultTeamID,
			Task: func(t *testing.T, entityID, suffix string) string {
				t.Helper()
				return seedSQLiteIdentityTask(t, conn, entityID, suffix)
			},
			RawActionURL: func(t *testing.T, dedupKey string) (string, string) {
				t.Helper()
				var u, cur string
				if err := conn.QueryRow(
					`SELECT COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions WHERE dedup_key = ?`,
					dedupKey,
				).Scan(&u, &cur); err != nil {
					t.Fatalf("read action row %q: %v", dedupKey, err)
				}
				return u, cur
			},
			// The database is this subtest's alone, so the index stays dropped.
			ForceExternalID: func(t *testing.T, entityID, externalID string) {
				t.Helper()
				if _, err := conn.Exec(`DROP INDEX entities_identity`); err != nil {
					t.Fatalf("drop identity index: %v", err)
				}
				if _, err := conn.Exec(`UPDATE entities SET external_id = ? WHERE id = ?`, externalID, entityID); err != nil {
					t.Fatalf("force external id: %v", err)
				}
			},
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				conversationID, _ := seedSQLiteConversationForTaskMemory(t, conn, suffix)
				return conversationID
			},
			Link: func(t *testing.T, from, to string) {
				t.Helper()
				if _, err := conn.Exec(
					`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES (?, ?, 'relates', 'agent')`,
					from, to,
				); err != nil {
					t.Fatalf("seed entity link: %v", err)
				}
			},
			Referents: func(t *testing.T, entityID string) dbtest.EntityReferents {
				t.Helper()
				return sqliteEntityReferents(t, conn, entityID)
			},
		}
		return sqlitestore.New(conn), runmode.LocalDefaultOrgID, seed
	})
}

// seedSQLiteIdentityTask inserts an event and a queued task on entityID, with
// suffix as the task's dedup_key, so two tasks seeded with one suffix hold the
// same (event_type, dedup_key) slot.
func seedSQLiteIdentityTask(t *testing.T, conn *sql.DB, entityID, suffix string) string {
	t.Helper()
	const eventType = "jira:issue:assigned"
	eventID := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, ?)`,
		eventID, entityID, eventType, suffix,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
		VALUES (?, ?, ?, ?, ?, 'queued')
	`, taskID, entityID, eventType, suffix, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

func sqliteEntityReferents(t *testing.T, conn *sql.DB, entityID string) dbtest.EntityReferents {
	t.Helper()
	out := dbtest.EntityReferents{Rows: map[string]int{}, MemoryRoles: map[string]string{}}
	for table, query := range map[string]string{
		"tasks":                        `SELECT COUNT(*) FROM tasks WHERE entity_id = ?`,
		"events":                       `SELECT COUNT(*) FROM events WHERE entity_id = ?`,
		"event_queue":                  `SELECT COUNT(*) FROM event_queue WHERE entity_id = ?`,
		"pending_firings":              `SELECT COUNT(*) FROM pending_firings WHERE entity_id = ?`,
		"conversation_memory_entities": `SELECT COUNT(*) FROM conversation_memory_entities WHERE entity_id = ?`,
		"entity_links":                 `SELECT COUNT(*) FROM entity_links WHERE from_entity_id = ?1 OR to_entity_id = ?1`,
	} {
		var n int
		if err := conn.QueryRow(query, entityID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out.Rows[table] = n
	}
	rows, err := conn.Query(`SELECT conversation_id, role FROM conversation_memory_entities WHERE entity_id = ?`, entityID)
	if err != nil {
		t.Fatalf("read memory roles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var conv, role string
		if err := rows.Scan(&conv, &role); err != nil {
			t.Fatalf("scan memory role: %v", err)
		}
		out.MemoryRoles[conv] = role
	}
	return out
}
