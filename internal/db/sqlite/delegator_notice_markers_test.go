package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMigrate_DelegatorNoticeMarkers_SQLite pins the upgrade: a failure or a
// park already on disk was told, or lost, by the build that recorded it, so
// the brain's sweep sends no notice for it. A failure recorded after the
// upgrade is still owed one.
func TestMigrate_DelegatorNoticeMarkers_SQLite(t *testing.T) {
	ctx := context.Background()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	// The migration before the markers.
	const prior = 202611150001
	if err := db.BootstrapSchemaAtForTest(conn, prior); err != nil {
		t.Fatalf("schema at %d: %v", prior, err)
	}
	if err := db.SeedLocalTenantRows(ctx, conn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// events.event_type references the catalog, which Migrate seeds after
	// the last migration and this database has not reached.
	if err := db.SeedEventTypes(conn, "sqlite3"); err != nil {
		t.Fatalf("seed the events catalog: %v", err)
	}
	stores := sqlitestore.New(conn)
	org, team, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", dbtest.TestGitHubHost,
		"octo/notices#1", "PR_notices1", domain.EntityKindPullRequest, "t", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev-notices', ?, 'github:pr:opened', '')`, entity.ID)
	exec(`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES ('t-notices', ?, 'github:pr:opened', 'ev-notices', 'in_progress')`, entity.ID)
	// failed is a failure stored before the upgrade; parked a park that waits
	// on the delegator.
	failed := func(id string) {
		t.Helper()
		exec(`
			INSERT INTO conversations (id, org_id, team_id, task_id, type, origin, trigger_type, creator_user_id,
			                           status, failure_kind, result_summary, completed_at)
			VALUES (?, ?, ?, 't-notices', 'delegation', 'interactive', 'manual', ?, 'failed', 'crash', 'exit 1', '2026-11-01 00:00:00')`,
			id, org, team, user)
	}
	failed("c-failed")
	exec(`
		INSERT INTO conversations (id, org_id, team_id, task_id, type, origin, trigger_type, creator_user_id,
		                           status, park_reason, parked_at)
		VALUES ('c-parked', ?, ?, 't-notices', 'delegation', 'interactive', 'manual', ?, 'open', ?, '2026-11-01 00:00:00')`,
		org, team, user, domain.ParkReasonStalled)

	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	failures, err := stores.Conversations.ListFailureNoticesDueSystem(ctx, 100)
	if err != nil {
		t.Fatalf("ListFailureNoticesDueSystem: %v", err)
	}
	pauses, err := stores.Conversations.ListPauseNoticesDueSystem(ctx, 100)
	if err != nil {
		t.Fatalf("ListPauseNoticesDueSystem: %v", err)
	}
	if len(failures) != 0 || len(pauses) != 0 {
		t.Errorf("due after the upgrade: failures %+v, pauses %+v, want none", failures, pauses)
	}

	// Without the backfill the same row would be owed a notice.
	failed("c-failed-after")
	failures, err = stores.Conversations.ListFailureNoticesDueSystem(ctx, 100)
	if err != nil {
		t.Fatalf("ListFailureNoticesDueSystem: %v", err)
	}
	if len(failures) != 1 || failures[0].ConversationID != "c-failed-after" {
		t.Errorf("due after a failure recorded since the upgrade = %+v, want only c-failed-after", failures)
	}
}
