package db_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// tfMigrateSubprocessDSNEnv is set by
// TestMigrate_Postgres_ConcurrentMigrateSerializes on the child
// processes it execs, carrying the DSN each child should independently
// connect to and migrate. Its presence is what tells
// TestMigrateSubprocessHelper it's running as a child rather than as
// an ordinary (skipped) test in a normal `go test` invocation.
const tfMigrateSubprocessDSNEnv = "TF_MIGRATE_SUBPROCESS_DSN"

// TestMigrateSubprocessHelper is not an independent test — it is the
// child-process entrypoint TestMigrate_Postgres_ConcurrentMigrateSerializes
// execs (via exec.Command(os.Args[0], ...), the standard Go
// re-invoke-the-test-binary-as-a-subprocess idiom) to get a genuinely
// separate OS process attempting Migrate, rather than a goroutine
// racing another goroutine on goose's own non-thread-safe package-level
// globals within ONE process. A plain `go test` run (no env var set)
// treats this as a no-op skip.
func TestMigrateSubprocessHelper(t *testing.T) {
	dsn := os.Getenv(tfMigrateSubprocessDSNEnv)
	if dsn == "" {
		t.Skip("not invoked as a migrate subprocess (see TestMigrate_Postgres_ConcurrentMigrateSerializes)")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn, "postgres"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestMigrate_Postgres_ConcurrentMigrateSerializes is the regression
// guard for the advisory-lock wrap: two processes booting
// against the same never-migrated Postgres database concurrently must
// not race goose's own version-tracking inserts. Without the lock,
// both processes would observe "nothing applied yet" and race to run
// the (sizable) baseline migration's DDL simultaneously — which fails
// outright (duplicate CREATE TABLE) far more often than it silently
// corrupts, so this is a real regression guard, not just a sanity
// check.
//
// Deliberately spawns two real OS processes (exec.Command re-invoking
// this test binary as TestMigrateSubprocessHelper) rather than two
// goroutines in this one process: goose.SetBaseFS / goose.SetDialect
// are unsynchronized package-level globals, so two goroutines calling
// db.Migrate concurrently would race on those regardless of whether
// the advisory lock they contend on afterward is correct — that lock
// coordinates the shared database across processes, it does nothing
// for two goroutines sharing one process's copy of goose's in-memory
// config. Separate processes have separate address spaces (and hence
// separate copies of goose's globals), so this is both race-detector
// clean AND a more faithful reproduction of "M concurrently-booting
// control pods" than goroutines ever were.
//
// Uses pgtest.NewInstance (a dedicated, never-migrated container)
// rather than pgtest.Shared — Shared's tf_test is always already fully
// migrated by the time a test can touch it, which would make this a
// no-op (nothing pending to race over).
func TestMigrate_Postgres_ConcurrentMigrateSerializes(t *testing.T) {
	adminDB, dsn := pgtest.NewInstance(t)

	const concurrency = 2
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestMigrateSubprocessHelper$", "-test.v")
			cmd.Env = append(os.Environ(), tfMigrateSubprocessDSNEnv+"="+dsn)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("subprocess %d: %w\n%s", i, err, out)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate[%d]: %v", i, err)
		}
	}

	// No corruption: exactly one row per version_id (a race that
	// double-applied a migration would show up here as a duplicate
	// insert into goose_db_version, or as one of the two subprocesses
	// above erroring out first).
	var total, distinct int
	if err := adminDB.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT version_id) FROM goose_db_version`,
	).Scan(&total, &distinct); err != nil {
		t.Fatalf("count goose_db_version: %v", err)
	}
	if total != distinct {
		t.Errorf("goose_db_version has duplicate version_id rows: total=%d distinct=%d", total, distinct)
	}
	if total == 0 {
		t.Error("goose_db_version is empty after Migrate — baseline never applied")
	}

	for _, table := range []string{"entities", "events", "tasks", "runs", "orgs", "users", "instances"} {
		var exists bool
		if err := adminDB.QueryRow(
			`SELECT to_regclass($1) IS NOT NULL`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table missing after concurrent Migrate", table)
		}
	}
}

// TestMigrate_Postgres_ExecutorRole_NeverMigrated_Behind covers the
// executor-boot-against-a-behind-schema case: TF_ROLE=executor against
// a database no control-plane process has ever migrated must refuse to
// boot with ErrExecutorSchemaBehind, and
// — the load-bearing assertion — must not create a single table doing
// so. An executor that silently ran goose.Up here would defeat the
// entire point of the role split.
func TestMigrate_Postgres_ExecutorRole_NeverMigrated_Behind(t *testing.T) {
	adminDB, _ := pgtest.NewInstance(t)
	t.Setenv("TF_ROLE", "executor")

	err := db.Migrate(adminDB, "postgres")
	if !errors.Is(err, db.ErrExecutorSchemaBehind) {
		t.Fatalf("Migrate err = %v, want ErrExecutorSchemaBehind", err)
	}

	for _, table := range []string{"goose_db_version", "entities", "orgs"} {
		var exists bool
		if err := adminDB.QueryRow(
			`SELECT to_regclass($1) IS NOT NULL`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if exists {
			t.Errorf("%s exists after executor-role Migrate on a never-migrated DB — executors must never run DDL", table)
		}
	}
}

// TestMigrate_Postgres_ExecutorRole_Ahead covers the other refuse-to-
// boot half: a schema stamped past what this build's embedded
// migration tree understands (simulating a control plane that already
// rolled forward with a newer binary) must refuse with
// ErrExecutorSchemaAhead — the "deploy a newer executor" case, which
// unlike the behind case never self-resolves by waiting.
func TestMigrate_Postgres_ExecutorRole_Ahead(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	const aheadVersion = 999999999999
	if _, err := h.AdminDB.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`, aheadVersion,
	); err != nil {
		t.Fatalf("stamp ahead version: %v", err)
	}
	t.Cleanup(func() {
		if _, err := h.AdminDB.Exec(
			`DELETE FROM goose_db_version WHERE version_id = $1`, aheadVersion,
		); err != nil {
			t.Errorf("cleanup ahead version stamp: %v", err)
		}
	})

	t.Setenv("TF_ROLE", "executor")
	err := db.Migrate(h.AdminDB, "postgres")
	if !errors.Is(err, db.ErrExecutorSchemaAhead) {
		t.Fatalf("Migrate err = %v, want ErrExecutorSchemaAhead", err)
	}
}

// TestMigrate_Postgres_ExecutorRole_Current covers the happy path: an
// executor whose schema exactly matches what its embedded tree expects
// boots cleanly (and, implicitly, never attempts goose.Up — Shared(t)'s
// admin pool has no permission problem that would run into, so this
// only proves the version-compare's equal branch, not the DDL-skip;
// that's covered by the Behind test's "no tables created" assertion).
func TestMigrate_Postgres_ExecutorRole_Current(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	t.Setenv("TF_ROLE", "executor")
	if err := db.Migrate(h.AdminDB, "postgres"); err != nil {
		t.Fatalf("Migrate on a current schema: %v", err)
	}
}

// TestMigrate_Postgres_ExecutorRole_SkipsEventTypeSeed pins that
// Migrate's executor-role skip covers SeedEventTypes too, not just
// goose.Up. Deliberately drifts one events_catalog row's description
// away from its domain.AllEventTypes() canonical value — a
// control-plane Migrate call's WHERE-guarded upsert would correct this
// straight back, so seeing it still drifted after an executor-role
// Migrate call is the only direct way to observe the reconciliation
// write was skipped (the upsert is otherwise a no-op whether it runs
// or not, since it's idempotent).
func TestMigrate_Postgres_ExecutorRole_SkipsEventTypeSeed(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	const drifted = "drifted-for-test"
	if _, err := h.AdminDB.Exec(
		`UPDATE events_catalog SET description = $1 WHERE id = $2`,
		drifted, string(domain.EventGitHubPRReviewApproved),
	); err != nil {
		t.Fatalf("drift events_catalog description: %v", err)
	}

	t.Setenv("TF_ROLE", "executor")
	if err := db.Migrate(h.AdminDB, "postgres"); err != nil {
		t.Fatalf("Migrate on a current schema: %v", err)
	}

	var desc string
	if err := h.AdminDB.QueryRow(
		`SELECT description FROM events_catalog WHERE id = $1`, string(domain.EventGitHubPRReviewApproved),
	).Scan(&desc); err != nil {
		t.Fatalf("read events_catalog description: %v", err)
	}
	if desc != drifted {
		t.Errorf("description = %q, want still %q — executor-role Migrate must not run SeedEventTypes", desc, drifted)
	}
}
