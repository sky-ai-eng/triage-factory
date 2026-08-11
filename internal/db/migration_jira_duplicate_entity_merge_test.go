package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that merges the duplicate Jira entity rows the
// canonicalization fold left behind (202608110002). It runs on real deployed
// data, so the test stages rows the way the defect produced them — pre-fold,
// under both migrations — and asserts what came out.
//
// The load-bearing assertions are not "UPDATE works". A row referencing an
// entity that the merge fails to repoint meets one of two fates when the
// variant is deleted, and neither is acceptable: under the four NO ACTION
// foreign keys (events, event_queue, tasks, conversation_memory) the DELETE
// raises and the migration takes the process down at boot; under the three
// CASCADE ones (pending_firings, conversation_memory_entities, entity_links)
// the row is silently destroyed. Only the first kind announces itself, so all
// seven referencing columns are seeded below — the CASCADE ones would
// otherwise leave nothing behind to fail on. (Verified by deleting a repoint
// of each kind and watching this test fail differently for each.)
//
// The second is that a repoint can collide — tasks against the dedup partial
// index, memory links and entity links against their primary keys — and a
// colliding write that raises is, again, a process that will not start.
func TestMigrate_MergesDuplicateJiraEntities(t *testing.T) {
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
	// Stop one version short of the canonicalization fold, so the rows below
	// are staged exactly as builds before it wrote them and BOTH migrations
	// run over them.
	upToErr := goose.UpTo(database, dir, 202608080001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}
	// events_catalog is seeded by Migrate, not by goose, and the seed chain
	// below FKs into it through events / tasks / event_handlers.
	if err := SeedEventTypes(database, "sqlite3"); err != nil {
		t.Fatalf("seed event types: %v", err)
	}

	const userID = "00000000-0000-0000-0000-000000000100"
	seed := []string{
		`INSERT INTO users (id) VALUES ('` + userID + `')`,

		// The healthy twin and its variant: one Jira issue tracked twice,
		// once under each spelling. SKY-2 carries the live tracking.
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('surv', 'jira', 'SKY-2', 'issue', 'canonical')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('var',  'jira', 'sky-2', 'issue', 'variant')`,
		// Two variants of one key with no canonical row at all. The fold
		// elects the lowest id ('dupe-a' → SKY-4) and leaves 'dupe-b'; the
		// merge then folds 'dupe-b' into the row the fold just minted.
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('dupe-a', 'jira', 'sky-4', 'issue', 'elected')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('dupe-b', 'jira', 'Sky-4', 'issue', 'loser')`,
		// A GitHub PR to link against, and to prove the merge stays inside
		// source='jira'.
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('gh', 'github', 'owner/repo#1', 'pr', 'a pr')`,

		// events, on both rows.
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev-surv', 'surv', (SELECT id FROM events_catalog LIMIT 1))`,
		`INSERT INTO events (id, entity_id, event_type) VALUES ('ev-var',  'var',  (SELECT id FROM events_catalog LIMIT 1))`,

		// tasks. 't-move' has a slot nothing else holds, so it moves and
		// stays active. 't-collide' duplicates 't-rival' on the survivor —
		// same (event_type, dedup_key) — so it moves and closes.
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-rival', 'surv', (SELECT id FROM events_catalog LIMIT 1), 'dup', 'ev-surv', 'queued')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-collide', 'var', (SELECT id FROM events_catalog LIMIT 1), 'dup', 'ev-var', 'queued')`,
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status)
			VALUES ('t-move', 'var', (SELECT id FROM events_catalog LIMIT 1), 'solo', 'ev-var', 'queued')`,
		// A terminal task on the variant: outside the dedup partial index, so
		// it moves under the same slot as the active rival without colliding,
		// and keeps the outcome it was closed with.
		`INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id, status, close_reason)
			VALUES ('t-done', 'var', (SELECT id FROM events_catalog LIMIT 1), 'dup', 'ev-var', 'done', 'run_completed')`,

		// event_queue + pending_firings: the two remaining FK columns.
		`INSERT INTO event_queue (event_id, entity_id, event_type)
			VALUES ('ev-var', 'var', (SELECT id FROM events_catalog LIMIT 1))`,
		`INSERT INTO blueprints (id, name, creator_user_id) VALUES ('bp1', 'BP', '` + userID + `')`,
		`INSERT INTO event_handlers (id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
			VALUES ('trig1', '` + userID + `', 'trigger', (SELECT id FROM events_catalog LIMIT 1), 'bp1', 3, 0.5)`,
		`INSERT INTO pending_firings (entity_id, task_id, trigger_id, triggering_event_id)
			VALUES ('var', 't-move', 'trig1', 'ev-var')`,

		// conversation_memory / conversation_memory_entities. One conversation
		// touched BOTH spellings, so its entity link collides on
		// (conversation_id, entity_id); a second touched only the variant and
		// moves cleanly.
		`INSERT INTO prompts (id, name, body, creator_user_id) VALUES ('p1', 'P', 'b', '` + userID + `')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br1', 'bp1', 't-move', '[]', '')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-both', 't-move', 'p1', 'br1', 'completed')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-var',  't-move', 'p1', 'br1', 'completed')`,
		`INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content) VALUES ('mem-var', 'conv-var', 'var', 'what I learned')`,
		`INSERT INTO conversation_memory_entities (conversation_id, entity_id, role) VALUES ('conv-both', 'surv', 'primary')`,
		`INSERT INTO conversation_memory_entities (conversation_id, entity_id, role) VALUES ('conv-both', 'var',  'touched')`,
		`INSERT INTO conversation_memory_entities (conversation_id, entity_id, role) VALUES ('conv-var',  'var',  'touched')`,

		// In-flight work, for the cancel intent a forced close owes. Four
		// blueprints, one per arm of CloseWithRunCancelIntentSystem's
		// predicate: running under a live conversation on the task the merge
		// force-closes (must be called off); the same on the task that
		// survives active (must NOT be — that work is still on the board);
		// one that already finished; and one still running whose conversation
		// has ended. The two live conversations take the two arms of the
		// non-terminal test, NULL and a non-terminal value.
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br-collide', 'bp1', 't-collide', '[]', '')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-live-collide', 't-collide', 'p1', 'br-collide', NULL)`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br-move', 'bp1', 't-move', '[]', '')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-live-move', 't-move', 'p1', 'br-move', 'running')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, status, step_plan, worktree_path)
			VALUES ('br-finished', 'bp1', 't-collide', 'completed', '[]', '')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-finished', 't-collide', 'p1', 'br-finished', 'running')`,
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, step_plan, worktree_path)
			VALUES ('br-ended', 'bp1', 't-collide', '[]', '')`,
		`INSERT INTO conversations (id, task_id, prompt_id, blueprint_run_id, status)
			VALUES ('conv-ended', 't-collide', 'p1', 'br-ended', 'completed')`,

		// entity_links, in all four shapes the merge has to tell apart: one
		// that survives repointed, one that collapses to a self-link (the
		// variant linked to its own twin), one that lands on a link the
		// survivor already has, and one whose BOTH endpoints are variants of
		// different keys — the case the single two-column UPDATE exists for.
		`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES ('var', 'gh', 'relates', 'agent')`,
		`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES ('var', 'surv', 'relates', 'agent')`,
		`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES ('surv', 'gh', 'blocks', 'agent')`,
		`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES ('var',  'gh', 'blocks', 'agent')`,
		`INSERT INTO entity_links (from_entity_id, to_entity_id, kind, origin) VALUES ('var', 'dupe-b', 'duplicates', 'agent')`,
	}
	for _, stmt := range seed {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Both migrations, in order — the fold, then the merge.
	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := database.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	scalar := func(query string, args ...any) string {
		t.Helper()
		var got sql.NullString
		if err := database.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return got.String
	}

	// The variants are gone and their canonical twins are the only rows left
	// for those two issues. 'dupe-a' is the row the fold renamed to SKY-4,
	// which is what 'dupe-b' merged into.
	if n := count(`SELECT COUNT(*) FROM entities WHERE id IN ('var','dupe-b')`); n != 0 {
		t.Errorf("variant entity rows remaining = %d, want 0 — each can never match its own issue in a refresh", n)
	}
	if n := count(`SELECT COUNT(*) FROM entities WHERE source = 'jira'`); n != 2 {
		t.Errorf("jira entity rows = %d, want 2 (SKY-2, SKY-4)", n)
	}
	if got := scalar(`SELECT source_id FROM entities WHERE id = 'dupe-a'`); got != "SKY-4" {
		t.Errorf("elected variant = %q, want SKY-4 — the row 'dupe-b' merges into", got)
	}
	if got := scalar(`SELECT title FROM entities WHERE id = 'surv'`); got != "canonical" {
		t.Errorf("survivor title = %q, want it untouched — the survivor is authoritative for its own columns", got)
	}
	// The GitHub row is never a candidate: its source_id is a case-sensitive
	// natural key, and only Jira resolves keys case-insensitively.
	if n := count(`SELECT COUNT(*) FROM entities WHERE id = 'gh'`); n != 1 {
		t.Errorf("github entity rows = %d, want 1 — the merge is scoped to source='jira'", n)
	}

	// Every referent moved to the survivor.
	if got := scalar(`SELECT entity_id FROM events WHERE id = 'ev-var'`); got != "surv" {
		t.Errorf("event entity_id = %q, want surv — the log moves rather than being discarded", got)
	}
	if got := scalar(`SELECT entity_id FROM event_queue WHERE event_id = 'ev-var'`); got != "surv" {
		t.Errorf("queued event entity_id = %q, want surv", got)
	}
	if got := scalar(`SELECT entity_id FROM pending_firings WHERE task_id = 't-move'`); got != "surv" {
		t.Errorf("pending firing entity_id = %q, want surv", got)
	}
	if got := scalar(`SELECT entity_id FROM conversation_memory WHERE id = 'mem-var'`); got != "surv" {
		t.Errorf("memory entity_id = %q, want surv", got)
	}

	// tasks: the non-colliding one moves and stays on the board, the
	// colliding one moves and closes as the duplicate it is, and the rival
	// that was already on the survivor is untouched.
	if got := scalar(`SELECT entity_id FROM tasks WHERE id = 't-move'`); got != "surv" {
		t.Errorf("non-colliding task entity_id = %q, want surv", got)
	}
	if got := scalar(`SELECT status FROM tasks WHERE id = 't-move'`); got != "queued" {
		t.Errorf("non-colliding task status = %q, want queued — a merge is not a reason to close work", got)
	}
	if got := scalar(`SELECT entity_id FROM tasks WHERE id = 't-collide'`); got != "surv" {
		t.Errorf("colliding task entity_id = %q, want surv", got)
	}
	if got := scalar(`SELECT status FROM tasks WHERE id = 't-collide'`); got != "dismissed" {
		t.Errorf("colliding task status = %q, want dismissed — one active task per (entity, event_type, dedup_key)", got)
	}
	if got := scalar(`SELECT close_reason FROM tasks WHERE id = 't-collide'`); got != "duplicate_entity_merged" {
		t.Errorf("colliding task close_reason = %q, want duplicate_entity_merged", got)
	}
	if got := scalar(`SELECT closed_at FROM tasks WHERE id = 't-collide'`); got == "" {
		t.Error("colliding task closed_at is NULL, want a timestamp — a closed task carries when it closed")
	}
	if got := scalar(`SELECT status FROM tasks WHERE id = 't-rival'`); got != "queued" {
		t.Errorf("survivor-side task status = %q, want queued — the live card is the one that stays", got)
	}
	// A forced close owes the same cancel intent the application's own close
	// path stamps: an agent still running under the closed card is stopped,
	// not orphaned. Everything the app's guard excludes stays excluded here.
	cancelOf := func(blueprintRunID string) int {
		t.Helper()
		return count(`SELECT cancel_requested FROM blueprint_runs WHERE id = ?`, blueprintRunID)
	}
	if got := cancelOf("br-collide"); got != 1 {
		t.Errorf("cancel_requested on the force-closed task's live blueprint = %d, want 1 — a closed card must not leave an agent running", got)
	}
	if got := cancelOf("br-move"); got != 0 {
		t.Errorf("cancel_requested on the surviving task's live blueprint = %d, want 0 — that work is still on the board", got)
	}
	if got := cancelOf("br-finished"); got != 0 {
		t.Errorf("cancel_requested on an already-finished blueprint = %d, want 0 — a close cannot call off what already ended", got)
	}
	if got := cancelOf("br-ended"); got != 0 {
		t.Errorf("cancel_requested on a blueprint whose conversation ended = %d, want 0 — no live work to stop", got)
	}
	// The terminal task keeps its own outcome: it was never in the partial
	// index, so it moved on the first pass and the duplicate-close never
	// reached it.
	if got := scalar(`SELECT status FROM tasks WHERE id = 't-done'`); got != "done" {
		t.Errorf("terminal task status = %q, want done — a completed task is not re-closed by a merge", got)
	}
	if got := scalar(`SELECT close_reason FROM tasks WHERE id = 't-done'`); got != "run_completed" {
		t.Errorf("terminal task close_reason = %q, want run_completed", got)
	}
	if got := scalar(`SELECT entity_id FROM tasks WHERE id = 't-done'`); got != "surv" {
		t.Errorf("terminal task entity_id = %q, want surv", got)
	}
	// Exactly one active task holds the contested slot afterwards — the
	// invariant the partial index exists to keep.
	if n := count(`SELECT COUNT(*) FROM tasks WHERE entity_id = 'surv' AND dedup_key = 'dup' AND status NOT IN ('done','dismissed')`); n != 1 {
		t.Errorf("active tasks in the contested dedup slot = %d, want 1", n)
	}

	// Memory links: the conversation that touched both spellings keeps one
	// link to the survivor, the one that touched only the variant is moved.
	if n := count(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = 'conv-both'`); n != 1 {
		t.Errorf("memory links for the both-spellings conversation = %d, want 1 (deduped)", n)
	}
	if got := scalar(`SELECT role FROM conversation_memory_entities WHERE conversation_id = 'conv-both'`); got != "primary" {
		t.Errorf("surviving memory link role = %q, want primary — the survivor's link wins", got)
	}
	if got := scalar(`SELECT entity_id FROM conversation_memory_entities WHERE conversation_id = 'conv-var'`); got != "surv" {
		t.Errorf("moved memory link entity_id = %q, want surv", got)
	}

	// entity_links: the unique link is repointed, the variant→survivor link
	// is dropped rather than kept as a self-link, and the duplicate that
	// lands on an existing link is dropped.
	if n := count(`SELECT COUNT(*) FROM entity_links WHERE from_entity_id = 'surv' AND to_entity_id = 'gh' AND kind = 'relates'`); n != 1 {
		t.Errorf("repointed link count = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM entity_links WHERE from_entity_id = to_entity_id`); n != 0 {
		t.Errorf("self-links = %d, want 0 — an issue does not relate to itself", n)
	}
	if n := count(`SELECT COUNT(*) FROM entity_links WHERE kind = 'blocks'`); n != 1 {
		t.Errorf("duplicate 'blocks' links = %d, want 1 — the variant's copy is redundant", n)
	}
	// A link whose two ends are both variants resolves on both sides in the
	// one statement, rather than half-moving.
	if n := count(`SELECT COUNT(*) FROM entity_links WHERE kind = 'duplicates' AND from_entity_id = 'surv' AND to_entity_id = 'dupe-a'`); n != 1 {
		t.Errorf("both-ends-variant link resolved to (surv, dupe-a) = %d rows, want 1", n)
	}

	// The staging table is not left behind for the app to trip over.
	if n := count(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'jira_entity_merge_src'`); n != 0 {
		t.Errorf("staging table rows in sqlite_master = %d, want 0 — it is dropped by the migration", n)
	}
}

// A fresh install has no duplicates to merge, which is the case almost every
// deployment is in. The migration must be a no-op there — not a failure, and
// not a rewrite of the healthy rows next to it.
func TestMigrate_DuplicateJiraEntityMergeIsNoOpWithoutDuplicates(t *testing.T) {
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
	upToErr := goose.UpTo(database, dir, 202608080001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	for _, stmt := range []string{
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('a', 'jira', 'SKY-1', 'issue', 'healthy')`,
		// A lone non-canonical row with no twin: the fold repairs it, and the
		// merge must leave the repaired row alone rather than treating it as
		// a duplicate of itself.
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('b', 'jira', 'sky-9', 'issue', 'folded')`,
		`INSERT INTO entities (id, source, source_id, kind, title) VALUES ('c', 'github', 'owner/repo#1', 'pr', 'pr')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM entities`).Scan(&n); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if n != 3 {
		t.Errorf("entities = %d, want 3 — nothing to merge, nothing deleted", n)
	}
	var key string
	if err := database.QueryRow(`SELECT source_id FROM entities WHERE id = 'b'`).Scan(&key); err != nil {
		t.Fatalf("read folded row: %v", err)
	}
	if key != "SKY-9" {
		t.Errorf("folded row = %q, want SKY-9", key)
	}
}
