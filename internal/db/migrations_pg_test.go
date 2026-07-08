package db_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestMigrate_Postgres_ConcurrentMigrateSerializes is the TFAC-580
// regression guard for the advisory-lock wrap: two processes booting
// against the same never-migrated Postgres database concurrently must
// not race goose's own version-tracking inserts. Without the lock,
// both goroutines would observe "nothing applied yet" and race to run
// the (sizable) baseline migration's DDL simultaneously — which fails
// outright (duplicate CREATE TABLE) far more often than it silently
// corrupts, so this is a real regression guard, not just a sanity
// check.
//
// Uses pgtest.NewInstance (a dedicated, never-migrated container)
// rather than pgtest.Shared — Shared's tf_test is always already fully
// migrated by the time a test can touch it, which would make this a
// no-op (nothing pending to race over).
func TestMigrate_Postgres_ConcurrentMigrateSerializes(t *testing.T) {
	adminDB := pgtest.NewInstance(t)

	const concurrency = 2
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Migrate(adminDB, "postgres")
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
	// insert into goose_db_version, or as one of the two calls above
	// erroring out first).
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
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table missing after concurrent Migrate", table)
		}
	}
}

// TestMigrate_Postgres_ExecutorRole_NeverMigrated_Behind covers the
// executor-boot-against-a-behind-schema half of TFAC-580's acceptance:
// TF_ROLE=executor against a database no control-plane process has
// ever migrated must refuse to boot with ErrExecutorSchemaBehind, and
// — the load-bearing assertion — must not create a single table doing
// so. An executor that silently ran goose.Up here would defeat the
// entire point of the role split.
func TestMigrate_Postgres_ExecutorRole_NeverMigrated_Behind(t *testing.T) {
	adminDB := pgtest.NewInstance(t)
	t.Setenv("TF_ROLE", "executor")

	err := db.Migrate(adminDB, "postgres")
	if !errors.Is(err, db.ErrExecutorSchemaBehind) {
		t.Fatalf("Migrate err = %v, want ErrExecutorSchemaBehind", err)
	}

	for _, table := range []string{"goose_db_version", "entities", "orgs"} {
		var exists bool
		if err := adminDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)`, table,
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
