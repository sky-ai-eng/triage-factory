package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// openMigrationsTestDB returns a fresh, schema-less in-memory SQLite
// for migration tests. Distinct from newTenantlessTestDB (which restores an
// already-migrated image) — these tests exercise Migrate itself.
func openMigrationsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

// openMigrationsTestDBAt returns a private in-memory SQLite opened with
// dsn and holding the schema goose.UpTo(version) leaves on an empty
// database, restored from a cached image (migratedImageAt) rather than
// replayed. It is for the tests that seed rows the way an older build
// wrote them and then migrate forward: the replay up to the seeding
// version is the expensive part, and none of them is about it.
//
// Such a test migrates through the migration it is about and stops
// (migrateThrough), never to head: a replay to head makes every such test
// slower with each migration that lands after it. And it is deleted once
// its migration ships in a release. A released migration's text is what
// every install that upgrades through it runs, so its test can only ever
// re-check SQL nothing may change.
//
// The foreign-key pragma is the connection's, from dsn. On return goose
// is pointed at the SQLite tree with the sqlite3 dialect, the state the
// goose.UpTo this replaces left behind, so a test's own goose.Up or
// goose.UpTo afterwards runs as it did.
func openMigrationsTestDBAt(t *testing.T, dsn string, version int64) *sql.DB {
	t.Helper()
	image, err := migratedImageAt(version)
	if err != nil {
		t.Fatalf("migrated image at %d: %v", version, err)
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := restoreImage(database, image); err != nil {
		t.Fatalf("restore image at %d: %v", version, err)
	}

	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	return database
}

// migrateThrough applies the SQLite migrations after database's current
// version up to and including version.
func migrateThrough(t *testing.T, database *sql.DB, version int64) {
	t.Helper()
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(database, "migrations-sqlite", version); err != nil {
		t.Fatalf("goose.UpTo(%d): %v", version, err)
	}
}

// TestMigrate_BricksPreV1110_GooseStampedAtOldBaseline covers the
// most-likely upgrade attempt: a user on the earlier baseline (or any
// pre-v1.11.0 goose-tracked install) tries to upgrade. The
// goose_db_version table exists but doesn't contain the v1.11.0
// baseline version, so the brick check fires.
func TestMigrate_BricksPreV1110_GooseStampedAtOldBaseline(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`
		CREATE TABLE goose_db_version (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			version_id INTEGER NOT NULL,
			is_applied INTEGER NOT NULL,
			tstamp TIMESTAMP DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("create goose_db_version: %v", err)
	}
	// Stamp the pre-v1.11.0 baseline (202605090001) and one
	// post-baseline migration — simulating a typical upgrade path.
	for _, v := range []int64{0, 202605090001, 202605120003} {
		if _, err := database.Exec(
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, v,
		); err != nil {
			t.Fatalf("stamp version %d: %v", v, err)
		}
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
	if !strings.Contains(err.Error(), "wipe") && !strings.Contains(err.Error(), "Wipe") {
		t.Errorf("error message should mention the wipe remediation, got: %v", err)
	}
}

// TestMigrate_BricksPreV1110_LegacySchemaMigrations covers the
// pre-goose runner case: the legacy `schema_migrations` table exists
// (left in place as audit trail by every prior install) plus the
// `entities` sentinel, but no goose tracker. Pre-v1.11.0.
func TestMigrate_BricksPreV1110_LegacySchemaMigrations(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (version TEXT PRIMARY KEY);
		INSERT INTO schema_migrations VALUES ('20260501_001_baseline');
		CREATE TABLE entities (id TEXT PRIMARY KEY);
	`); err != nil {
		t.Fatalf("stage legacy state: %v", err)
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
}

// TestMigrate_BricksPreV1110_PreRunner covers the oldest case: app
// tables present, no version metadata at all. This shape predates
// even the legacy schema_migrations runner.
func TestMigrate_BricksPreV1110_PreRunner(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`CREATE TABLE entities (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create entities: %v", err)
	}

	err := Migrate(database, "sqlite3")
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("Migrate err = %v, want ErrPreV1110Install", err)
	}
}

// TestMigrationStatus_BricksPreV1110 covers the operator command
// surface: `triagefactory migrate status` against a pre-v1.11.0 DB
// should refuse cleanly through the same brick path Migrate uses,
// not emit a misleading "all pending" listing.
func TestMigrationStatus_BricksPreV1110(t *testing.T) {
	database := openMigrationsTestDB(t)

	if _, err := database.Exec(`CREATE TABLE entities (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create entities: %v", err)
	}

	var buf strings.Builder
	err := MigrationStatus(database, "sqlite3", &buf)
	if !errors.Is(err, ErrPreV1110Install) {
		t.Fatalf("MigrationStatus err = %v, want ErrPreV1110Install", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output on brick path, got: %q", buf.String())
	}
}

// TestMigrate_MessagesAreTheOnlyAccountingLedger pins the accounting shape a
// fresh migration produces: messages carries the token columns AND cost_usd
// (the money/token ledger); conversations and claims carry NO dollar or token
// cache (claims keep only the duration/turns telemetry). A SELECT of the
// columns errors if any is missing, so the LIMIT 0 probe is a
// column-existence assertion without needing rows.
func TestMigrate_MessagesAreTheOnlyAccountingLedger(t *testing.T) {
	database := newTenantlessTestDB(t)
	if _, err := database.Exec(
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cost_usd FROM messages LIMIT 0`,
	); err != nil {
		t.Errorf("messages missing a ledger column after migrate: %v", err)
	}
	if _, err := database.Exec(`SELECT duration_ms, num_turns FROM claims LIMIT 0`); err != nil {
		t.Errorf("claims missing a telemetry column after migrate: %v", err)
	}
	for _, probe := range []struct{ table, col string }{
		{"conversations", "total_cost_usd"},
		{"conversations", "duration_ms"},
		{"conversations", "num_turns"},
		{"conversations", "input_tokens"},
		{"claims", "cost_usd"},
		{"claims", "input_tokens"},
	} {
		if _, err := database.Exec(`SELECT ` + probe.col + ` FROM ` + probe.table + ` LIMIT 0`); err == nil {
			t.Errorf("%s.%s survived the migration; the accounting cache must be gone", probe.table, probe.col)
		}
	}
}
