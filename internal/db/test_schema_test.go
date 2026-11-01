package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/pressly/goose/v3"
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

	// 6. The tenantless image is the same Migrate with no exception for
	//    the tenant tables. It is built from the version cache's image at
	//    head, so this also pins that image against a replay from empty.
	tenantless := openMem(t)
	if err := BootstrapTenantlessSchemaForTest(tenantless); err != nil {
		t.Fatalf("BootstrapTenantlessSchemaForTest: %v", err)
	}
	assertSameMigratedDatabase(t, "tenantless image", tenantless, real)
	if got, want := dumpEventsCatalog(t, tenantless), dumpEventsCatalog(t, real); !reflect.DeepEqual(got, want) {
		t.Errorf("tenantless image: events_catalog content differs.\nrestored: %v\nreal:     %v", got, want)
	}
}

// A file in the shared image cache that is not this tree's head image is
// treated as absent, so a bad file costs a rebuild rather than failing every
// SQLite-backed test that restores it.
func TestReadHeadImage_TakesOnlyTheHeadImage(t *testing.T) {
	head, err := sqliteHeadVersion()
	if err != nil {
		t.Fatalf("head version: %v", err)
	}
	image, err := sharedHeadImage(head)
	if err != nil {
		t.Fatalf("head image: %v", err)
	}
	write := func(body []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "head.img")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}

	if _, ok := readHeadImage(write(image), head); !ok {
		t.Error("the head image was rejected")
	}
	if _, ok := readHeadImage(write(image), head+1); ok {
		t.Error("an image at another version was taken as head")
	}
	if _, ok := readHeadImage(write([]byte("not a database")), head); ok {
		t.Error("a file that is not a database was taken as head")
	}
	if _, ok := readHeadImage(filepath.Join(t.TempDir(), "missing.img"), head); ok {
		t.Error("a missing file was taken as head")
	}

	// A damaged table page that leaves goose_db_version readable at head.
	damaged := append([]byte(nil), image...)
	template, err := openTemplate(image)
	if err != nil {
		t.Fatalf("open template: %v", err)
	}
	var pageSize, root int
	if err := template.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("page size: %v", err)
	}
	if err := template.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type = 'table' AND name = 'entities'`).Scan(&root); err != nil {
		t.Fatalf("entities root page: %v", err)
	}
	_ = template.Close()
	for i := (root - 1) * pageSize; i < root*pageSize; i++ {
		damaged[i] = 0xFF
	}
	probe, err := openTemplate(damaged)
	if err != nil {
		t.Fatalf("open damaged template: %v", err)
	}
	var version int64
	if err := probe.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != head {
		t.Fatalf("the damaged image no longer reads at head (%d, %v), so it checks nothing a version read would miss", version, err)
	}
	_ = probe.Close()
	if _, ok := readHeadImage(write(damaged), head); ok {
		t.Error("an image with a damaged table page was taken as head")
	}
}

// The cache key must change whenever the code of a module that writes the
// image can, so a go.mod that replaces one with a directory, in any spelling
// go.mod accepts, turns the cache off.
func TestHeadImageCachePins(t *testing.T) {
	const requires = "module m\n\ngo 1.26\n\nrequire (\n\tgithub.com/pressly/goose/v3 v3.27.1\n\tmodernc.org/libc v1.72.1\n\tmodernc.org/sqlite v1.50.0\n)\n"
	base, ok := headImageCachePins([]byte(requires))
	if !ok {
		t.Fatal("refused a go.mod that requires every module")
	}
	for _, tc := range []struct {
		name, replace string
		ok            bool
	}{
		{"relative directory", `replace modernc.org/sqlite => ../sqlite`, false},
		{"absolute directory", `replace modernc.org/sqlite => /src/sqlite`, false},
		{"quoted directory", `replace modernc.org/sqlite => "../sqlite"`, false},
		{"quoted module path and directory", `replace "modernc.org/sqlite" => "../sqlite"`, false},
		{"one version to a directory", `replace modernc.org/sqlite v1.50.0 => ../sqlite`, false},
		{"another module", `replace modernc.org/sqlite => example.com/fork/sqlite v1.50.1`, true},
	} {
		pins, ok := headImageCachePins([]byte(requires + tc.replace + "\n"))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
		}
		if ok && pins == base {
			t.Errorf("%s: the replacement left the key unchanged", tc.name)
		}
	}
	if _, ok := headImageCachePins([]byte("module m\n\nrequire modernc.org/sqlite v1.50.0\n")); ok {
		t.Error("took a go.mod that does not require every module")
	}
}

// TestMigratedImageAt_MatchesUpTo pins the images the migration tests
// restore in place of replaying the chain: at each version, a restored
// image must be the database goose.UpTo leaves on an empty one opened
// with the same DSN — same schema catalog, tables, row counts, applied
// versions, and foreign-key enforcement.
//
// The versions are ascending, so run on its own the second image is
// built from the first, through the table rebuilds that toggle
// foreign-key enforcement inside their own migration. The first is
// staged with enforcement off, as the tests that seed at it are, while
// every image is built with it on.
func TestMigratedImageAt_MatchesUpTo(t *testing.T) {
	for _, tc := range []struct {
		dsn     string
		version int64
	}{
		{TestDSNMemoryNoForeignKeys, 202607200002},
		{TestDSNMemory, 202609130001},
	} {
		label := fmt.Sprintf("image at %d", tc.version)
		restored := openMigrationsTestDBAt(t, tc.dsn, tc.version)

		replayed, err := sql.Open("sqlite", tc.dsn)
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		replayed.SetMaxOpenConns(1)
		replayed.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = replayed.Close() })
		gooseMu.Lock()
		goose.SetBaseFS(migrationsSQLiteFS)
		upErr := goose.SetDialect("sqlite3")
		if upErr == nil {
			upErr = goose.UpTo(replayed, "migrations-sqlite", tc.version)
		}
		gooseMu.Unlock()
		if upErr != nil {
			t.Fatalf("goose.UpTo(%d) from empty: %v", tc.version, upErr)
		}

		assertSameMigratedDatabase(t, label, restored, replayed)
		if got, want := foreignKeysEnabled(t, restored), foreignKeysEnabled(t, replayed); got != want {
			t.Errorf("%s: foreign_keys = %v on the restored database, %v on the replayed one", label, got, want)
		}
	}
}

// assertSameMigratedDatabase compares what a migration leaves behind:
// the schema catalog in creation order, the table set, every table's row
// count, and the applied goose versions.
func assertSameMigratedDatabase(t *testing.T, label string, got, want *sql.DB) {
	t.Helper()
	if g, w := dumpSchema(t, got), dumpSchema(t, want); !reflect.DeepEqual(g, w) {
		t.Errorf("%s: sqlite_master differs.\nrestored:\n%s\nreplayed:\n%s", label, joinLines(g), joinLines(w))
	}
	tablesGot, tablesWant := listUserTables(t, got), listUserTables(t, want)
	if !reflect.DeepEqual(tablesGot, tablesWant) {
		t.Fatalf("%s: table set differs.\nrestored: %v\nreplayed: %v", label, tablesGot, tablesWant)
	}
	for _, table := range tablesWant {
		if g, w := countRows(t, got, table), countRows(t, want, table); g != w {
			t.Errorf("%s: table %s has %d row(s) restored, %d replayed", label, table, g, w)
		}
	}
	if g, w := dumpMigrationVersions(t, got), dumpMigrationVersions(t, want); !reflect.DeepEqual(g, w) {
		t.Errorf("%s: goose_db_version versions differ.\nrestored: %v\nreplayed: %v", label, g, w)
	}
}

func foreignKeysEnabled(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var on bool
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	return on
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
