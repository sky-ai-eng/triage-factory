package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that retires 'in_review' from the task status vocabulary
// (202609120008). Two things have to come out of it and neither is "UPDATE
// works": a row a deployed build left in the retired status has to land
// somewhere the board can still render — 'in_progress', where the Jira mirror
// already put it — and the rebuilt table has to refuse the value afterwards,
// because a CHECK that survived the rebuild as the old six-value set would
// let any write path put the column back.
//
// The rebuild also has to carry the table: every column, every row, and the
// eight indexes — including rederive_owed and its partial index, which arrived
// after the baseline and is exactly the kind of thing a hand-copied DDL drops.
// The FK children are seeded for the same reason the merge migration's test
// seeds them: the swap drops and recreates `tasks`, so a child row that the
// rebuild strands announces itself here rather than at someone's boot.
func TestMigrate_DropsInReviewFromTheTaskStatusVocabulary(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// Stop one version short, so the rows below are staged by a schema that
	// still accepts 'in_review' — which is the only state this migration is
	// about.
	upToErr := goose.UpTo(database, dir, 202609120007)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const userID = "00000000-0000-0000-0000-000000000100"
	seed := []string{
		`INSERT INTO users (id) VALUES ('` + userID + `')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('e1', 'github', 'owner/repo#1', 'pr', 'a pr')`,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev1', 'e1', (SELECT id FROM events_catalog LIMIT 1))`,

		// The row the migration is for: user-claimed and parked in the
		// retired status, carrying enough of the rest of the table that a
		// column the rebuild forgot to copy shows up as a lost value.
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status,
			priority_score, ai_summary, scoring_status, claimed_by_user_id, rederive_owed)
			VALUES ('t-review', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'ir', 'ev1', 'in_review',
			0.75, 'the summary', 'scored', '` + userID + `', 1)`,
		// The statuses that stay, so the rewrite is shown to be targeted
		// rather than a blanket write over the board.
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-queued', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'q', 'ev1', 'queued')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, close_reason)
			VALUES ('t-done', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'd', 'ev1', 'done', 'user_completed')`,

		// One child per FK into tasks(id), so the drop-and-rename is shown to
		// leave them attached rather than orphaned or cascaded away.
		`INSERT INTO orgs (id, slug, name) VALUES ('00000000-0000-0000-0000-000000000001', 'org', 'Org')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', 'team', 'Team')`,
		`INSERT INTO task_teams (task_id, team_id) VALUES ('t-review', '00000000-0000-0000-0000-000000000010')`,
		`INSERT INTO task_events (task_id, event_id, kind) VALUES ('t-review', 'ev1', 'primary')`,
		`INSERT INTO swipe_events (task_id, action) VALUES ('t-review', 'claim')`,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'BP', '` + userID + `')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br1', 'bp1', 't-review', '[]', '')`,
		`INSERT INTO prompts (id, name, body, creator_user_id) VALUES ('p1', 'P', 'b', '` + userID + `')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv1', 't-review', 'p1', 'br1', 'completed')`,
		`INSERT INTO event_handlers (id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES ('trig1', '` + userID + `', 'trigger', (SELECT id FROM events_catalog LIMIT 1), 'bp1', 3, 0.5)`,
		`INSERT INTO pending_firings (entity_id, task_id, trigger_id, triggering_event_id)
			VALUES ('e1', 't-review', 'trig1', 'ev1')`,
		`INSERT INTO workspace_snapshots (org_id, task_id, state, writer_claim_id)
			VALUES ('00000000-0000-0000-0000-000000000001', 't-review', 'written', 'claim1')`,
	}
	for _, stmt := range seed {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	statusOf := func(id string) string {
		t.Helper()
		var s string
		if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("read status of %s: %v", id, err)
		}
		return s
	}
	if got := statusOf("t-review"); got != "in_progress" {
		t.Errorf("the in_review row reads %q after the upgrade, want in_progress", got)
	}
	if got := statusOf("t-queued"); got != "queued" {
		t.Errorf("the queued row reads %q, want queued (the rewrite is targeted)", got)
	}
	if got := statusOf("t-done"); got != "done" {
		t.Errorf("the done row reads %q, want done (the rewrite is targeted)", got)
	}

	// Every other column of the rewritten row came across the rebuild.
	var (
		priority  float64
		summary   string
		scoring   string
		claimedBy string
		rederive  bool
		closeReas *string
	)
	if err := database.QueryRow(`
		SELECT priority_score, ai_summary, scoring_status, claimed_by_user_id, rederive_owed, close_reason
		  FROM tasks WHERE id = 't-review'`,
	).Scan(&priority, &summary, &scoring, &claimedBy, &rederive, &closeReas); err != nil {
		t.Fatalf("read the rebuilt row: %v", err)
	}
	if priority != 0.75 || summary != "the summary" || scoring != "scored" || claimedBy != userID || !rederive || closeReas != nil {
		t.Errorf("rebuilt row = {priority %v, summary %q, scoring %q, claimed_by %q, rederive_owed %v, close_reason %v}; the rebuild lost a column",
			priority, summary, scoring, claimedBy, rederive, closeReas)
	}

	// The CHECK is what makes the value unwritable rather than merely
	// unwritten — every guarded helper sits above it, and SetStatus doesn't.
	if _, err := database.Exec(`UPDATE tasks SET status = 'in_review' WHERE id = 't-queued'`); err == nil {
		t.Error("UPDATE to in_review succeeded; the rebuilt CHECK must refuse the retired value")
	}
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
		VALUES ('t-new', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'new', 'ev1', 'in_review')`,
	); err == nil {
		t.Error("INSERT with in_review succeeded; the rebuilt CHECK must refuse the retired value")
	}

	// The eight indexes the dropped table carried are back — an index lost in
	// a rebuild is invisible until a lane gets slow or a dedup race lands two
	// tasks for one situation.
	wantIndexes := []string{
		"idx_tasks_active_entity_event_dedup",
		"idx_tasks_entity",
		"idx_tasks_rederive_owed",
		"idx_tasks_status",
		"idx_tasks_status_priority",
		"tasks_claimed_agent_idx",
		"tasks_claimed_user_idx",
		"tasks_id_org_unique",
	}
	for _, name := range wantIndexes {
		var n int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'tasks' AND name = ?`, name,
		).Scan(&n); err != nil {
			t.Fatalf("look up index %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("index %s is missing after the rebuild", name)
		}
	}

	// The dedup partial index still holds: two active tasks can't share a
	// (entity, event_type, dedup_key) slot.
	if _, err := database.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
		VALUES ('t-dupe', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'q', 'ev1', 'queued')`,
	); err == nil {
		t.Error("a second active task landed in the 'q' dedup slot; the unique partial index did not survive the rebuild")
	}

	// The children are still attached to the row they were seeded against.
	for _, q := range []string{
		`SELECT COUNT(*) FROM task_teams WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM task_events WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM swipe_events WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM conversations WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM pending_firings WHERE task_id = 't-review'`,
		`SELECT COUNT(*) FROM workspace_snapshots WHERE task_id = 't-review'`,
	} {
		var n int
		if err := database.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 1 {
			t.Errorf("%s = %d, want 1 (the rebuild stranded a child row)", q, n)
		}
	}
}
