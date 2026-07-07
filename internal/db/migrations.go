package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations-sqlite/*.sql
var migrationsSQLiteFS embed.FS

//go:embed migrations-postgres
var migrationsPostgresFS embed.FS

// v1110BaselineVersionID is the goose version_id of the v1.11.0
// consolidated baseline. It is the cutover boundary: a database is
// "current" if and only if `goose_db_version` shows this version as
// applied. Anything older — legacy `schema_migrations`-tracked DBs
// from the pre-goose runner, the pre-v1.11.0 goose baseline at
// 202605090001, or app tables without any version metadata — is
// refused at boot.
//
// The brick policy traded the install-upgrade path for a clean
// migration tree (see CHANGELOG v1.11.0). There is no in-place
// upgrade from pre-v1.11.0; operators wipe ~/.triagefactory/ and
// reinstall.
const v1110BaselineVersionID int64 = 202605130001

// ErrPreV1110Install is returned by Migrate when the database
// predates the v1.11.0 cutover. The wrapped message names the
// remediation directly — wipe + reinstall — so the operator gets
// an actionable error without needing to read release notes. Two
// wipe paths are suggested: `triagefactory uninstall` for users who
// installed the binary, `./scripts/clean-slate.sh` for developers
// working from source.
var ErrPreV1110Install = errors.New(
	"this database predates Triage Factory v1.11.0, which is a hard reset boundary. " +
		"Wipe local state and reinstall to start fresh: run `triagefactory uninstall` " +
		"(installed users) or `./scripts/clean-slate.sh` (developers working from source)",
)

// runMigrations brings the on-disk schema up to head via goose.
//
// Sequence:
//  1. assertFreshOrCurrent gates entry — pre-v1.11.0 installs refuse
//     here before any DDL runs.
//  2. Hand the routed embed.FS to goose and call goose.Up.
//
// Failures roll back at the per-migration boundary goose owns; the
// next launch retries any unapplied migration.
func runMigrations(db *sql.DB, dialect string) error {
	if err := assertFreshOrCurrent(db, dialect); err != nil {
		return err
	}
	treeFS, treeDir, err := migrationsFor(dialect)
	if err != nil {
		return err
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set dialect %s: %w", dialect, err)
	}
	if err := goose.Up(db, treeDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// migrationsFor returns the embedded migration tree for a goose
// dialect. Trees are kept side-by-side so the parser only ever sees
// DDL it can interpret — no runtime if/else inside a single migration
// file deciding whether to emit BYTEA or BLOB.
func migrationsFor(dialect string) (fs.FS, string, error) {
	switch dialect {
	case "sqlite3":
		return migrationsSQLiteFS, "migrations-sqlite", nil
	case "postgres":
		return migrationsPostgresFS, "migrations-postgres", nil
	default:
		return nil, "", fmt.Errorf("unsupported dialect %q", dialect)
	}
}

// assertFreshOrCurrent refuses pre-v1.11.0 installs. The check is a
// single goose-tracker probe:
//
//   - goose_db_version contains the v1.11.0 baseline as applied →
//     proceed (current install, goose.Up handles any forward
//     migrations).
//   - goose_db_version does not exist AND no application sentinel
//     table (`entities`) → proceed (fresh install; goose.Up will
//     create the tracker and run the baseline).
//   - anything else → ErrPreV1110Install. Covers (a) older goose
//     installs stamped at the earlier baseline 202605090001 without
//     the v1.11.0 baseline applied, (b) legacy `schema_migrations`
//     installs from before the goose cutover, and (c) pre-runner
//     installs that have app tables but no version metadata.
func assertFreshOrCurrent(db *sql.DB, dialect string) error {
	if dialect == "postgres" {
		// D3 Postgres has no pre-v1.11.0 users — fresh installs only.
		// goose.Up handles the rest.
		return nil
	}

	hasGoose, err := tableExists(db, "goose_db_version")
	if err != nil {
		return fmt.Errorf("probe goose_db_version: %w", err)
	}
	if hasGoose {
		var count int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ? AND is_applied = 1`,
			v1110BaselineVersionID,
		).Scan(&count); err != nil {
			return fmt.Errorf("read goose_db_version: %w", err)
		}
		if count > 0 {
			return nil
		}
		return ErrPreV1110Install
	}

	hasEntities, err := tableExists(db, "entities")
	if err != nil {
		return fmt.Errorf("probe entities: %w", err)
	}
	if hasEntities {
		// App tables present, no goose tracker — either a legacy
		// schema_migrations-tracked install or a pre-runner install.
		// Either way, pre-v1.11.0.
		return ErrPreV1110Install
	}

	// Fresh install: no tracker, no app tables.
	return nil
}

// tableExists reports whether sqlite_master has a row for the named
// table. Used by assertFreshOrCurrent to probe goose_db_version and
// entities without erroring on missing tables.
func tableExists(db *sql.DB, table string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe sqlite_master for %s: %w", table, err)
	}
	return n > 0, nil
}

// MigrationStatus prints the per-migration applied/pending state to w.
// Drives the `triagefactory migrate status` operator command. Calls
// assertFreshOrCurrent first so an old DB reports cleanly through the
// same brick path Migrate uses, rather than emitting confusing
// per-file pending state.
func MigrationStatus(db *sql.DB, dialect string, w io.Writer) error {
	if err := assertFreshOrCurrent(db, dialect); err != nil {
		return err
	}
	treeFS, treeDir, err := migrationsFor(dialect)
	if err != nil {
		return err
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set dialect %s: %w", dialect, err)
	}
	// Quiet goose's chatty default logger so the operator sees only
	// our table, not goose's per-file announcements.
	goose.SetLogger(goose.NopLogger())
	migrations, err := goose.CollectMigrations(treeDir, 0, goose.MaxVersion)
	if err != nil {
		return fmt.Errorf("collect migrations: %w", err)
	}
	current, err := goose.GetDBVersion(db)
	if err != nil {
		return fmt.Errorf("get db version: %w", err)
	}
	fmt.Fprintf(w, "    Status                      Migration\n")
	fmt.Fprintf(w, "    ====================================\n")
	for _, m := range migrations {
		state := "Pending"
		if m.Version <= current {
			state = "Applied"
		}
		fmt.Fprintf(w, "    %-27s %s\n", state, m.Source)
	}
	fmt.Fprintf(w, "\n    db version: %d\n", current)
	return nil
}
