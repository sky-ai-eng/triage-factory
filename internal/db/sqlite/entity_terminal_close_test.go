package sqlite_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityStore_CloseTerminal_SQLite runs the shared terminating-close
// suite against the SQLite impl. Each subtest gets a fresh in-memory DB, so
// the exact-count assertions can't pick up a sibling's rows.
func TestEntityStore_CloseTerminal_SQLite(t *testing.T) {
	dbtest.RunEntityTerminalCloseConformance(t, func(t *testing.T) (db.EntityStore, db.TaskStore, string, dbtest.EntityTerminalCloseSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		base, entityForTask := newSQLiteCloseIntentSeeder(conn)
		seeder := dbtest.EntityTerminalCloseSeeder{
			TaskCloseCancelIntentSeeder: base,
			EntityForTask: func(t *testing.T, taskID string) string {
				t.Helper()
				id, ok := entityForTask[taskID]
				if !ok {
					t.Fatalf("no seeded entity for task %s", taskID)
				}
				return id
			},
			TaskOnEntity: func(t *testing.T, entityID, eventType string) string {
				t.Helper()
				taskID := uuid.New().String()
				eventID := uuid.New().String()
				now := time.Now().UTC()
				if _, err := conn.Exec(`
					INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
					VALUES (?, ?, ?, '', '{}', ?)
				`, eventID, entityID, eventType, now); err != nil {
					t.Fatalf("seed event: %v", err)
				}
				if _, err := conn.Exec(`
					INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
					                   status, priority_score, scoring_status, created_at,
					                   team_id, visibility)
					VALUES (?, ?, ?, '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
				`, taskID, entityID, eventType, eventID, now, runmode.LocalDefaultTeamID); err != nil {
					t.Fatalf("seed task: %v", err)
				}
				entityForTask[taskID] = entityID
				return taskID
			},
			EntityRow: func(t *testing.T, entityID string) (string, int64) {
				t.Helper()
				var state string
				var seq int64
				if err := conn.QueryRow(`SELECT state, poll_seq FROM entities WHERE id = ?`, entityID).Scan(&state, &seq); err != nil {
					t.Fatalf("read entity %s: %v", entityID, err)
				}
				return state, seq
			},
			BumpPollSeq: func(t *testing.T, entityID string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE entities SET poll_seq = poll_seq + 1 WHERE id = ?`, entityID); err != nil {
					t.Fatalf("bump poll_seq for %s: %v", entityID, err)
				}
			},
			CloseEntityRaw: func(t *testing.T, entityID string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?`, time.Now().UTC(), entityID); err != nil {
					t.Fatalf("close entity %s around the guard: %v", entityID, err)
				}
			},
		}
		stores := sqlitestore.New(conn)
		return stores.Entities, stores.Tasks, runmode.LocalDefaultOrgID, seeder
	})
}
