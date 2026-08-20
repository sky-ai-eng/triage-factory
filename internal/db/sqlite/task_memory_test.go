package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskMemoryStore_SQLite runs the shared conformance suite
// against the SQLite TaskMemoryStore impl. Each subtest opens a
// fresh in-memory DB so conversation_memory state doesn't leak between
// assertions.
func TestTaskMemoryStore_SQLite(t *testing.T) {
	dbtest.RunTaskMemoryStoreConformance(t, func(t *testing.T) (db.TaskMemoryStore, string, dbtest.TaskMemorySeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.TaskMemorySeeder{
			Conversation: func(t *testing.T, suffix string) (conversationID, entityID string) {
				t.Helper()
				return seedSQLiteConversationForTaskMemory(t, conn, suffix)
			},
			BlueprintRun: func(t *testing.T, suffix string) (blueprintRunID string) {
				t.Helper()
				return seedSQLiteBlueprintRunForTaskMemory(t, conn, suffix)
			},
			Role: func(t *testing.T, conversationID, entityID string) string {
				t.Helper()
				return roleForSQLiteJoinRow(t, conn, conversationID, entityID)
			},
		}
		return stores.TaskMemory, runmode.LocalDefaultOrgID, seed
	})
}

// TestTaskMemoryStore_SQLite_ReturnedRowConformance runs the returned-row
// suite (TFAC-867) against the SQLite impl. SQLite has one connection and no
// RLS, so there is no separate app-pool wiring the way the Postgres suite
// needs — this single run covers both the plain and System variants.
func TestTaskMemoryStore_SQLite_ReturnedRowConformance(t *testing.T) {
	dbtest.RunTaskMemoryReturnedRowConformance(t, func(t *testing.T) (db.TaskMemoryStore, string, dbtest.TaskMemorySeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.TaskMemorySeeder{
			Conversation: func(t *testing.T, suffix string) (conversationID, entityID string) {
				t.Helper()
				return seedSQLiteConversationForTaskMemory(t, conn, suffix)
			},
			BlueprintRun: func(t *testing.T, suffix string) (blueprintRunID string) {
				t.Helper()
				return seedSQLiteBlueprintRunForTaskMemory(t, conn, suffix)
			},
			Role: func(t *testing.T, conversationID, entityID string) string {
				t.Helper()
				return roleForSQLiteJoinRow(t, conn, conversationID, entityID)
			},
		}
		return stores.TaskMemory, runmode.LocalDefaultOrgID, seed
	})
}

// roleForSQLiteJoinRow reads back conversation_memory_entities.role directly — the
// store interface has no role-returning read, so the
// RecordEntityTouchSystem precedence conformance subtest needs a raw
// escape hatch. Returns "" if no row exists.
func roleForSQLiteJoinRow(t *testing.T, conn *sql.DB, conversationID, entityID string) string {
	t.Helper()
	var role string
	err := conn.QueryRow(`SELECT role FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ?`, conversationID, entityID).Scan(&role)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read conversation_memory_entities role: %v", err)
	}
	return role
}

// TestTaskMemoryStore_SQLite_RejectsNonLocalOrg pins the
// assertLocalOrg gate at every method entry. Non-default org IDs
// indicate a confused caller (they think they're in multi mode) and
// must be rejected loudly so the silent default-org fallthrough
// never happens.
func TestTaskMemoryStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const badOrg = "11111111-1111-1111-1111-111111111111"
	if _, err := stores.TaskMemory.UpsertAgentMemory(ctx, badOrg, "r", "e", "", "x"); err == nil {
		t.Error("UpsertAgentMemory(non-local org) should error")
	}
	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, badOrg, "r", "e", "", "x"); err == nil {
		t.Error("UpsertAgentMemorySystem(non-local org) should error")
	}
	if _, err := stores.TaskMemory.UpdateConversationMemoryHumanContent(ctx, badOrg, "r", "x"); err == nil {
		t.Error("UpdateConversationMemoryHumanContent(non-local org) should error")
	}
	if _, err := stores.TaskMemory.GetMemoriesForEntity(ctx, badOrg, "e"); err == nil {
		t.Error("GetMemoriesForEntity(non-local org) should error")
	}
	if _, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, badOrg, "e", runmode.LocalDefaultTeamID); err == nil {
		t.Error("GetMemoriesForEntitySystem(non-local org) should error")
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, badOrg, "r", "e", domain.MemoryRolePrimary); err == nil {
		t.Error("RecordEntityTouchSystem(non-local org) should error")
	}
	if _, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, badOrg, "e", runmode.LocalDefaultTeamID); err == nil {
		t.Error("CountMemoriesForEntitySystem(non-local org) should error")
	}
}

// TestTaskMemoryStore_SQLite_CountMemoriesForEntitySystem pins the basic
// counting contract (SQLite ignores teamID — N=1, one team, so there is
// no cross-team memory to scope out; see GetMemoriesForEntitySystem's own
// doc comment for the parity rationale).
func TestTaskMemoryStore_SQLite_CountMemoriesForEntitySystem(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	conv1, entityID := seedSQLiteConversationForTaskMemory(t, conn, "count-1")
	if _, err := stores.TaskMemory.UpsertAgentMemory(ctx, runmode.LocalDefaultOrgID, conv1, entityID, "", "first"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, runmode.LocalDefaultOrgID, conv1, entityID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("RecordEntityTouchSystem: %v", err)
	}

	if n, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("CountMemoriesForEntitySystem: %v", err)
	} else if n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}

	conv2, _ := seedSQLiteConversationForTaskMemory(t, conn, "count-2")
	if _, err := stores.TaskMemory.UpsertAgentMemory(ctx, runmode.LocalDefaultOrgID, conv2, entityID, "", "second"); err != nil {
		t.Fatalf("UpsertAgentMemory: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, runmode.LocalDefaultOrgID, conv2, entityID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("RecordEntityTouchSystem: %v", err)
	}

	if n, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("CountMemoriesForEntitySystem: %v", err)
	} else if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}

	// Unrelated entity has none.
	_, otherEntity := seedSQLiteConversationForTaskMemory(t, conn, "count-unrelated")
	if n, err := stores.TaskMemory.CountMemoriesForEntitySystem(ctx, runmode.LocalDefaultOrgID, otherEntity, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("CountMemoriesForEntitySystem: %v", err)
	} else if n != 0 {
		t.Errorf("Count for unrelated entity = %d, want 0", n)
	}
}

// TestTaskMemoryStore_SQLite_BackfillProducesPrimaryJoinRows pins the
// migration's backfill statement directly: a conversation_memory row inserted
// with NO corresponding conversation_memory_entities row (simulating a
// pre-migration row) gets exactly one 'primary' join row once the
// backfill INSERT runs, and the entity-scoped read then returns it —
// the read-path switch's result-identical claim for pre-existing data.
func TestTaskMemoryStore_SQLite_BackfillProducesPrimaryJoinRows(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	conversationID, entityID := seedSQLiteConversationForTaskMemory(t, conn, "backfill")
	now := time.Now().UTC()
	if _, err := conn.Exec(
		`INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content, created_at) VALUES (?, ?, ?, 'pre-migration note', ?)`,
		uuid.New().String(), conversationID, entityID, now,
	); err != nil {
		t.Fatalf("seed pre-migration conversation_memory row: %v", err)
	}

	// No join row exists yet — the read path finds nothing.
	if mems, err := stores.TaskMemory.GetMemoriesForEntity(ctx, runmode.LocalDefaultOrgID, entityID); err != nil {
		t.Fatalf("GetMemoriesForEntity (pre-backfill): %v", err)
	} else if len(mems) != 0 {
		t.Fatalf("GetMemoriesForEntity (pre-backfill) = %+v, want empty", mems)
	}

	// The exact backfill statement from the migration.
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO conversation_memory_entities (org_id, conversation_id, entity_id, role, created_at)
		SELECT org_id, conversation_id, entity_id, 'primary', created_at FROM conversation_memory
	`); err != nil {
		t.Fatalf("run backfill: %v", err)
	}

	if role := roleForSQLiteJoinRow(t, conn, conversationID, entityID); role != domain.MemoryRolePrimary {
		t.Errorf("backfilled role = %q, want %q", role, domain.MemoryRolePrimary)
	}

	mems, err := stores.TaskMemory.GetMemoriesForEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntity (post-backfill): %v", err)
	}
	if len(mems) != 1 || mems[0].Content != "pre-migration note" {
		t.Errorf("GetMemoriesForEntity (post-backfill) = %+v, want the pre-migration row", mems)
	}
}

// seedSQLiteConversationForTaskMemory seeds the entity + event + prompt +
// task + conversation FK chain conversation_memory needs. Direct INSERTs
// keep the fixture path schema-coupled and short — matches the SwipeStore
// / EventStore conformance seed pattern.
func seedSQLiteConversationForTaskMemory(t *testing.T, conn *sql.DB, suffix string) (conversationID, entityID string) {
	t.Helper()
	entityID = uuid.New().String()
	now := time.Now().UTC()
	sourceID := fmt.Sprintf("task-memory-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'github', ?, 'pr', 'Task Memory Conformance', 'https://example/x', '{}', ?, 'active')
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key)
		VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_task_memory', ?, 'body', ?, ?)
	`, dbtest.TaskMemorySeedPromptName, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	taskID := uuid.New().String()
	// 'done' is the terminal task status (an earlier version of the seed used
	// 'completed', which was never a valid task value — 'completed'
	// is conversation-level; a later CHECK constraint caught the latent bug).
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'done')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// conversations.blueprint_run_id is NOT NULL — mint a blueprint + blueprint_run
	// for this task so the conversation row satisfies the FK.
	blueprintRunID := seedBlueprintRunForConversation(t, conn, taskID)
	conversationID = uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, 'p_task_memory', 'completed', ?, ?)
	`, conversationID, taskID, blueprintRunID, dbtest.TaskMemorySeedStepIndex); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return conversationID, entityID
}

// seedSQLiteBlueprintRunForTaskMemory seeds the entity + event + task +
// blueprint + blueprint_run FK chain a conversation_memory.blueprint_run_id can point
// at (the column FKs blueprint_runs(id) ON DELETE SET NULL). Returns the
// blueprint_run id. Independent of seedSQLiteConversationForTaskMemory's
// conversation — the round-trip test only needs a valid FK target.
func seedSQLiteBlueprintRunForTaskMemory(t *testing.T, conn *sql.DB, suffix string) (blueprintRunID string) {
	t.Helper()
	now := time.Now().UTC()
	entityID := uuid.New().String()
	sourceID := fmt.Sprintf("bp-run-%s-%d", suffix, now.UnixNano())
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at, state)
		VALUES (?, 'github', ?, 'pr', 'Blueprint Run Conformance', 'https://example/bp', '{}', ?, 'active')
	`, entityID, sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	const eventType = "github:pr:opened"
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')
	`, eventID, entityID, eventType); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'done')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	blueprintID := "bp_" + uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprints (id, name, source, creator_user_id, team_id) VALUES (?, 'BP', 'user', ?, ?)
	`, blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	blueprintRunID = uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', ?, '[]')
	`, blueprintRunID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return blueprintRunID
}
