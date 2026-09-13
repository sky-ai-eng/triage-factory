package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The version a 1.13.3 install has applied, and the consolidated migration
// that follows it. Every test that stages rows the way a released build wrote
// them stops at the first and reads them back after the second.
const (
	beforeTaskContextModel = 202609060002
	taskContextModel       = 202609130001
)

// The `tasks` rebuild the consolidated migration ends on. Two data rules and
// one constraint rule come out of it, and none of them is "UPDATE works".
//
// 'in_review' left the status vocabulary: a row a deployed build parked there
// has to land somewhere the board can still render — 'in_progress', where the
// Jira mirror already put it — and the rebuilt CHECK has to refuse the value
// afterwards, because a CHECK that survived the rebuild as the old six-value
// set would let any write path put it back.
//
// And the queue holds nobody's work: assigning a task is what starts it, so a
// row a released build left queued-and-held has to read in progress,
// unsnoozed, with the rebuilt table refusing that shape from then on.
//
// The rebuild also has to carry the table: every column, every row, and the
// eight indexes — including rederive_owed and its partial index, which arrived
// after the baseline and is exactly the kind of thing a hand-copied DDL drops.
// The FK children are seeded for the same reason the merge migration's test
// seeds them: the swap drops and recreates `tasks`, so a child row that the
// rebuild strands announces itself here rather than at someone's boot.
func TestMigrate_TasksRebuild_RetiresInReviewAndEmptiesTheQueueOfClaims(t *testing.T) {
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
	// Stop at the released schema, so the rows below are staged the way a
	// 1.13.3 install wrote them — 'in_review' still accepted, a claim still
	// writable on a queued row.
	upToErr := goose.UpTo(database, dir, beforeTaskContextModel)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const (
		userID  = "00000000-0000-0000-0000-000000000100"
		agentID = "00000000-0000-0000-0000-000000000200"
	)
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

		// The first of the two rows the queue-holds-no-assignee rule is for: a
		// person's claim on a queued task. A state the released build could
		// write and the rebuilt table cannot.
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, claimed_by_user_id)
			VALUES ('t-held', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'h', 'ev1', 'queued', '` + userID + `')`,

		// One child per FK into tasks(id), so the drop-and-rename is shown to
		// leave them attached rather than orphaned or cascaded away.
		`INSERT INTO orgs (id, slug, name) VALUES ('00000000-0000-0000-0000-000000000001', 'org', 'Org')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', 'team', 'Team')`,

		// The second: a bot's claim on a snoozed task, which also has a wake
		// time an in-progress row cannot keep. The agent it names needs the
		// org above, hence the order.
		`INSERT INTO agents (id, org_id, display_name) VALUES ('` + agentID + `', '00000000-0000-0000-0000-000000000001', 'Bot')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, snooze_until, claimed_by_agent_id)
			VALUES ('t-held-snoozed', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'hs', 'ev1', 'snoozed',
			'2099-01-01 00:00:00', '` + agentID + `')`,
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
		`INSERT INTO workspace_snapshots (org_id, blueprint_run_id, state, writer_claim_id)
			VALUES ('00000000-0000-0000-0000-000000000001', 'br1', 'written', 'claim1')`,
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

	// The held rows: a claim is a stage marker now, so both read in progress —
	// and the snoozed one loses the wake time it can no longer have. Clearing
	// the claim instead would silently unassign work somebody took.
	for _, id := range []string{"t-held", "t-held-snoozed"} {
		if got := statusOf(id); got != "in_progress" {
			t.Errorf("the held row %s reads %q after the upgrade, want in_progress", id, got)
		}
	}
	var (
		heldClaimant string
		heldSnooze   *string
	)
	if err := database.QueryRow(
		`SELECT claimed_by_user_id, snooze_until FROM tasks WHERE id = 't-held'`,
	).Scan(&heldClaimant, &heldSnooze); err != nil {
		t.Fatalf("read the held row: %v", err)
	}
	if heldClaimant != userID {
		t.Errorf("the held row's claimant reads %q, want %q (the fix moves the row, it does not unassign it)", heldClaimant, userID)
	}
	if heldSnooze != nil {
		t.Errorf("the held row's snooze_until reads %v, want NULL", *heldSnooze)
	}
	var snoozedWake *string
	if err := database.QueryRow(
		`SELECT snooze_until FROM tasks WHERE id = 't-held-snoozed'`,
	).Scan(&snoozedWake); err != nil {
		t.Fatalf("read the held snoozed row: %v", err)
	}
	if snoozedWake != nil {
		t.Errorf("the held snoozed row kept a wake time (%v); an in-progress row has none", *snoozedWake)
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

	// And the same for the shape the data fix just cleared: no door can write
	// a queued or snoozed row back onto somebody's name.
	if _, err := database.Exec(
		`UPDATE tasks SET claimed_by_user_id = ? WHERE id = 't-queued'`, userID,
	); err == nil {
		t.Error("claiming a queued task succeeded; tasks_queue_unclaimed must refuse it")
	}
	if _, err := database.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, claimed_by_user_id)
		 VALUES ('t-new-held', 'e1', (SELECT id FROM events_catalog LIMIT 1), 'nh', 'ev1', 'snoozed', ?)`, userID,
	); err == nil {
		t.Error("inserting a held snoozed task succeeded; tasks_queue_unclaimed must refuse it")
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
