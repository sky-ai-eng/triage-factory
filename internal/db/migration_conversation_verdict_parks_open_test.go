package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that takes 'completed' out of conversations.status
// (202610060001). A verdict parks the row `open`, and completed_at is what
// makes an `open` row concluded, so the stamp has to survive the rewrite — and
// a row the old terminal write left without one has to gain one, or it lands
// unsettled: live to the router and claimable on undelivered input.
//
// Staged by migrating UP TO the previous version and writing rows the way the
// old build did, for the same reason as TestMigrate_RetiresDeadConversationStatuses.
func TestMigrate_ConversationVerdictParksOpen(t *testing.T) {
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
	upToErr := goose.UpTo(database, dir, 202610050001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	seed := func(id string, completedAt, endedAt, parkReason any) {
		t.Helper()
		if _, err := database.Exec(`
			INSERT INTO conversations (id, type, origin, trigger_type, creator_user_id, visibility, team_id,
			                           status, outcome, completed_at, ended_at, ended_reason, park_reason, started_at)
			VALUES (?, 'delegation', 'interactive', 'event', NULL, 'team',
			        '00000000-0000-0000-0000-000000000010', 'completed', 'finish',
			        ?, ?, CASE WHEN ? IS NULL THEN NULL ELSE 'requeued' END, ?, '2026-01-01 00:00:00')
		`, id, completedAt, endedAt, endedAt, parkReason); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("stamped", "2026-01-02 03:04:05", nil, "user_cancelled")
	seed("unstamped-ended", nil, "2026-01-03 00:00:00", nil)
	seed("unstamped", nil, nil, nil)

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	read := func(id string) (status, outcome string, completedAt, parkReason sql.NullString) {
		t.Helper()
		if err := database.QueryRow(
			`SELECT status, outcome, completed_at, park_reason FROM conversations WHERE id = ?`, id,
		).Scan(&status, &outcome, &completedAt, &parkReason); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return status, outcome, completedAt, parkReason
	}

	status, outcome, completedAt, parkReason := read("stamped")
	if status != "open" || outcome != "finish" {
		t.Errorf("stamped row = (%q, %q), want (open, finish) — the verdict stays on the row", status, outcome)
	}
	if completedAt.String != "2026-01-02T03:04:05Z" {
		t.Errorf("completed_at = %v, want the terminal write's stamp kept", completedAt)
	}
	if parkReason.Valid {
		t.Errorf("park_reason = %q, want cleared — a verdict records no reason", parkReason.String)
	}

	if _, _, completedAt, _ := read("unstamped-ended"); completedAt.String != "2026-01-03T00:00:00Z" {
		t.Errorf("unstamped ended row completed_at = %v, want its ended_at", completedAt)
	}
	if _, _, completedAt, _ := read("unstamped"); completedAt.String != "2026-01-01T00:00:00Z" {
		t.Errorf("unstamped row completed_at = %v, want its started_at", completedAt)
	}

	var leftovers int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE status = 'completed' OR (status = 'open' AND completed_at IS NULL AND outcome IS NOT NULL)`,
	).Scan(&leftovers); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d conversations still read completed, or carry a verdict without a conclusion stamp", leftovers)
	}
}
