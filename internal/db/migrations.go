package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"

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

// tfRoleExecutorEnv is the TF_ROLE value runMigrations gates goose.Up
// on. The full TF_ROLE plumbing — subsystem wiring, per-role pool
// defaults, the N-executor compose profile — is TFAC-582, which
// depends on this ticket landing first. Reading the env var directly
// here (rather than standing up a stub in internal/runmode) keeps this
// migration-coordination assert self-contained: it doesn't partially
// build a config surface that belongs to TFAC-582, and it costs
// nothing to ship ahead of that split (see the parent epic's P0
// framing — "the assert is cheap now").
const tfRoleExecutorEnv = "executor"

// isExecutorRole reports whether this process is TF_ROLE=executor.
// Case-insensitive and whitespace-trimmed, mirroring internal/runmode's
// env-parsing conventions even though this doesn't route through that
// package (see tfRoleExecutorEnv).
func isExecutorRole() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TF_ROLE")), tfRoleExecutorEnv)
}

// ErrExecutorSchemaBehind and ErrExecutorSchemaAhead are returned by
// Migrate when a TF_ROLE=executor process's connected Postgres schema
// doesn't match what its own embedded migration tree expects. Neither
// resolves in-process: every existing Migrate caller already treats a
// non-nil error as fatal at boot (main.go, cmd/migrate, openStores), so
// the process exits and the deployment's restart/backoff policy is
// what retries — "wait" here means an external process restart, not an
// in-process sleep loop. See assertExecutorSchemaCompatible.
var (
	ErrExecutorSchemaBehind = errors.New("connected schema is behind this executor build's expected version")
	ErrExecutorSchemaAhead  = errors.New("connected schema is ahead of this executor build's expected version")
)

// gooseMu guards every call into the goose package below. goose.SetBaseFS
// / goose.SetDialect are plain package-level globals with no
// synchronization of their own — goose.Up, goose.CollectMigrations, and
// goose.GetDBVersion all read them internally over their full
// execution, not just at entry. In production this is moot (each
// process calls Migrate exactly once, synchronously, at boot); it only
// matters if something in-process ever called Migrate/MigrationStatus
// concurrently from multiple goroutines, which would otherwise race on
// goose's shared state regardless of anything the pg_advisory_lock
// below does — that lock coordinates separate OS processes over the
// shared database, it says nothing about two goroutines sharing one
// process's copy of goose's package globals.
var gooseMu sync.Mutex

// runMigrations brings the on-disk schema up to head via goose — or,
// for a TF_ROLE=executor process on Postgres, refuses to and instead
// asserts the already-migrated schema matches what this build expects
// (spec TFAC-71 §5.5: executors never migrate).
//
// Sequence:
//  1. assertFreshOrCurrent gates entry — pre-v1.11.0 installs refuse
//     here before any DDL runs.
//  2. Route to the embedded tree for dialect and configure goose.
//  3. SQLite (single process by construction) and Postgres
//     control/all-role processes call goose.Up — Postgres wrapped in a
//     session-level advisory lock so M concurrently-booting pods
//     serialize cleanly. A Postgres executor-role process instead
//     calls assertExecutorSchemaCompatible and never touches DDL.
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

	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set dialect %s: %w", dialect, err)
	}

	if dialect != "postgres" {
		// SQLite: internal/db.Open caps the pool at one connection, so
		// there is no cross-process race to coordinate, and TF_ROLE is
		// a multi-mode-only concept.
		if err := goose.Up(db, treeDir); err != nil {
			return fmt.Errorf("goose up: %w", err)
		}
		return nil
	}

	if isExecutorRole() {
		return assertExecutorSchemaCompatible(db, treeDir)
	}
	return runPostgresMigrationLocked(db, treeDir)
}

// postgresMigrationLockText seeds the fixed session-level advisory
// lock every migrating Postgres process acquires before calling
// goose.Up. hashtextextended over a literal keeps this lock's key
// space separate from the org/user-keyed xact-locks elsewhere
// (internal/server/auth_provision.go, internal/db/postgres/
// team_github_repos.go) without either family needing to agree on a
// shared key scheme — those lock one entity for the duration of a
// transaction; this locks the entire migration run for the whole
// process.
const postgresMigrationLockText = "triagefactory:goose-migrate"

// runPostgresMigrationLocked wraps goose.Up in a session-level
// pg_advisory_lock so M concurrently-booting control pods serialize
// their migration attempt. goose has no cross-process coordination of
// its own — two processes racing to apply the same pending migration
// can both observe "not yet applied" and either double-run DDL or
// corrupt the goose_db_version bookkeeping.
//
// The lock is session-scoped (tied to one physical backend
// connection), not transaction-scoped: goose.Up runs each migration in
// its own transaction internally, so an *xact_lock held only for the
// first of those transactions would release long before the rest ran.
// A dedicated *sql.Conn checked out of the pool for the duration is
// what makes a session lock usable here — a bare db.ExecContext call
// could hand the lock and unlock statements two different physical
// connections from the pool, silently failing to serialize anything.
//
// (*sql.Conn).Close alone is NOT a backstop for the lock: it only
// returns the connection to the pool, it doesn't end the backend
// session — so if the explicit pg_advisory_unlock below ever failed,
// a plain Close would leave the lock held on a connection sitting in
// the pool, possibly reused for unrelated queries, until the pool
// happens to evict it. conn.Raw + driver.ErrBadConn tells database/sql
// to physically close the connection instead of pooling it, so the
// backend session — and with it the lock — always ends here,
// regardless of whether the unlock statement succeeded.
func runPostgresMigrationLocked(database *sql.DB, treeDir string) error {
	ctx := context.Background()
	conn, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration-lock connection: %w", err)
	}
	defer func() {
		_ = conn.Raw(func(driverConn any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()

	if _, err := conn.ExecContext(ctx,
		`SELECT pg_advisory_lock(hashtextextended($1, 0))`, postgresMigrationLockText,
	); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx,
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, postgresMigrationLockText,
		); err != nil {
			migrateLog.Warn("release migration advisory lock", "error", err)
		}
	}()

	if err := goose.Up(database, treeDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// assertExecutorSchemaCompatible implements the executor half of the
// role split: TF_ROLE=executor never runs goose.Up, so instead it
// compares the connected schema's current goose version against the
// highest version this build's embedded migration tree carries and
// refuses to boot on any mismatch rather than guessing:
//
//   - behind (current < expected): a sibling control pod hasn't
//     finished migrating yet, or none has been deployed. See
//     ErrExecutorSchemaBehind for why this "waits" via process exit +
//     external restart rather than an in-process retry loop.
//   - ahead (current > expected): the control plane has already rolled
//     the schema forward past what this binary understands. Never
//     self-resolves — the fix is deploying a newer executor build
//     first (spec §5.5's drain-first-on-schema-change contract, so a
//     stale executor never runs against DDL it doesn't recognize).
//   - equal: proceed.
func assertExecutorSchemaCompatible(database *sql.DB, treeDir string) error {
	expected, err := headMigrationVersion(treeDir)
	if err != nil {
		return fmt.Errorf("collect embedded migration tree: %w", err)
	}
	current, tracked, err := currentPostgresSchemaVersion(database)
	if err != nil {
		return fmt.Errorf("read goose_db_version: %w", err)
	}
	if !tracked {
		return fmt.Errorf("%w: no goose_db_version table found — this database has never been migrated by a control-plane process (this build expects version %d)",
			ErrExecutorSchemaBehind, expected)
	}
	switch {
	case current < expected:
		return fmt.Errorf("%w: schema at %d, this build expects %d — waiting on a control-plane pod to finish migrating",
			ErrExecutorSchemaBehind, current, expected)
	case current > expected:
		return fmt.Errorf("%w: schema at %d, this build only understands up to %d — deploy a newer executor build",
			ErrExecutorSchemaAhead, current, expected)
	default:
		return nil
	}
}

// headMigrationVersion returns the highest version in the embedded
// migration tree at treeDir — this build's "expected" schema head.
// Read-only against the FS goose.SetBaseFS already points at; never
// touches the DB.
func headMigrationVersion(treeDir string) (int64, error) {
	migrations, err := goose.CollectMigrations(treeDir, 0, goose.MaxVersion)
	if err != nil {
		return 0, err
	}
	if len(migrations) == 0 {
		return 0, nil
	}
	return migrations[len(migrations)-1].Version, nil
}

// currentPostgresSchemaVersion reads the connected DB's current goose
// version without the side effects goose.GetDBVersion carries — it
// creates goose_db_version (stamping version 0) if the table doesn't
// exist yet, which an executor must never trigger. tracked is false
// when the table doesn't exist at all.
//
// The existence probe uses to_regclass rather than pg_tables +
// schemaname = 'public': goose itself never schema-qualifies the
// tracking table (TableName() is the bare "goose_db_version", resolved
// through whatever the connection's search_path finds), so hardcoding
// 'public' here could report "missing" for a table goose would
// otherwise happily see. to_regclass performs the same unqualified,
// search_path-driven lookup goose's own dialect queries rely on, so
// this can't disagree with what goose considers "the" tracking table.
//
// MAX(version_id) over is_applied rows is sufficient here — rather
// than re-deriving goose's own descending-scan is_applied/rollback
// logic — because TF's migrations never expose Down (see CLAUDE.md's
// goose conventions: Down blocks are no-ops), so version_id is
// monotonically inserted and never rolled back.
func currentPostgresSchemaVersion(database *sql.DB) (version int64, tracked bool, err error) {
	if err := database.QueryRow(
		`SELECT to_regclass('goose_db_version') IS NOT NULL`,
	).Scan(&tracked); err != nil {
		return 0, false, fmt.Errorf("probe goose_db_version: %w", err)
	}
	if !tracked {
		return 0, false, nil
	}
	if err := database.QueryRow(
		`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`,
	).Scan(&version); err != nil {
		return 0, false, fmt.Errorf("read goose_db_version: %w", err)
	}
	return version, true, nil
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

	gooseMu.Lock()
	defer gooseMu.Unlock()

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
