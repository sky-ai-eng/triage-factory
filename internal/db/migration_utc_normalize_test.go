package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that rewrites every stored timestamp to offset-free UTC text
// (202608170001). It exists because _time_format=sqlite serializes a bound
// time.Time with the writer's own UTC offset attached, so a database written
// by a build that bound a bare time.Now() on a machine east or west of UTC
// holds values that read as the wrong instant to SQLite's own date functions
// and sort wrongly against the CURRENT_TIMESTAMP rows beside them.
//
// Two properties are load-bearing and both are pinned below: the rewrite lands
// every on-disk shape on the same instant, and running it twice changes
// nothing.

const (
	utcNormalizeVersion     int64 = 202608170001
	utcNormalizePrevVersion int64 = 202608160006
	utcNormalizeFile              = "migrations-sqlite/202608170001_utc_normalize_timestamps.sql"
)

// utcNormalizeTargets extracts the (table, column) pairs the migration
// actually rewrites, straight from the shipped SQL.
var utcNormalizeStatement = regexp.MustCompile(`UPDATE OR IGNORE (\w+) SET (\w+) =`)

func utcNormalizeTargets(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := fs.ReadFile(migrationsSQLiteFS, utcNormalizeFile)
	if err != nil {
		t.Fatalf("read %s: %v", utcNormalizeFile, err)
	}
	out := map[string]bool{}
	for _, m := range utcNormalizeStatement.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]+"."+m[2]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s rewrites nothing — the statement shape the test greps for has changed", utcNormalizeFile)
	}
	return out
}

// migrateUpTo brings a fresh test DB to one specific goose version.
func migrateUpTo(t *testing.T, database *sql.DB, version int64) {
	t.Helper()
	gooseMu.Lock()
	defer gooseMu.Unlock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(database, dir, version); err != nil {
		t.Fatalf("goose.UpTo(%d): %v", version, err)
	}
}

// timestampColumns is the schema's own answer to "what is a timestamp here",
// which is where the migration's column list came from in the first place.
func timestampColumns(t *testing.T, database *sql.DB) []string {
	t.Helper()
	rows, err := database.Query(`
		SELECT m.name || '.' || p.name
		FROM sqlite_master m
		JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table'
		  AND m.name NOT LIKE 'sqlite_%'
		  AND m.name <> 'goose_db_version'
		  AND UPPER(p.type) IN ('DATETIME', 'TIMESTAMP')
		ORDER BY m.name, p.cid
	`)
	if err != nil {
		t.Fatalf("enumerate timestamp columns: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out = append(out, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return out
}

// TestMigrate_UTCNormalizeCoversEveryTimestampColumn is what "derive the
// column list from the schema, not by hand" means once the migration is
// written: the list in the file has to equal what pragma_table_info reports
// for the schema as it stood at that version. A column missed by hand is a
// column that keeps its mixed offsets forever, since the migration runs once.
//
// It deliberately measures the schema at the version BEFORE the normalization
// rather than at head. A timestamp column added later can only ever have been
// written by post-fix code, so it needs no normalizing — and pinning to head
// would turn every future column into a spurious failure here.
func TestMigrate_UTCNormalizeCoversEveryTimestampColumn(t *testing.T) {
	database := openMigrationsTestDB(t)
	migrateUpTo(t, database, utcNormalizePrevVersion)

	want := timestampColumns(t, database)
	got := utcNormalizeTargets(t)

	var missing []string
	for _, col := range want {
		if !got[col] {
			missing = append(missing, col)
		}
		delete(got, col)
	}
	if len(missing) > 0 {
		t.Errorf("migration does not normalize %d timestamp column(s): %s",
			len(missing), strings.Join(missing, ", "))
	}
	if len(got) > 0 {
		var extra []string
		for col := range got {
			extra = append(extra, col)
		}
		sort.Strings(extra)
		t.Errorf("migration normalizes %s, which the schema at version %d has no timestamp column for",
			strings.Join(extra, ", "), utcNormalizePrevVersion)
	}
}

// utcNormalizeShapes is the on-disk timestamp vocabulary, one row per shape a
// deployed database can hold, each paired with the instant it denotes. The two
// New York values straddle a DST boundary so a fix that hardcodes one offset
// fails on the other; the Kolkata value is +05:30, so a fix that assumes whole
// hours fails on it.
var utcNormalizeShapes = []struct {
	name   string
	stored string
	want   string // offset-free UTC, millisecond precision
}{
	{"driver-bound, summer (-04:00)", "2026-08-16 20:02:59.123456789-04:00", "2026-08-17 00:02:59.123"},
	{"driver-bound, winter (-05:00)", "2026-01-16 20:02:59.123456789-05:00", "2026-01-17 01:02:59.123"},
	{"driver-bound, half-hour (+05:30)", "2026-08-16 20:02:59.123456789+05:30", "2026-08-16 14:32:59.123"},
	{"driver-bound, already UTC", "2026-08-17 00:02:59.123456789+00:00", "2026-08-17 00:02:59.123"},
	{"CURRENT_TIMESTAMP default", "2026-08-17 00:02:59", "2026-08-17 00:02:59.000"},
	{"ISO-8601", "2026-08-17T00:02:59Z", "2026-08-17 00:02:59.000"},
	{"already normalized", "2026-08-17 00:02:59.123", "2026-08-17 00:02:59.123"},
	// The legacy Go time.String() rendering, which strftime cannot parse.
	// _time_format=sqlite stopped it being written months before the v1.11.0
	// baseline the brick check demands, so no live database holds one — but
	// the guard that leaves it alone rather than nulling it is the difference
	// between an unreachable case and silent data loss, so it is pinned.
	{"legacy Go String(), unparseable", "2026-08-16 20:02:59 -0400 EDT", "2026-08-16 20:02:59 -0400 EDT"},
}

func TestMigrate_UTCNormalizeRewritesEveryShapeAndIsIdempotent(t *testing.T) {
	database := openMigrationsTestDB(t)
	migrateUpTo(t, database, utcNormalizePrevVersion)

	const orgID = "00000000-0000-0000-0000-000000000001"
	if _, err := database.Exec(
		`INSERT INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, orgID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// Written as literal text, not as a bound time.Time: the point is to stage
	// the bytes a past build left on disk, which today's driver would not
	// produce.
	for i, s := range utcNormalizeShapes {
		if _, err := database.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, created_at, last_polled_at, closed_at)
			VALUES (?, ?, 'github', ?, 'pull_request', ?, ?, ?)
		`, fmt.Sprintf("e%02d", i), orgID, fmt.Sprintf("sky/repo#%d", i), s.stored, s.stored, s.stored); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}

	readBack := func() map[string][3]string {
		t.Helper()
		// The || '' defeats the driver's DATETIME-column conversion, which
		// would otherwise re-render the value on the way out and hide what is
		// actually stored.
		rows, err := database.Query(
			`SELECT id, created_at || '', last_polled_at || '', closed_at || '' FROM entities ORDER BY id`)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		defer rows.Close()
		out := map[string][3]string{}
		for rows.Next() {
			var id, created, polled, closed string
			if err := rows.Scan(&id, &created, &polled, &closed); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = [3]string{created, polled, closed}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		return out
	}

	// The real path: goose applies the migration like any deployment would.
	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	upErr := goose.UpTo(database, dir, utcNormalizeVersion)
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo(normalization): %v", upErr)
	}

	after := readBack()
	for i, s := range utcNormalizeShapes {
		id := fmt.Sprintf("e%02d", i)
		for col, got := range after[id] {
			if got != s.want {
				t.Errorf("%s (%s, column %d): stored %q, want %q",
					s.name, s.stored, col, got, s.want)
			}
		}
	}

	// Every normalized value must denote the instant the original did — the
	// assertion the string comparison above cannot make on its own, since it
	// would pass just as happily on a consistently wrong offset.
	for _, s := range utcNormalizeShapes {
		if s.want == s.stored {
			continue // untouched by construction (already normal, or unparseable)
		}
		var same bool
		if err := database.QueryRow(
			`SELECT datetime(?) IS datetime(?)`, s.stored, s.want,
		).Scan(&same); err != nil {
			t.Fatalf("compare instants for %s: %v", s.name, err)
		}
		if !same {
			t.Errorf("%s: %q and %q are not the same instant", s.name, s.stored, s.want)
		}
	}

	// Idempotence: replaying the migration's own body leaves every byte alone.
	// It matters beyond re-running goose (which would not) because the guard
	// conditions are what stop a second pass re-interpreting an already-UTC
	// value as local.
	before := readBack()
	if _, err := database.Exec(utcNormalizeUpBody(t)); err != nil {
		t.Fatalf("replay migration body: %v", err)
	}
	if replayed := readBack(); fmt.Sprint(replayed) != fmt.Sprint(before) {
		t.Errorf("replaying the migration changed rows:\nbefore = %v\nafter  = %v", before, replayed)
	}
}

// utcNormalizeUpBody is the migration's Up block, ready to Exec. Goose owns the
// markers; everything between them is plain SQL.
func utcNormalizeUpBody(t *testing.T) string {
	t.Helper()
	raw, err := fs.ReadFile(migrationsSQLiteFS, utcNormalizeFile)
	if err != nil {
		t.Fatalf("read %s: %v", utcNormalizeFile, err)
	}
	_, rest, ok := strings.Cut(string(raw), "-- +goose Up")
	if !ok {
		t.Fatalf("%s has no goose Up marker", utcNormalizeFile)
	}
	body, _, ok := strings.Cut(rest, "-- +goose Down")
	if !ok {
		t.Fatalf("%s has no goose Down marker", utcNormalizeFile)
	}
	return body
}
