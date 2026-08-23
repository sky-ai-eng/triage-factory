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
	database, err := sql.Open("sqlite", TestDSNMemory)
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
		"entities", "events", "tasks", "conversations", "messages", "claims",
		"orgs", "users", "event_handlers",
		"org_settings", "team_settings", "user_settings",
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

	// A fresh install must not resurrect any dissolved table.
	for _, table := range []string{
		"runs", "run_messages", "curator_requests", "curator_messages",
		"curator_pending_context", "run_pending_input",
		"staged_agent_injections", "run_credentials", "curator_turn_credentials",
		"curator_homes", "projects", "project_pinned_repos",
	} {
		exists, err := tableExists(database, table)
		if err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if exists {
			t.Errorf("dissolved table %s present after fresh Migrate", table)
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
	database := openMigrationsTestDB(t)
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
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

// TestMigrate_BackfillsRunTokensFromRunMessages pins the TFAC-473 SQLite
// backfill: a run that completed BEFORE the migration (its per-message
// tokens already in run_messages, but no denormalized total) gets its four
// token columns populated from the run_messages SUM when the migration
// runs. Driven through goose directly so we can land the pre-migration
// fixture between the prior migration and the new one.
func TestMigrate_BackfillsRunTokensFromRunMessages(t *testing.T) {
	// TestDSNMemoryNoForeignKeys so the fixture needs no entity / task /
	// prompt / blueprint scaffolding — the backfill is pure SQL over
	// runs ⋈ run_messages and doesn't care about FK targets. The CHECK
	// constraints still apply, so the run carries origin='blueprint' parents.
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
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

	// The refactor made messages the ledger: the per-message tokens survive
	// the run_messages → messages copy and SUM to the historical totals.
	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
		FROM messages WHERE conversation_id = 'r1'
	`).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read ledger tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("ledger tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
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
	// TestDSNMemoryNoForeignKeys so the fixture needs no projects
	// scaffolding — the backfill is pure SQL over
	// curator_requests ⋈ curator_messages and doesn't care about FK targets.
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
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

	// The conversations refactor re-parents each request under a curator
	// conversation whose project FK is enforced mid-migration, so the
	// project row must exist.
	if _, err := database.Exec(`
		INSERT INTO users (id) VALUES ('00000000-0000-0000-0000-000000000100');
		INSERT INTO projects (id, name, team_id) VALUES ('proj1', 'P1', '00000000-0000-0000-0000-000000000010');
	`); err != nil {
		t.Fatalf("seed project: %v", err)
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

	// The conversations refactor turns the request into a claim (claim id =
	// request id) whose stream rows keep their per-message tokens — the
	// ledger the per-turn accounting now derives from.
	var in, out, cr, cc int
	if err := database.QueryRow(`
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0)
		FROM messages WHERE claim_id = 'cr1'
	`).Scan(&in, &out, &cr, &cc); err != nil {
		t.Fatalf("read ledger tokens: %v", err)
	}
	if in != 150 || out != 25 || cr != 1500 || cc != 10 {
		t.Errorf("ledger tokens = (%d,%d,%d,%d), want (150,25,1500,10)", in, out, cr, cc)
	}
}

// TestMigrate_RunsTriggerIDBackfill pins the 202607060001 backfill: step conversations
// minted while enqueueBlueprintStep dropped the firing trigger (trigger_type =
// 'event' with a NULL trigger_id — the shape every autonomous run carried
// after the blueprint orchestrator unification) are healed from their parent
// blueprint_run's frozen trigger_id, so historical autonomous spend regains
// its by-rule attribution in llm_spend. Manual step conversations (parent carries no
// trigger) stay NULL.
func TestMigrate_RunsTriggerIDBackfill(t *testing.T) {
	database, err := sql.Open("sqlite", TestDSNMemory)
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

	// Up to the migration *before* the backfill, then seed the legacy shape.
	const priorVersion = 202607030001
	if err := goose.UpTo(database, "migrations-sqlite", priorVersion); err != nil {
		t.Fatalf("goose UpTo %d: %v", priorVersion, err)
	}
	// events_catalog is seeded by Migrate (not by goose alone), and the seed
	// chain below FKs into it via events / tasks / event_handlers.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	// Minimal FK chain for two step conversations: one under an event-fired
	// blueprint_run (trigger frozen on the parent only — the bug shape), one
	// under a manual blueprint_run. The sentinel user satisfies
	// conversations.creator_user_id's FK (pure-goose DBs carry no tenant rows); the
	// event_type FKs reuse the catalog rows seeded above.
	const userID = "00000000-0000-0000-0000-000000000100"
	for _, stmt := range []string{
		`INSERT INTO users (id) VALUES ('` + userID + `')`,
		`INSERT INTO entities (id, source, source_id, kind) VALUES ('e1', 'github', 'o/r#1', 'pr')`,
		`INSERT INTO events (id, event_type) VALUES ('ev1', (SELECT id FROM events_catalog LIMIT 1))`,
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id)
			VALUES ('t1', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'ev1')`,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'BP', '` + userID + `')`,
		`INSERT INTO event_handlers (id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES ('trig1', '` + userID + `', 'trigger', (SELECT id FROM events_catalog LIMIT 1), 'bp1', 3, 0.5)`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id, creator_user_id, step_plan, worktree_path)
			VALUES ('br-ev', 'bp1', 't1', 'event', 'trig1', NULL, '[]', '')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br-man', 'bp1', 't1', '[]', '')`,
		`INSERT INTO prompts (id, name, body, creator_user_id) VALUES ('p1', 'P', 'b', '` + userID + `')`,
		`INSERT INTO runs (id, task_id, prompt_id, trigger_type, creator_user_id, status, blueprint_run_id, blueprint_step_index, total_cost_usd)
			VALUES ('run-ev', 't1', 'p1', 'event', NULL, 'completed', 'br-ev', 0, 1.0)`,
		`INSERT INTO runs (id, task_id, prompt_id, status, blueprint_run_id, blueprint_step_index, total_cost_usd)
			VALUES ('run-man', 't1', 'p1', 'completed', 'br-man', 0, 2.0)`,
		`INSERT INTO run_messages (run_id, role, content) VALUES ('run-ev', 'assistant', 'work')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Apply the backfill.
	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	assertRunTriggerID(t, database, `SELECT trigger_id FROM conversations WHERE id = 'run-ev'`, "trig1")
	assertRunTriggerID(t, database, `SELECT trigger_id FROM conversations WHERE id = 'run-man'`, "")
	// And through the spend layer: the autonomous run's ledger row (its one
	// streamed message, now carrying the historical cost stamp) is
	// rule-attributed.
	assertRunTriggerID(t, database,
		`SELECT trigger_id FROM llm_spend WHERE source = 'run'
		   AND source_id = (SELECT id FROM messages WHERE conversation_id = 'run-ev')`, "trig1")
	var cat string
	var cost float64
	if err := database.QueryRow(
		`SELECT category, total_cost_usd FROM llm_spend WHERE source = 'run'
		   AND source_id = (SELECT id FROM messages WHERE conversation_id = 'run-ev')`,
	).Scan(&cat, &cost); err != nil {
		t.Fatalf("scan llm_spend category: %v", err)
	}
	if cat != "autonomous" {
		t.Errorf("llm_spend category = %q, want autonomous", cat)
	}
	if cost != 1.0 {
		t.Errorf("llm_spend cost = %v, want 1.0 (the historical stamp on the last message)", cost)
	}
}

// assertRunTriggerID runs a single-row trigger_id query and checks it against
// want ("" means SQL NULL is expected).
func assertRunTriggerID(t *testing.T, database *sql.DB, query, want string) {
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

// TestMigrate_StampsHistoricalCostOntoLastMessage pins the refactor's
// historical dollars re-key: a pre-migration run's total_cost_usd lands as
// ONE lump on the conversation's last message row (never the earlier rows),
// and a curator request's cost_usd lands on the turn's last stream row —
// falling back to the turn's user message row when nothing streamed. A row
// with dollars but zero messages loses the stamp by design (accepted
// residue).
func TestMigrate_StampsHistoricalCostOntoLastMessage(t *testing.T) {
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
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
	if err := goose.UpTo(database, "migrations-sqlite", 202607200002); err != nil {
		t.Fatalf("goose UpTo: %v", err)
	}

	seed := []string{
		`INSERT INTO users (id) VALUES ('00000000-0000-0000-0000-000000000100')`,
		`INSERT INTO projects (id, name, visibility) VALUES ('proj1', 'P', 'private')`,
		// A settled run with two streamed rows: the lump must land on the
		// SECOND row only.
		`INSERT INTO runs (id, task_id, prompt_id, blueprint_run_id, status, total_cost_usd)
		 VALUES ('r-cost', 't1', 'p1', 'b1', 'completed', 1.5)`,
		`INSERT INTO run_messages (run_id, role, content) VALUES ('r-cost', 'assistant', 'first')`,
		`INSERT INTO run_messages (run_id, role, content) VALUES ('r-cost', 'assistant', 'last')`,
		// A settled run with no rows: the residue is dropped, not misfiled.
		`INSERT INTO runs (id, task_id, prompt_id, blueprint_run_id, status, total_cost_usd)
		 VALUES ('r-empty', 't1', 'p1', 'b2', 'completed', 9.9)`,
		// A curator turn WITH stream rows: the lump lands on its last stream
		// row, not the user row.
		`INSERT INTO curator_requests (id, project_id, status, user_input, cost_usd)
		 VALUES ('cq-stream', 'proj1', 'done', 'streamed turn', 0.5)`,
		`INSERT INTO curator_messages (request_id, role, content) VALUES ('cq-stream', 'assistant', 'ack one')`,
		`INSERT INTO curator_messages (request_id, role, content) VALUES ('cq-stream', 'assistant', 'ack two')`,
		// A curator turn with NO stream rows: the lump falls back to the
		// user message row.
		`INSERT INTO curator_requests (id, project_id, status, user_input, cost_usd)
		 VALUES ('cq-bare', 'proj1', 'done', 'bare turn', 0.25)`,
	}
	for _, q := range seed {
		if _, err := database.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	assertCost := func(desc, query string, want float64) {
		t.Helper()
		var got sql.NullFloat64
		if err := database.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		if !got.Valid {
			t.Errorf("%s = NULL, want %v", desc, want)
			return
		}
		if got.Float64 != want {
			t.Errorf("%s = %v, want %v", desc, got.Float64, want)
		}
	}
	assertCost("run last-row lump",
		`SELECT cost_usd FROM messages WHERE conversation_id = 'r-cost' AND content = 'last'`, 1.5)
	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE conversation_id = 'r-cost' AND cost_usd IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("count stamped conversation rows: %v", err)
	}
	if n != 1 {
		t.Errorf("stamped conversation rows = %d, want 1 (one lump, no proration)", n)
	}
	assertCost("run total via SUM",
		`SELECT SUM(cost_usd) FROM messages WHERE conversation_id = 'r-cost'`, 1.5)

	assertCost("curator stream lump",
		`SELECT cost_usd FROM messages WHERE claim_id = 'cq-stream' AND content = 'ack two'`, 0.5)
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE claim_id = 'cq-stream' AND cost_usd IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("count stamped curator rows: %v", err)
	}
	if n != 1 {
		t.Errorf("stamped curator rows = %d, want 1", n)
	}
	assertCost("curator user-row fallback",
		`SELECT cost_usd FROM messages WHERE claim_id = 'cq-bare' AND role = 'user'`, 0.25)
}

// TestMigrate_ConvertsPendingSideTablesToUndeliveredMessages pins the
// refactor's pending unification: rows that were waiting in
// run_pending_input / staged_agent_injections when the upgrade ran must
// surface as delivered=0 message rows — the exactly-once delivery contract
// transfers to the messages table, nothing silently drops.
func TestMigrate_ConvertsPendingSideTablesToUndeliveredMessages(t *testing.T) {
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
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

	// Everything before the conversations refactor: the pending side-tables
	// still exist and carry waiting rows.
	if err := goose.UpTo(database, "migrations-sqlite", 202607200002); err != nil {
		t.Fatalf("goose UpTo: %v", err)
	}

	seed := []string{
		`INSERT INTO users (id) VALUES ('u1')`,
		`INSERT INTO runs (id, task_id, prompt_id, blueprint_run_id, status) VALUES ('r1', 't1', 'p1', 'b1', 'open')`,
		`INSERT INTO run_pending_input (run_id, org_id, message, user_id) VALUES ('r1', '00000000-0000-0000-0000-000000000001', 'resume me', 'u1')`,
		`INSERT INTO staged_agent_injections (id, run_id, producer, body) VALUES ('si1', 'r1', 'new_commits', 'PR gained commits')`,
	}
	for _, q := range seed {
		if _, err := database.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	assertUndelivered := func(desc, query string, want int) {
		t.Helper()
		var n int
		if err := database.QueryRow(query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		if n != want {
			t.Errorf("%s = %d, want %d", desc, n, want)
		}
	}
	assertUndelivered("pending follow-up",
		`SELECT COUNT(*) FROM messages WHERE conversation_id='r1' AND role='user' AND content='resume me' AND user_id='u1' AND delivered=0 AND subtype=''`, 1)
	assertUndelivered("staged injection",
		`SELECT COUNT(*) FROM messages WHERE conversation_id='r1' AND subtype='injection:system-note' AND content='PR gained commits' AND delivered=0 AND json_extract(metadata,'$.producer')='new_commits'`, 1)
}

// TestMigrate_CollapsesSetupTransientOntoClaimPhase covers the in-flight-run
// conversion: a claimed run seeded in a runs-era setup transient reads back
// as a 'running' conversation whose ACTIVE claim carries the transient as
// its phase — so the boot sweep still sees claimed work and the display
// coalesce still renders the setup sub-state.
func TestMigrate_CollapsesSetupTransientOntoClaimPhase(t *testing.T) {
	database, err := sql.Open("sqlite", TestDSNMemoryNoForeignKeys)
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

	// Everything before the conversations refactor: runs still carries the
	// flattened claim columns and the granular setup statuses.
	if err := goose.UpTo(database, "migrations-sqlite", 202607200002); err != nil {
		t.Fatalf("goose UpTo: %v", err)
	}

	if _, err := database.Exec(`
		INSERT INTO runs (id, task_id, prompt_id, blueprint_run_id, status,
		                  executor_id, boot_epoch, claimed_at)
		VALUES ('r-mid', 't1', 'p1', 'b1', 'cloning', 'exec-mig', 3, CURRENT_TIMESTAMP)
	`); err != nil {
		t.Fatalf("seed mid-setup run: %v", err)
	}

	if err := goose.Up(database, "migrations-sqlite"); err != nil {
		t.Fatalf("goose Up: %v", err)
	}

	var status sql.NullString
	if err := database.QueryRow(
		`SELECT status FROM conversations WHERE id = 'r-mid'`,
	).Scan(&status); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status.Valid {
		t.Errorf("conversation status = %q, want no stored status (a mid-setup run is mid-flight, and mid-flight is the absence of an outcome)", status.String)
	}

	var (
		phase      string
		executorID string
		released   bool
	)
	if err := database.QueryRow(`
		SELECT COALESCE(phase, ''), executor_id, released_at IS NOT NULL
		FROM claims WHERE conversation_id = 'r-mid'
	`).Scan(&phase, &executorID, &released); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if phase != "cloning" {
		t.Errorf("claim phase = %q, want cloning (the staged transient)", phase)
	}
	if executorID != "exec-mig" {
		t.Errorf("claim executor_id = %q, want exec-mig", executorID)
	}
	if released {
		t.Error("claim released, want active (mid-flight at migration time)")
	}
}
