package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEventQueueWorkItem_SQLite runs the table-agnostic half of the
// work-item conformance suite against the production event_queue table, as
// the migrated schema builds it, under the kind production declares. Each
// admission needs an events row (and that row an entity) for the table's
// foreign keys, which is what the factory's column hook inserts.
func TestEventQueueWorkItem_SQLite(t *testing.T) {
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		conn := newSQLiteForEventQueueTest(t)
		cols := func(int) map[string]any {
			suf := uuid.New().String()[:8]
			entityID := "e-" + suf
			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url)
				VALUES (?, 'github', ?, 'pr', 'Test PR', '')
			`, entityID, "owner/repo#"+suf); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			eventID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json)
				VALUES (?, ?, ?, '', '{}')
			`, eventID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			return map[string]any{
				"event_id":        eventID,
				"entity_id":       entityID,
				"event_type":      domain.EventGitHubPRCICheckFailed,
				"traceparent":     nil,
				"entity_poll_seq": nil,
			}
		}
		return conn, workkinds.EventQueue(workitem.SQLite), runmode.LocalDefaultOrgID, cols
	})
}
