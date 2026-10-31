package memoryentities

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// --- fixtures ---

// newTestDB spins up an in-memory SQLite with the full schema so the attach
// runs against the same stores production uses. Forcing single-conn because
// :memory: is per-conn — a pooled second connection would see an empty schema.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	if err := db.SetLocalGitHubURLForTest(database, "https://github.com"); err != nil {
		t.Fatalf("set github url: %v", err)
	}
	return database
}

// seedBlueprintRun mints a blueprint + blueprint_run for taskID and returns the
// blueprint_run id, so a conversation fixture can satisfy
// conversations.blueprint_run_id NOT NULL.
func seedBlueprintRun(t *testing.T, database *sql.DB, suffix, taskID string) string {
	t.Helper()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	bpID := "bp-" + suffix
	if _, err := sqlitestore.New(database).Blueprints.Create(context.Background(), org, team, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: team,
	}); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := "bpr-" + suffix
	// One running blueprint_run per task is a schema invariant
	// (blueprint_runs_one_active_run_per_task), so staging a task's next
	// engagement settles the one before it — which is what the task moving on
	// means.
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE task_id = ? AND status = 'running'`, taskID); err != nil {
		t.Fatalf("settle the task's prior blueprint_run: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, worktree_path, step_plan) VALUES (?, ?, ?, 'manual', ?, '[]')`,
		brID, bpID, taskID, "/tmp/wt-"+brID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedConversation inserts a conversation (plus the entity/event/task/blueprint
// fixtures its foreign keys need) with the requested id. The task's entity is
// the primary one the attach is called with.
func seedConversation(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "https://github.com", "owner/repo#"+conversationID, "", "pr", "T", "https://example.com/"+conversationID)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, team, entity.ID, domain.EventGitHubPRCICheckFailed, conversationID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := stores.Prompts.Create(ctx, org, team, domain.Prompt{ID: "test-prompt-" + conversationID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	brID := seedBlueprintRun(t, database, conversationID, task.ID)
	stepIdx := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: conversationID, TaskID: task.ID, PromptID: "test-prompt-" + conversationID,
		Status: "running", Model: "claude-sonnet-4-6",
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})
}

// primaryEntity returns the entity seedConversation attached the conversation's
// task to.
func primaryEntity(t *testing.T, database *sql.DB, conversationID string) *domain.Entity {
	t.Helper()
	return entityBySource(t, database, "github", "owner/repo#"+conversationID)
}

func entityBySource(t *testing.T, database *sql.DB, source, sourceID string) *domain.Entity {
	t.Helper()
	ent, err := sqlitestore.New(database).Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, source, testScope(source), sourceID)
	if err != nil {
		t.Fatalf("GetBySource(%s,%s): %v", source, sourceID, err)
	}
	return ent
}

func makeEntity(t *testing.T, database *sql.DB, source, sourceID, kind string) *domain.Entity {
	t.Helper()
	entities := sqlitestore.New(database).Entities
	var ent *domain.Entity
	var err error
	if source == "jira" {
		ent, _, err = entities.FindOrCreateJiraSystem(context.Background(), runmode.LocalDefaultOrgID, testScope(source), sourceID, dbseed.JiraIssueID(sourceID), dbseed.JiraIssueProject(sourceID), "", "")
	} else {
		ent, _, err = entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, source, testScope(source), sourceID, "", kind, "", "")
	}
	if err != nil {
		t.Fatalf("FindOrCreate(%s,%s): %v", source, sourceID, err)
	}
	return ent
}

// roleFor reads the conversation_memory_entities role for one
// (conversation, entity) pair, or "" when no join row exists.
func roleFor(t *testing.T, database *sql.DB, conversationID, entityID string) string {
	t.Helper()
	var role string
	err := database.QueryRow(
		`SELECT role FROM conversation_memory_entities WHERE conversation_id = ? AND entity_id = ?`,
		conversationID, entityID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read role (conversation=%s entity=%s): %v", conversationID, entityID, err)
	}
	return role
}

// memoryConversationIDs returns the set of conversation ids whose memory the
// entity-scoped read surfaces — the through-the-join reachability the attach
// creates.
func memoryConversationIDs(t *testing.T, database *sql.DB, entityID string) map[string]bool {
	t.Helper()
	mems, err := sqlitestore.New(database).TaskMemory.GetMemoriesForEntitySystem(context.Background(), runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem(%s): %v", entityID, err)
	}
	out := make(map[string]bool, len(mems))
	for _, m := range mems {
		out[m.ConversationID] = true
	}
	return out
}

// attachAll calls Attach with the memory store both production callers hand
// it.
func attachAll(t *testing.T, database *sql.DB, orgID, conversationID, primaryEntityID string) {
	t.Helper()
	Attach(context.Background(), sqlitestore.New(database).TaskMemory, orgID, conversationID, primaryEntityID)
}

// --- tests ---

// TestAttach_PrimaryAlwaysWritten: the primary join row is written
// unconditionally, even for a conversation that remembered nothing — the join
// row is what a later memory write on the same conversation becomes reachable
// through, so it must not wait for one.
func TestAttach_PrimaryAlwaysWritten(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-prim")
	ctx := context.Background()
	taskMemory := sqlitestore.New(database).TaskMemory
	entA := primaryEntity(t, database, "r-prim")

	// source='none' → agent_content NULL, matching a conversation whose agent
	// never wrote its memory file. The row still lands, and the primary join
	// row still attaches to it.
	if _, err := taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-prim", "", "", domain.MemorySourceNone); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	attachAll(t, database, runmode.LocalDefaultOrgID, "r-prim", entA.ID)

	if role := roleFor(t, database, "r-prim", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("primary role = %q, want %q (attach is unconditional)", role, domain.MemoryRolePrimary)
	}
	// The 'none' row itself stays out of the entity read — there is nothing to
	// materialize — but the join row above is what carries the agent's own
	// memory to this entity the moment one is written.
	if memoryConversationIDs(t, database, entA.ID)["r-prim"] {
		t.Error("a conversation that remembered nothing surfaced in the entity read")
	}
	if _, err := taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-prim", "", "what I tried", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert agent memory: %v", err)
	}
	if !memoryConversationIDs(t, database, entA.ID)["r-prim"] {
		t.Error("the already-attached entity does not reach the memory the conversation later wrote")
	}
}

// TestAttach_NilTaskMemoryWritesNothing: with no memory store there is nowhere
// to write at all.
func TestAttach_NilTaskMemoryWritesNothing(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-nomem")
	entA := primaryEntity(t, database, "r-nomem")

	Attach(context.Background(), nil, runmode.LocalDefaultOrgID, "r-nomem", entA.ID)

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ?`, "r-nomem").Scan(&n); err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if n != 0 {
		t.Errorf("join rows = %d, want 0", n)
	}
}

// TestAttach_Precedence: the primary entity ends primary over the produced and
// touched rows the exec funnel wrote for it mid-run, and the attach leaves the
// produced row of another entity as it is.
func TestAttach_Precedence(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-prec")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	// Primary P is itself a PR the agent touched and then changed mid-run.
	entP := makeEntity(t, database, "github", "o/r#100", "pr")
	entB := makeEntity(t, database, "github", "o/r#200", "pr")

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, org, "r-prec", "", "narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	for _, link := range []struct{ entityID, role string }{
		{entP.ID, domain.MemoryRoleTouched},
		{entP.ID, domain.MemoryRoleProduced},
		{entB.ID, domain.MemoryRoleProduced},
	} {
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-prec", link.entityID, link.role); err != nil {
			t.Fatalf("record %s link: %v", link.role, err)
		}
	}

	attachAll(t, database, org, "r-prec", entP.ID)

	if role := roleFor(t, database, "r-prec", entP.ID); role != domain.MemoryRolePrimary {
		t.Errorf("P role = %q, want %q (primary wins over touched+produced)", role, domain.MemoryRolePrimary)
	}
	if role := roleFor(t, database, "r-prec", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("B role = %q, want %q", role, domain.MemoryRoleProduced)
	}
}

// TestAttach_MotivatingCase is the read-path acceptance for the motivating
// case: a Slack-thread-triggered conversation (primary A) that opened a PR
// (produced B) and read an issue (C), both linked mid-run by the exec funnel,
// leaves its narrative reachable from all three, with roles {A: primary,
// B: produced, C: touched}.
func TestAttach_MotivatingCase(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-507")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	entA := makeEntity(t, database, "slack", domain.SlackSourceID("C0125", "1700000000.000100"), "thread")
	entB := makeEntity(t, database, "github", "o/r#77", "pr")
	entC := makeEntity(t, database, "jira", "SKY-9", "issue")

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, org, "r-507", "", "the run narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-507", entB.ID, domain.MemoryRoleProduced); err != nil {
		t.Fatalf("produce B: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-507", entC.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("touch C: %v", err)
	}

	attachAll(t, database, org, "r-507", entA.ID)

	for name, id := range map[string]string{"A": entA.ID, "B": entB.ID, "C": entC.ID} {
		mems, err := stores.TaskMemory.GetMemoriesForEntitySystem(ctx, org, id, runmode.LocalDefaultTeamID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntitySystem(%s): %v", name, err)
		}
		if len(mems) != 1 || mems[0].ConversationID != "r-507" {
			t.Fatalf("entity %s: got %d memories %+v, want the conversation's one", name, len(mems), mems)
		}
		if mems[0].Content != "the run narrative" {
			t.Errorf("entity %s content = %q, want the run narrative", name, mems[0].Content)
		}
	}

	if role := roleFor(t, database, "r-507", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("A role = %q, want primary", role)
	}
	if role := roleFor(t, database, "r-507", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("B role = %q, want produced", role)
	}
	if role := roleFor(t, database, "r-507", entC.ID); role != domain.MemoryRoleTouched {
		t.Errorf("C role = %q, want touched", role)
	}
}

// TestAttach_MultiStepPrimaryPerStep: two step conversations on one task each
// write their own primary join row for the shared entity, so the entity's
// memory read surfaces both conversations.
func TestAttach_MultiStepPrimaryPerStep(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-step1")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	entA := primaryEntity(t, database, "r-step1")

	// A second step conversation on the same task (same entity).
	conv1, err := stores.Conversations.GetSystem(ctx, org, "r-step1")
	if err != nil || conv1 == nil {
		t.Fatalf("GetSystem(r-step1): err=%v conversation=%v", err, conv1)
	}
	stepIdx := 1
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: "r-step2", TaskID: conv1.TaskID, PromptID: "test-prompt-r-step1",
		Status: "running", Model: "claude-sonnet-4-6",
		BlueprintRunID: seedBlueprintRun(t, database, "r-step2", conv1.TaskID), BlueprintStepIndex: &stepIdx,
	})

	for _, conversationID := range []string{"r-step1", "r-step2"} {
		if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, org, conversationID, "", conversationID+" narrative", domain.MemorySourceAgent); err != nil {
			t.Fatalf("upsert memory %s: %v", conversationID, err)
		}
		attachAll(t, database, org, conversationID, entA.ID)
	}

	if role := roleFor(t, database, "r-step1", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("step1 role = %q, want primary", role)
	}
	if role := roleFor(t, database, "r-step2", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("step2 role = %q, want primary", role)
	}
	conversationIDs := memoryConversationIDs(t, database, entA.ID)
	if !conversationIDs["r-step1"] || !conversationIDs["r-step2"] {
		t.Errorf("entity should reach both step conversations, got %v", conversationIDs)
	}
}

// testScope is the scope a test keys an entity of source under: what
// domain.EntityScope answers for an org with default settings where the
// source has one, and a fixed stand-in where it has none.
func testScope(source string) string {
	switch source {
	case "github":
		return "https://github.com"
	case "jira":
		return "https://jira.example.com"
	case "slack":
		return "slack.com"
	case "linear":
		return "ws-test"
	}
	return "test-scope"
}
