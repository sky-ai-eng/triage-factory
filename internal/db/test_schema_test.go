package db

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// imagePinVersion is the version below head at which
// TestMigrate_MatchesTheTestImages pins the image a migration test restores.
// It is past a checkpoint and not one itself, so the image is a checkpoint's
// restored and replayed forward, the way every such image is built.
const imagePinVersion = 202609130001

// TestMigrate_MatchesTheTestImages replays the SQLite tree from empty once,
// and checks along the way everything a replay is needed for: the image a
// migration test restores below head, a fresh install, and the images every
// other SQLite-backed test restores. Nearly every store and handler test in
// the repo starts from one of those images, so if a restored image ever stops
// reproducing what the migrations leave — a schema object that doesn't
// survive the round trip, a migration whose seeded rows land somewhere the
// image never captured — those tests all start from the wrong state and say
// nothing about it. This test fails instead, comparing the schema catalog and
// row-counting every user table on both paths.
func TestMigrate_MatchesTheTestImages(t *testing.T) {
	// The replay starts Migrate from a database goose has already begun, so
	// the brick check's arm for an empty database is checked on its own.
	if err := assertFreshOrCurrent(openMem(t), "sqlite3"); err != nil {
		t.Fatalf("brick check on an empty database: %v", err)
	}

	real := openMem(t)
	migrateThrough(t, real, imagePinVersion)
	restored := openMigrationsTestDBAt(t, imagePinVersion)
	assertSameMigratedDatabase(t, fmt.Sprintf("image at %d", imagePinVersion), restored, real)

	// Migrate takes it the rest of the way. The second Migrate is a launch on
	// an up-to-date install, and the comparisons below see anything it
	// changed.
	for range 2 {
		if err := Migrate(real, "sqlite3"); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}

	var version int64
	if err := real.QueryRow(
		`SELECT version_id FROM goose_db_version WHERE version_id = ?`, v1110BaselineVersionID,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped in goose_db_version: %v", err)
	}
	for _, table := range []string{
		"entities", "events", "tasks", "conversations", "messages", "claims",
		"orgs", "users", "event_handlers",
		"org_settings", "team_settings", "user_settings",
		"jira_project_status_rules", "linear_team_rules",
	} {
		exists, err := tableExists(real, table)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table missing after Migrate", table)
		}
	}
	// A fresh install must not resurrect any dissolved table.
	for _, table := range []string{
		"runs", "run_messages", "curator_requests", "curator_messages",
		"curator_pending_context", "run_pending_input",
		"staged_agent_injections", "run_credentials", "curator_turn_credentials",
		"curator_homes", "projects", "project_pinned_repos",
	} {
		exists, err := tableExists(real, table)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if exists {
			t.Errorf("dissolved table %s present after Migrate", table)
		}
	}

	cached := openMem(t)
	if err := BootstrapSchemaForTest(cached); err != nil {
		t.Fatalf("BootstrapSchemaForTest: %v", err)
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
	//    the tenant tables. It is built from the image cache's image at
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

// A file in the image cache that is not the image it is named for is treated
// as absent, so a bad file costs a rebuild rather than failing every
// SQLite-backed test that restores it.
func TestImageCache_TakesOnlyTheImageItNames(t *testing.T) {
	head, err := sqliteHeadVersion()
	if err != nil {
		t.Fatalf("head version: %v", err)
	}
	image, err := migratedImageAt(head)
	if err != nil {
		t.Fatalf("head image: %v", err)
	}
	below := head - 1
	cache := &imageCache{dir: t.TempDir(), keys: map[int64]string{head: "head", below: "below"}}
	put := func(version int64, body []byte) {
		t.Helper()
		path, _ := cache.path(version)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if _, ok := cache.read(head); ok {
		t.Error("a missing file was taken")
	}
	cache.write(head, image)
	if _, ok := cache.read(head); !ok {
		t.Error("the image at head was rejected")
	}
	put(below, image)
	if _, ok := cache.read(below); ok {
		t.Error("the image at head was taken as the image below it")
	}
	put(head, []byte("not a database"))
	if _, ok := cache.read(head); ok {
		t.Error("a file that is not a database was taken")
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
	put(head, damaged)
	if _, ok := cache.read(head); ok {
		t.Error("an image with a damaged table page was taken")
	}
}

// An image's key covers the migrations up to its version and no others, so a
// new migration leaves every cached image below it in place, and an edit to
// one rebuilds every image above it.
func TestImageCacheKeys_CoverTheMigrationsUpToTheirVersion(t *testing.T) {
	keys := func(bodies ...string) map[int64]string {
		t.Helper()
		treeFS := fstest.MapFS{}
		var tree goose.Migrations
		for i, body := range bodies {
			source := fmt.Sprintf("m/%d_m.sql", i+1)
			treeFS[source] = &fstest.MapFile{Data: []byte(body)}
			tree = append(tree, &goose.Migration{Version: int64(i + 1), Source: source})
		}
		k, ok := imageCacheKeysUnder("module pins\n", treeFS, tree)
		if !ok {
			t.Fatal("no keys for the tree")
		}
		return k
	}
	base := keys("a", "b")
	appended := keys("a", "b", "c")
	edited := keys("a", "B")
	if appended[1] != base[1] || appended[2] != base[2] {
		t.Error("a new migration changed the keys of the images below it")
	}
	if edited[1] != base[1] {
		t.Error("an edit to migration 2 changed the key of the image at 1")
	}
	if edited[2] == base[2] {
		t.Error("an edit to migration 2 left the key of the image at 2 unchanged")
	}
}

// The cache key must change whenever the code of a module that writes the
// image can, so a go.mod that replaces one with a directory, in any spelling
// go.mod accepts, turns the cache off.
func TestImageCachePins(t *testing.T) {
	const requires = "module m\n\ngo 1.26\n\nrequire (\n\tgithub.com/pressly/goose/v3 v3.27.1\n\tmodernc.org/libc v1.72.1\n\tmodernc.org/sqlite v1.50.0\n)\n"
	base, ok := imageCachePins([]byte(requires))
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
		pins, ok := imageCachePins([]byte(requires + tc.replace + "\n"))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
		}
		if ok && pins == base {
			t.Errorf("%s: the replacement left the key unchanged", tc.name)
		}
	}
	if _, ok := imageCachePins([]byte("module m\n\nrequire modernc.org/sqlite v1.50.0\n")); ok {
		t.Error("took a go.mod that does not require every module")
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
