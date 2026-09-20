package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitemtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEventQueueWorkItem_Postgres runs the table-agnostic half of the
// work-item conformance suite against the production event_queue table, as
// the baseline builds it, under the kind production declares. Each admission
// needs an events row (and that row an entity) for the table's foreign keys,
// which is what the factory's column hook inserts. Skips without Docker like
// every pgtest file.
func TestEventQueueWorkItem_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	workitemtest.RunTable(t, func(t *testing.T) (*sql.DB, workitem.Kind, string, func(int) map[string]any) {
		t.Helper()
		h.Reset(t)
		orgID, _ := seedPgEventQueueOrg(t, h)
		seeder := newPgEventQueueSeeder(h, orgID)
		cols := func(int) map[string]any {
			entityID := seeder.Entity(t)
			eventID := uuid.New().String()
			if _, err := h.AdminDB.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
			`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
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
		return h.AdminDB, workkinds.EventQueue(workitem.Postgres), orgID, cols
	})
}
