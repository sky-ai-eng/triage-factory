package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/secretenv"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// DB wraps a sql.DB connection for passing to subsystems.
type DB struct {
	Conn *sql.DB
}

// Open returns a connection to the SQLite database at the local state
// root (~/.triagefactory/triagefactory.db). Creates the directory if it
// doesn't exist. Local mode only — multi mode is Postgres-backed.
func Open() (*sql.DB, error) {
	// Surface a missing $HOME as an error (the pre-paths behavior) rather
	// than letting the error-free DBPath panic underneath.
	if _, err := paths.StateRootErr(); err != nil {
		return nil, err
	}
	dbPath := paths.DBPath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}

	return OpenAt(dbPath)
}

// OpenAt returns a connection to the SQLite database at the given path.
// The directory must already exist.
func OpenAt(dbPath string) (*sql.DB, error) {
	// busy_timeout(5000) is the safety net: modernc.org/sqlite returns
	// SQLITE_BUSY immediately on lock contention unless this is set,
	// unlike mattn/go-sqlite3 which had implicit driver-level retries.
	// 5s gives any rare contention plenty of room to resolve before
	// surfacing an error.
	//
	// _time_format=sqlite forces modernc to serialize time.Time bind
	// parameters as "2006-01-02 15:04:05.999999999-07:00" instead of
	// the default Go time.String() form ("2006-01-02 15:04:05 -0700
	// MST [m=+...]"), which is unparseable by SQLite date functions
	// and by anyone reading the column as TEXT (e.g. via COALESCE in
	// factory queries). Direct time.Time scans against legacy rows
	// already in the old format still succeed — modernc's reader is
	// permissive — so no data migration is needed.
	db, err := OpenTraced("sqlite", dbPath+
		"?_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(on)"+
		"&_pragma=busy_timeout(5000)"+
		"&_time_format=sqlite", PoolLocal)
	if err != nil {
		return nil, err
	}

	// A single connection serializes this process's DB work at the
	// Go-pool layer, eliminating in-process races for SQLite's file
	// lock by queueing contention in Go instead. SQLITE_BUSY can still
	// happen from external contention, such as another process holding
	// a write transaction or a long-running read transaction, so the
	// busy_timeout above remains an important backstop. WAL still allows
	// other processes (e.g. `triagefactory exec` invocations) to read
	// concurrently against the same file. SetConnMaxLifetime(0) keeps
	// the one connection alive for the process lifetime so we don't
	// pay reconnect cost on idle gaps.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// OpenForCLI returns the (db, dialect) pair for the current runmode,
// independent of the server's startup wiring (internal/app) — the shared
// helper behind every operator-facing CLI subcommand that needs a live
// connection to the same database the server uses without booting the
// whole HTTP stack (`triagefactory migrate status`, `triagefactory
// instance list/drain/undrain`, ...). Local mode opens the same SQLite
// file the server uses; multi mode opens TF_DATABASE_URL via pgx. Callers
// own closing the returned *sql.DB.
func OpenForCLI() (*sql.DB, string, error) {
	if runmode.Current() == runmode.ModeMulti {
		dsn := secretenv.Get("TF_DATABASE_URL")
		if dsn == "" {
			return nil, "", fmt.Errorf("TF_MODE=multi requires TF_DATABASE_URL")
		}
		// PoolAdmin: TF_DATABASE_URL is the superuser DSN, the same one
		// the server's admin pool uses. A CLI invocation is short-lived
		// and opens no second pool, so this is its only one.
		conn, err := OpenTraced("pgx", dsn, PoolAdmin)
		if err != nil {
			return nil, "", fmt.Errorf("open postgres: %w", err)
		}
		if err := conn.Ping(); err != nil {
			conn.Close()
			return nil, "", fmt.Errorf("ping postgres: %w", err)
		}
		return conn, "postgres", nil
	}
	conn, err := Open()
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	return conn, "sqlite3", nil
}

// Migrate brings the schema to head by applying any pending forward
// migrations from the embedded tree matching `dialect` (sqlite3 or
// postgres), then reconciles events_catalog against
// domain.AllEventTypes() (see SeedEventTypes). Idempotent — both the
// migration run and the catalog reconciliation are safe to repeat.
// Existing SQLite installs that predate the migration runner are
// stamped at the baseline on first run.
//
// The dialect is caller-provided rather than auto-detected: the caller
// opened the connection and knows which driver it used, and threading
// the value through avoids brittle driver-type reflection inside the
// runner. SQLite callers pass "sqlite3"; Postgres callers pass
// "postgres". See migrations.go for the runner.
//
// A TF_ROLE=executor process skips SeedEventTypes too, not just
// goose.Up: it's DML rather than DDL and harmless if it did run, but
// only the control plane (where the router lives) has any use for
// events_catalog, and skipping it avoids an old executor build
// stomping a label/description a newer control-plane build already
// updated.
func Migrate(db *sql.DB, dialect string) error {
	if err := runMigrations(db, dialect); err != nil {
		return err
	}
	if dialect == "postgres" && isExecutorRole() {
		return nil
	}
	return SeedEventTypes(db, dialect)
}
