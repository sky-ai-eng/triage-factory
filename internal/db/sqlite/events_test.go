package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEventStore_SQLite runs the shared conformance suite against
// the SQLite EventStore impl. Each subtest opens a fresh in-memory
// DB so events state doesn't leak between assertions.
func TestEventStore_SQLite(t *testing.T) {
	dbtest.RunEventStoreConformance(t, func(t *testing.T) (db.EventStore, string, dbtest.EventStoreSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.EventStoreSeeder{
			Entity: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteEntityForEvents(t, conn, suffix)
			},
		}
		return stores.Events, runmode.LocalDefaultOrgID, seed
	})
}

// TestEventStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// gate at every method entry. Non-default org IDs are a confused
// caller (they think they're in multi mode); reject loudly so the
// silent default-org fallthrough never happens.
func TestEventStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const badOrg = "11111111-1111-1111-1111-111111111111"
	evt := domain.Event{EventType: domain.EventGitHubPROpened}
	if _, err := stores.Events.Record(ctx, badOrg, evt); err == nil {
		t.Error("Record(non-local org) should error")
	}
	if _, err := stores.Events.RecordSystem(ctx, badOrg, evt); err == nil {
		t.Error("RecordSystem(non-local org) should error")
	}
	if _, err := stores.Events.LatestForEntityTypeAndDedupKey(ctx, badOrg, "any", "any", ""); err == nil {
		t.Error("Latest(non-local org) should error")
	}
	if _, err := stores.Events.GetMetadataSystem(ctx, badOrg, "any"); err == nil {
		t.Error("GetMetadataSystem(non-local org) should error")
	}
}

// seedSQLiteEntityForEvents inserts an active GitHub PR entity for
// EventStore conformance fixtures. Direct INSERT (not the
// EntityStore) so the conformance harness stays close to the
// SwipeStore/RepositoryStore seeding pattern — small, schema-coupled, and
// independent of other stores' behavior changes.
func seedSQLiteEntityForEvents(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("events-conformance-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', 'Events Conformance', 'https://example/x', '{}', ?)
	`, id, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}
