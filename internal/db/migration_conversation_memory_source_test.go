package db

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that gives conversation_memory a `source` and takes away
// `entity_id` and `human_content` (202609120001). It runs on real deployed
// data, where the only thing a NULL agent_content row records is that the
// agent wrote no usable memory file — so it carries across as 'none', and
// anything with content as 'agent'.
//
// The rebuild is the risk this pins: SQLite cannot drop a column a FK
// references, so the table is recreated and every row copied. A copy that
// silently dropped rows, mismatched columns, or fired the children's ON DELETE
// CASCADE on the drop would leave a memory constellation with holes in it —
// hence the assertions on both surviving rows AND on conversation_memory_entities
// being untouched.
func TestMigrate_ConversationMemoryGainsSourceAndDropsEntityAndHumanContent(t *testing.T) {
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
	// Stop one version short, so the rows below are staged the way a deployed
	// build wrote them — with entity_id and human_content still present.
	upToErr := goose.UpTo(database, dir, 202609060002)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	const (
		orgID    = "00000000-0000-0000-0000-000000000001"
		teamID   = "00000000-0000-0000-0000-000000000010"
		userID   = "00000000-0000-0000-0000-000000000100"
		entityID = "e-mem-source"
	)
	for _, stmt := range []string{
		`INSERT INTO orgs (id, slug, name) VALUES ('` + orgID + `', 'local', 'Local')`,
		`INSERT INTO teams (id, org_id, slug, name) VALUES ('` + teamID + `', '` + orgID + `', 'default', 'Default')`,
		`INSERT INTO users (id, display_name) VALUES ('` + userID + `', 'Local')`,
		`INSERT INTO entities (id, source, source_id, kind, state) VALUES ('` + entityID + `', 'github', 'owner/repo#1', 'pr', 'active')`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// Two memory rows on one entity: one the agent wrote, one it did not. Each
	// carries the human_content the five deleted writers used to compose, and
	// the primary join row every memory has carried since the backfill.
	seed := func(conversationID, agentContent string) {
		t.Helper()
		if _, err := database.Exec(
			`INSERT INTO conversations (id, org_id, team_id, origin, status) VALUES (?, ?, ?, 'interactive', 'completed')`,
			conversationID, orgID, teamID,
		); err != nil {
			t.Fatalf("seed conversation %s: %v", conversationID, err)
		}
		var content any
		if agentContent != "" {
			content = agentContent
		}
		if _, err := database.Exec(
			`INSERT INTO conversation_memory (id, conversation_id, entity_id, agent_content, human_content, created_at)
			 VALUES (?, ?, ?, ?, 'a machine-composed verdict', '2026-09-01 00:00:00')`,
			"mem-"+conversationID, conversationID, entityID, content,
		); err != nil {
			t.Fatalf("seed memory %s: %v", conversationID, err)
		}
		if _, err := database.Exec(
			`INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role) VALUES (?, ?, ?, 'primary')`,
			orgID, conversationID, entityID,
		); err != nil {
			t.Fatalf("seed join row %s: %v", conversationID, err)
		}
	}
	seed("conv-wrote", "what I tried and why")
	seed("conv-silent", "")

	gooseMu.Lock()
	goose.SetBaseFS(treeFS)
	upErr := goose.SetDialect("sqlite3")
	if upErr == nil {
		upErr = goose.UpTo(database, dir, 202609120001)
	}
	gooseMu.Unlock()
	if upErr != nil {
		t.Fatalf("goose.UpTo conversation_memory source: %v", upErr)
	}

	read := func(conversationID string) (content sql.NullString, source string) {
		t.Helper()
		if err := database.QueryRow(
			`SELECT agent_content, source FROM conversation_memory WHERE conversation_id = ?`, conversationID,
		).Scan(&content, &source); err != nil {
			t.Fatalf("read %s: %v", conversationID, err)
		}
		return content, source
	}

	if content, source := read("conv-wrote"); source != "agent" || content.String != "what I tried and why" {
		t.Errorf("a row with content read back (source=%q, agent_content=%q), want (agent, %q)",
			source, content.String, "what I tried and why")
	}
	// The only honest reading of a legacy NULL-content row: nothing was
	// remembered. Anything else would put words in a conversation's mouth.
	if content, source := read("conv-silent"); source != "none" || content.Valid {
		t.Errorf("a NULL-content row read back (source=%q, agent_content valid=%v), want (none, invalid)",
			source, content.Valid)
	}

	// The dropped columns are gone from the rebuilt table.
	for _, col := range []string{"entity_id", "human_content"} {
		var n int
		if err := database.QueryRow(
			`SELECT count(*) FROM pragma_table_info('conversation_memory') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info: %v", err)
		}
		if n != 0 {
			t.Errorf("column %q survived the rebuild", col)
		}
	}

	// The rebuild drops and recreates the table. With foreign_keys ON that drop
	// is an implicit DELETE, which would cascade into the join table and take
	// every memory's reachability with it — the pragma toggle is what stops
	// that, and this is the assertion that notices if it is ever removed.
	var joinRows int
	if err := database.QueryRow(
		`SELECT count(*) FROM conversation_memory_entities WHERE entity_id = ?`, entityID,
	).Scan(&joinRows); err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if joinRows != 2 {
		t.Errorf("conversation_memory_entities has %d rows for the entity, want 2 (untouched by the rebuild)", joinRows)
	}

	// The UNIQUE(conversation_id) the reads and the upsert's ON CONFLICT both
	// depend on survives the rebuild.
	if _, err := database.Exec(
		`INSERT INTO conversation_memory (id, conversation_id, agent_content, source) VALUES ('dupe', 'conv-wrote', 'x', 'agent')`,
	); err == nil {
		t.Error("a second memory row for one conversation was accepted; UNIQUE(conversation_id) did not survive the rebuild")
	}

	// The cascade the new table declares is the one the old one had: deleting a
	// conversation takes its memory with it.
	if _, err := database.Exec(`DELETE FROM conversations WHERE id = 'conv-silent'`); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	var remaining int
	if err := database.QueryRow(
		`SELECT count(*) FROM conversation_memory WHERE conversation_id = 'conv-silent'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if remaining != 0 {
		t.Errorf("conversation delete left %d memory row(s); ON DELETE CASCADE did not survive the rebuild", remaining)
	}
}
