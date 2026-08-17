package db

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

// TestBootstrapSchemaForTest_MatchesMigrate pins the cached bootstrap
// path to the real Migrate path. Nearly every store and handler test in
// the repo starts from the cached path, so if the restored page image
// ever stops reproducing a freshly migrated database — a schema object
// that doesn't survive the round trip, a migration whose seeded rows
// land somewhere the template never captured — those tests all start
// from the wrong state and say nothing about it. This test fails loudly
// instead, comparing the schema catalog and row-counting every user
// table on both paths.
func TestBootstrapSchemaForTest_MatchesMigrate(t *testing.T) {
	cached := openMem(t)
	if err := BootstrapSchemaForTest(cached); err != nil {
		t.Fatalf("BootstrapSchemaForTest: %v", err)
	}

	real := openMem(t)
	if err := Migrate(real, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// 1. sqlite_master must be identical: same DDL for every table,
	//    index, trigger, view, and in the same rowid order.
	if got, want := dumpSchema(t, cached), dumpSchema(t, real); !reflect.DeepEqual(got, want) {
		t.Errorf("sqlite_master differs between cached and real bootstrap.\ncached:\n%s\nreal:\n%s",
			joinLines(got), joinLines(want))
	}

	// 2. Same set of tables on both sides.
	tablesCached := listUserTables(t, cached)
	tablesReal := listUserTables(t, real)
	if !reflect.DeepEqual(tablesCached, tablesReal) {
		t.Fatalf("table set differs.\ncached: %v\nreal:   %v", tablesCached, tablesReal)
	}

	// 3. Row count per table must match — EXCEPT for the synthetic local
	//    tenant tables, which BootstrapSchemaForTest deliberately seeds as
	//    a fixture (production no longer provisions at boot or in the
	//    migration; provisioning is the explicit BootstrapLocalOrg action).
	//    So the real (Migrate-only) path has zero tenant rows while the
	//    cached fixture has exactly one of each. We assert that expected
	//    divergence explicitly here rather than ignoring it, and keep the
	//    strict equality guard for every other table — a future migration
	//    that starts seeding some OTHER table still trips this.
	fixtureSeededTenantTables := map[string]bool{
		"orgs": true, "teams": true, "users": true,
		"org_memberships": true, "memberships": true,
		"org_settings": true, "team_settings": true,
	}
	for _, table := range tablesReal {
		gotCount := countRows(t, cached, table)
		wantCount := countRows(t, real, table)
		if fixtureSeededTenantTables[table] {
			if gotCount != 1 {
				t.Errorf("fixture tenant table %s: cached bootstrap has %d row(s); want exactly 1 (SeedLocalTenantRows)", table, gotCount)
			}
			if wantCount != 0 {
				t.Errorf("fixture tenant table %s: real Migrate has %d row(s); want 0 (boot/migration provisions nothing)", table, wantCount)
			}
			continue
		}
		if gotCount != wantCount {
			t.Errorf("table %s: cached bootstrap has %d row(s), real has %d. "+
				"The cached path restores a page image of a migrated template, "+
				"so a divergence here means the image no longer round-trips "+
				"what Migrate produces — see buildSchemaImage in test_schema.go.",
				table, gotCount, wantCount)
		}
	}

	// 4. events_catalog content (the FK target most tests depend on)
	//    must match row-for-row.
	if got, want := dumpEventsCatalog(t, cached), dumpEventsCatalog(t, real); !reflect.DeepEqual(got, want) {
		t.Errorf("events_catalog content differs.\ncached: %v\nreal:   %v", got, want)
	}

	// 5. goose_db_version contents must match (tstamp is wall-clock
	//    so we ignore it, but the set of recorded version_ids must be
	//    identical — head check on both sides).
	if got, want := dumpMigrationVersions(t, cached), dumpMigrationVersions(t, real); !reflect.DeepEqual(got, want) {
		t.Errorf("goose_db_version versions differ.\ncached: %v\nreal:   %v", got, want)
	}
}

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// dumpSchema returns the sql column of sqlite_master in rowid order,
// excluding sqlite-internal entries. Byte-equality across the two paths
// proves the restored image carries an identical schema catalog, down
// to the order the objects were created in.
func dumpSchema(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY rowid
	`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func listUserTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func dumpEventsCatalog(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT id, source, category, label, COALESCE(description, '')
		FROM events_catalog ORDER BY id
	`)
	if err != nil {
		t.Fatalf("dump events_catalog: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, source, category, label, description string
		if err := rows.Scan(&id, &source, &category, &label, &description); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s", id, source, category, label, description))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func dumpMigrationVersions(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT version_id, is_applied FROM goose_db_version ORDER BY version_id`)
	if err != nil {
		t.Fatalf("dump goose_db_version: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			version   int64
			isApplied int
		)
		if err := rows.Scan(&version, &isApplied); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%d:%d", version, isApplied))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func joinLines(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "\n---\n"
		}
		out += x
	}
	return out
}
