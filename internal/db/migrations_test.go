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
// for migration tests. Distinct from newTestDB (which calls Migrate
// for you) — these tests exercise Migrate itself.
func openMigrationsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite memory: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

// TestMigrate_FreshInstall pins the bootstrap path: a blank DB ends up
// with all baseline tables and a goose_db_version row stamping the
// v1.11.0 baseline as applied.
func TestMigrate_FreshInstall(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var version int64
	if err := database.QueryRow(
		`SELECT version_id FROM goose_db_version WHERE version_id = ?`, v1110BaselineVersionID,
	).Scan(&version); err != nil {
		t.Fatalf("baseline not stamped in goose_db_version: %v", err)
	}
	if version != v1110BaselineVersionID {
		t.Errorf("version_id = %d, want %d", version, v1110BaselineVersionID)
	}

	for _, table := range []string{
		"entities", "events", "tasks", "runs", "projects",
		"orgs", "users", "event_handlers",
		"instance_config", "org_settings", "team_settings", "user_settings",
		"jira_project_status_rules",
	} {
		exists, err := tableExists(database, table)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s table missing after fresh Migrate", table)
		}
	}
}

// TestMigrate_Idempotent guards the "launch on an up-to-date DB" case.
// Two Migrate calls in a row leave the post-state unchanged.
func TestMigrate_Idempotent(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, v1110BaselineVersionID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("goose_db_version[%d] count = %d, want 1", v1110BaselineVersionID, n)
	}
}

// TestMigrate_BricksPreV1110_GooseStampedAtOldBaseline covers the
// most-likely upgrade attempt: a user on the SKY-245 baseline (or any
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
	// Stamp the pre-v1.11.0 SKY-245 baseline (202605090001) and one
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

// TestMigrate_RunCuratorTokenColumnsPresent pins that the TFAC-473 token
// columns land on both runs and curator_requests after a full fresh
// migration. A SELECT of the four columns errors if any is missing, so the
// LIMIT 0 probe is a column-existence assertion without needing rows.
func TestMigrate_RunCuratorTokenColumnsPresent(t *testing.T) {
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, table := range []string{"runs", "curator_requests"} {
		if _, err := database.Exec(
			`SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens FROM ` + table + ` LIMIT 0`,
		); err != nil {
			t.Errorf("%s missing a token column after migrate: %v", table, err)
		}
	}
}

// TestMigrate_BackfillsRunTokensFromRunMessages pins the TFAC-473 SQLite
// backfill: a run that completed BEFORE the migration (its per-message
// tokens already in run_messages, but no denormalized total) gets its four
// token columns populated from the run_messages SUM when the migration
// runs. Driven through goose directly so we can land the pre-migration
// fixture between the prior migration and the new one.
func TestMigrate_BackfillsRunTokensFromRunMessages(t *testing.T) {
	// Foreign keys off (plain :memory:) so the fixture needs no entity /
	// task / prompt / blueprint scaffolding — the backfill is pure SQL over
	// runs ⋈ run_messages and doesn't care about FK targets. The CHECK
	// constraints still apply, so the run carries origin='blueprint' parents.
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Migrate up to the migration *before* the token-breakdown one, so the
	// runs table does not yet have the token columns.
	const priorVersion = 202606250002
	if err := goose.UpTo(database, "migrations-sqlite", priorVersion); err != nil {
		t.Fatalf("goose UpTo %d: %v", priorVersion, err)
	}

	// A completed run with two token-bearing run_messages rows — history
	// that predates the backfill.
	if _, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, blueprint_run_id, status)
		VALUES ('r1', 't1', 'p1', 'b1', 'completed')
	`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO run_messages (run_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES ('r1', 'assistant', 100, 20, 1000, 7),
		       ('r1', 'assistant',  50,  5,  500, 3)
	`); err != nil {
		t.Fatalf("seed run_messages: %v", err)
	}

	// Apply the rest (the token-breakdown migration + backfill).
	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
		FROM runs WHERE id = 'r1'
	`).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read backfilled tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("backfilled tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
	}
}

// TestMigrate_BackfillsCuratorTokensFromMessages pins the TFAC-473 SQLite
// backfill for the curator side: a request that completed BEFORE the
// migration (its per-message tokens already in curator_messages) gets its
// four token columns populated from the curator_messages SUM when the
// migration runs. Symmetric with the runs backfill — the per-message data
// was on disk all along, so historical curator spend is recovered, not just
// going-forward.
func TestMigrate_BackfillsCuratorTokensFromMessages(t *testing.T) {
	// Foreign keys off (plain :memory:) so the fixture needs no projects
	// scaffolding — the backfill is pure SQL over
	// curator_requests ⋈ curator_messages and doesn't care about FK targets.
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Migrate up to the migration *before* the token-breakdown one, so
	// curator_requests does not yet have the token columns.
	const priorVersion = 202606250002
	if err := goose.UpTo(database, "migrations-sqlite", priorVersion); err != nil {
		t.Fatalf("goose UpTo %d: %v", priorVersion, err)
	}

	// A completed request with two token-bearing curator_messages rows —
	// history that predates the backfill.
	if _, err := database.Exec(`
		INSERT INTO curator_requests (id, project_id, status, user_input)
		VALUES ('cr1', 'proj1', 'done', 'hi')
	`); err != nil {
		t.Fatalf("seed curator_request: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO curator_messages (request_id, role, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		VALUES ('cr1', 'assistant', 100, 20, 1000, 7),
		       ('cr1', 'assistant',  50,  5,  500, 3)
	`); err != nil {
		t.Fatalf("seed curator_messages: %v", err)
	}

	// Apply the rest (the token-breakdown migration + backfill).
	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens
		FROM curator_requests WHERE id = 'cr1'
	`).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read backfilled tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("backfilled tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
	}
}

// TestMigrate_CuratorTeamIDBackfillAndView pins the TFAC-476 SQLite migration:
// curator_requests gains a nullable team_id, existing rows backfill from their
// project's team (team project → that team; null-team org/private project →
// NULL), and the recreated llm_spend view surfaces team_id on the curator arm.
// Together these are the migration's acceptance: applies on an existing DB,
// backfills, and the view reads the project's team for team-visible projects and
// NULL otherwise.
func TestMigrate_CuratorTeamIDBackfillAndView(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Migrate up to the migration *before* the curator team_id one, so
	// curator_requests does not yet have team_id and llm_spend's curator arm
	// still hardcodes NULL.
	const priorVersion = 202606260005
	if err := goose.UpTo(database, "migrations-sqlite", priorVersion); err != nil {
		t.Fatalf("goose UpTo %d: %v", priorVersion, err)
	}

	const teamID = "00000000-0000-0000-0000-000000000010"
	// A team-visible project (carries a team) and a null-team org-visible project.
	// org_id / creator_user_id fall to their NOT NULL DEFAULT sentinels.
	if _, err := database.Exec(`
		INSERT INTO projects (id, name, team_id, visibility) VALUES
			('pteam', 'team-proj', ?,    'team'),
			('porg',  'org-proj',  NULL, 'org')
	`, teamID); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	// Two curator requests that predate the migration (no team_id column yet).
	if _, err := database.Exec(`
		INSERT INTO curator_requests (id, project_id, status, user_input) VALUES
			('ct', 'pteam', 'done', 'hi'),
			('co', 'porg',  'done', 'hi')
	`); err != nil {
		t.Fatalf("seed curator_requests: %v", err)
	}

	// Apply the rest (the team_id migration: ALTER + backfill + view recreate).
	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	// Column backfilled from each row's project team.
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM curator_requests WHERE id = 'ct'`, teamID)
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM curator_requests WHERE id = 'co'`, "")

	// View surfaces the same team_id on the curator arm.
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM llm_spend WHERE source = 'curator' AND source_id = 'ct'`, teamID)
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM llm_spend WHERE source = 'curator' AND source_id = 'co'`, "")
}

// TestMigrate_LLMSpendTriggerIDView pins the TFAC-478 SQLite migration: applied
// on a DB that already carries the pre-008 view, it DROP+RECREATEs llm_spend so
// the runs arm exposes trigger_id while the curator + system arms emit NULL —
// and it preserves TFAC-476's curator team_id (re-emitted verbatim). The runs
// arm's trigger_id population is covered by the SpendStore conformance suite
// (which reads the fully-migrated view); here we assert the migration applies on
// existing data, the new column appears, and the curator arm stays intact.
func TestMigrate_LLMSpendTriggerIDView(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	goose.SetBaseFS(migrationsSQLiteFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Up to the migration *before* trigger_id: the view exists (already carrying
	// TFAC-476's curator team_id) but has no trigger_id column yet.
	const priorVersion = 202606260007
	if err := goose.UpTo(database, "migrations-sqlite", priorVersion); err != nil {
		t.Fatalf("goose UpTo %d: %v", priorVersion, err)
	}

	const teamID = "00000000-0000-0000-0000-000000000010"
	if _, err := database.Exec(`
		INSERT INTO projects (id, name, team_id, visibility) VALUES
			('pteam', 'team-proj', ?,    'team'),
			('porg',  'org-proj',  NULL, 'org')
	`, teamID); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	// curator_requests already has team_id at this version (TFAC-476), so set it
	// directly: the team project's row carries the team, the org project's NULL.
	if _, err := database.Exec(`
		INSERT INTO curator_requests (id, project_id, team_id, status, user_input) VALUES
			('ct', 'pteam', ?,    'done', 'hi'),
			('co', 'porg',  NULL, 'done', 'hi')
	`, teamID); err != nil {
		t.Fatalf("seed curator_requests: %v", err)
	}

	// The pre-008 view must NOT yet expose trigger_id.
	if rows, err := database.Query(`SELECT trigger_id FROM llm_spend LIMIT 0`); err == nil {
		rows.Close()
		t.Fatal("llm_spend exposed trigger_id before the 008 migration; prior view shape wrong")
	}

	// Apply the trigger_id migration (DROP + recreate).
	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	// trigger_id column now exists on the recreated view.
	if rows, err := database.Query(`SELECT trigger_id FROM llm_spend LIMIT 0`); err != nil {
		t.Fatalf("llm_spend missing trigger_id after the 008 migration: %v", err)
	} else {
		rows.Close()
	}

	// Curator arm: team_id preserved from TFAC-476; trigger_id NULL (only runs
	// are trigger-fired).
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM llm_spend WHERE source = 'curator' AND source_id = 'ct'`, teamID)
	assertCuratorTeamID(t, database,
		`SELECT team_id FROM llm_spend WHERE source = 'curator' AND source_id = 'co'`, "")
	var triggerID sql.NullString
	if err := database.QueryRow(
		`SELECT trigger_id FROM llm_spend WHERE source = 'curator' AND source_id = 'ct'`,
	).Scan(&triggerID); err != nil {
		t.Fatalf("scan curator trigger_id: %v", err)
	}
	if triggerID.Valid {
		t.Errorf("curator arm trigger_id = %q, want NULL", triggerID.String)
	}
}

// assertCuratorTeamID runs a single-row team_id query and checks it against want
// ("" means SQL NULL is expected).
func assertCuratorTeamID(t *testing.T, database *sql.DB, query, want string) {
	t.Helper()
	var got sql.NullString
	if err := database.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if want == "" {
		if got.Valid {
			t.Errorf("%q = %q, want NULL", query, got.String)
		}
		return
	}
	if !got.Valid || got.String != want {
		t.Errorf("%q = %v (valid=%t), want %q", query, got.String, got.Valid, want)
	}
}
