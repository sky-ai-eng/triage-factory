package db

import (
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
