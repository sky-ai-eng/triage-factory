package db

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestSeedEventTypes_FreshInstall pins that a fresh Migrate leaves
// events_catalog with exactly one row per domain.AllEventTypes() entry.
func TestSeedEventTypes_FreshInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("count events_catalog: %v", err)
	}
	want := len(domain.AllEventTypes())
	if n != want {
		t.Errorf("events_catalog rowcount = %d, want %d (len of domain.AllEventTypes)", n, want)
	}
}

// TestSeedEventTypes_Idempotent pins that repeated Migrate/SeedEventTypes
// calls don't error or change the row count — the ON CONFLICT path
// doesn't collide with rows the baseline (or a forward migration)
// already inserted.
func TestSeedEventTypes_Idempotent(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("direct SeedEventTypes: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("count events_catalog: %v", err)
	}
	want := len(domain.AllEventTypes())
	if n != want {
		t.Errorf("events_catalog rowcount = %d, want %d after repeated seeding", n, want)
	}
}

// TestSeedEventTypes_OverwritesDrift pins UPSERT semantics: a row whose
// label was hand-mutated gets overwritten back to what
// domain.AllEventTypes() currently declares — proving this isn't the
// INSERT OR IGNORE-only semantics the baseline uses.
func TestSeedEventTypes_OverwritesDrift(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const id = "github:pr:opened"
	if _, err := database.Exec(
		`UPDATE events_catalog SET label = 'DRIFTED LABEL' WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("mutate label: %v", err)
	}

	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("SeedEventTypes: %v", err)
	}

	var label string
	if err := database.QueryRow(`SELECT label FROM events_catalog WHERE id = ?`, id).Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "PR Opened" {
		t.Errorf("label = %q after SeedEventTypes, want 'PR Opened' (drift not overwritten)", label)
	}
}

// TestSeedEventTypes_NeverDeletes pins the "no deletes" invariant: a
// synthetic row with an id not in domain.AllEventTypes() (simulating a
// retired event type) survives SeedEventTypes untouched.
func TestSeedEventTypes_NeverDeletes(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const syntheticID = "test:synthetic:not_in_go_source"
	if _, err := database.Exec(
		`INSERT INTO events_catalog (id, source, category, label, description) VALUES (?, 'test', 'synthetic', 'Synthetic', 'not in domain.AllEventTypes()')`,
		syntheticID,
	); err != nil {
		t.Fatalf("seed synthetic row: %v", err)
	}

	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("SeedEventTypes: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events_catalog WHERE id = ?`, syntheticID).Scan(&n); err != nil {
		t.Fatalf("count synthetic row: %v", err)
	}
	if n != 1 {
		t.Errorf("synthetic row count = %d after SeedEventTypes, want 1 (must never delete)", n)
	}
}

// TestSeedEventTypes_UnchangedRowsSkipWrite pins the WHERE guard on the
// DO UPDATE: reseeding with values that already match must not touch any
// row. An AFTER UPDATE trigger makes "was a write attempted" directly
// observable — row content alone can't distinguish a no-op UPDATE from
// one that wrote the same values back.
func TestSeedEventTypes_UnchangedRowsSkipWrite(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := database.Exec(`
		CREATE TABLE test_update_counter (n INTEGER NOT NULL);
		INSERT INTO test_update_counter (n) VALUES (0);
		CREATE TRIGGER test_count_ec_updates AFTER UPDATE ON events_catalog
		BEGIN
			UPDATE test_update_counter SET n = n + 1;
		END;
	`); err != nil {
		t.Fatalf("install update counter: %v", err)
	}

	// Reseeding with unchanged values must not fire the trigger at all —
	// the WHERE guard should make every conflicting row a no-op.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("SeedEventTypes (unchanged): %v", err)
	}
	assertUpdateCounter(t, database, 0)

	// Mutating exactly one row and reseeding must fire the trigger exactly
	// once — proves the guard isn't accidentally suppressing real changes.
	if _, err := database.Exec(
		`UPDATE events_catalog SET label = 'DRIFTED LABEL' WHERE id = 'github:pr:opened'`,
	); err != nil {
		t.Fatalf("mutate label: %v", err)
	}
	// The mutation itself fires the trigger once; reset before the real
	// assertion so the counter isolates SeedEventTypes's own writes.
	if _, err := database.Exec(`UPDATE test_update_counter SET n = 0`); err != nil {
		t.Fatalf("reset counter: %v", err)
	}

	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("SeedEventTypes (one drifted row): %v", err)
	}
	assertUpdateCounter(t, database, 1)
}

func assertUpdateCounter(t *testing.T, database *sql.DB, want int) {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT n FROM test_update_counter`).Scan(&n); err != nil {
		t.Fatalf("read update counter: %v", err)
	}
	if n != want {
		t.Errorf("events_catalog UPDATE trigger fired %d times, want %d", n, want)
	}
}
