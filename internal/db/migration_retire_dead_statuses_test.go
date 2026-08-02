package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that retires 'cancelled' / 'pending_approval' /
// 'task_unsolvable' from conversations.status (202608010002). It runs on real
// deployed data — this build wrote 'cancelled' right up until it landed, and
// 'pending_approval' until approval became a derived view — so the point of the
// test is that an existing row survives it in the right shape, not that UPDATE
// works.
//
// Staged by migrating UP TO the previous version, writing rows the way the old
// build did, then finishing the migration. That ordering is the whole test: a
// row inserted after the migration proves nothing about a row that predates it.
func TestMigrate_RetiresDeadConversationStatuses(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// Stop one version short of the retirement.
	upToErr := goose.UpTo(database, dir, 202608010001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	// A conversation carrying each retired terminal, written the way the old
	// build wrote them: a completed_at stamp and no parked_at. origin is not
	// 'blueprint' so the row needs no task/prompt/blueprint parents — this
	// test is about the status column, not the graph around it.
	seed := func(id, status string) {
		t.Helper()
		if _, err := database.Exec(`
			INSERT INTO conversations (id, type, origin, trigger_type, creator_user_id, visibility, team_id,
			                           status, completed_at, started_at)
			VALUES (?, 'delegation', 'interactive', 'event', NULL, 'team',
			        '00000000-0000-0000-0000-000000000010', ?,
			        '2026-01-02 03:04:05', '2026-01-01 00:00:00')
		`, id, status); err != nil {
			t.Fatalf("seed %s row: %v", status, err)
		}
	}
	seed("legacy-cancelled", "cancelled")
	seed("legacy-pending-approval", "pending_approval")
	seed("legacy-unsolvable", "task_unsolvable")

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var status string
	var parkedAt, completedAt sql.NullString
	if err := database.QueryRow(
		`SELECT status, parked_at, completed_at FROM conversations WHERE id = 'legacy-cancelled'`,
	).Scan(&status, &parkedAt, &completedAt); err != nil {
		t.Fatalf("read migrated cancelled row: %v", err)
	}
	if status != "open" {
		t.Errorf("cancelled row migrated to %q, want open — a stopped run parks, and its blueprint carries the cancellation", status)
	}
	// Backfilled from the old terminal stamp, not left NULL and not stamped
	// now: the retention sweep has to read the row's true age rather than
	// treat a year-old cancellation as freshly parked.
	if !parkedAt.Valid || parkedAt.String != completedAt.String {
		t.Errorf("parked_at = %v, want the old completed_at stamp (%v)", parkedAt, completedAt)
	}

	// A row that was parked awaiting approval lands `open` with the same
	// backfilled age. `open` rather than a terminal because the artifact it
	// queued still exists and still drives the approval column — and because
	// `open` is the one parked state the snapshot-retention sweep enumerates,
	// so the workspace blob these rows were pinning can finally age out.
	if err := database.QueryRow(
		`SELECT status, parked_at, completed_at FROM conversations WHERE id = 'legacy-pending-approval'`,
	).Scan(&status, &parkedAt, &completedAt); err != nil {
		t.Fatalf("read migrated pending_approval row: %v", err)
	}
	if status != "open" {
		t.Errorf("pending_approval row migrated to %q, want open", status)
	}
	if !parkedAt.Valid || parkedAt.String != completedAt.String {
		t.Errorf("parked_at = %v, want the old completed_at stamp (%v)", parkedAt, completedAt)
	}

	if err := database.QueryRow(
		`SELECT status FROM conversations WHERE id = 'legacy-unsolvable'`,
	).Scan(&status); err != nil {
		t.Fatalf("read migrated task_unsolvable row: %v", err)
	}
	if status != "failed" {
		t.Errorf("task_unsolvable row migrated to %q, want failed", status)
	}

	// The vocabulary is closed afterwards: nothing carries a retired terminal,
	// which is what lets every guard read one set of names instead of two.
	var leftovers int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE status IN ('cancelled', 'pending_approval', 'task_unsolvable')`,
	).Scan(&leftovers); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d conversations still carry a retired status after migration", leftovers)
	}
}
