package memoryentities

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#"+conversationID, "pr", "T", "https://example.com/"+conversationID)
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
	ent, err := sqlitestore.New(database).Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, source, sourceID)
	if err != nil {
		t.Fatalf("GetBySource(%s,%s): %v", source, sourceID, err)
	}
	return ent
}

func makeEntity(t *testing.T, database *sql.DB, source, sourceID, kind string) *domain.Entity {
	t.Helper()
	ent, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, source, sourceID, kind, "", "")
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

// seedArtifact upserts an artifact attributed to conversationID so the produced
// pass sees it.
func seedArtifact(t *testing.T, database *sql.DB, conversationID string, a domain.Artifact) {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := sqlitestore.New(database).Artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed artifact %s/%s: %v", a.Kind, a.State, err)
	}
}

// seedProducedPR upserts a PR artifact attributed to conversationID so the
// produced attach derives an entity from it.
func seedProducedPR(t *testing.T, database *sql.DB, conversationID, repoPath string, number int, url string) {
	t.Helper()
	seedArtifact(t, database, conversationID, domain.NewPullRequestArtifact(repoPath, number, "node", "head", "main", url, "produced PR", "", false))
}

// attachAll calls Attach with the full store bundle — what both production
// callers hand it.
func attachAll(t *testing.T, database *sql.DB, orgID, conversationID, primaryEntityID string) {
	t.Helper()
	stores := sqlitestore.New(database)
	Attach(context.Background(), stores.TaskMemory, stores.Artifacts, stores.Entities, orgID, conversationID, primaryEntityID)
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

// TestAttach_ProducedStubForUnpolledPR: a produced-PR artifact whose entity
// hasn't been polled yet is create-minimal'd as a snapshot-less stub (linked
// out by the artifact URL) and attached with role=produced.
func TestAttach_ProducedStubForUnpolledPR(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-prod")
	ctx := context.Background()
	entA := primaryEntity(t, database, "r-prod")

	if _, err := sqlitestore.New(database).TaskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-prod", "", "narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// A PR the agent just opened — no entity exists for it yet.
	seedProducedPR(t, database, "r-prod", "o/r", 4242, "https://github.com/o/r/pull/4242")

	attachAll(t, database, runmode.LocalDefaultOrgID, "r-prod", entA.ID)

	entB := entityBySource(t, database, "github", "o/r#4242")
	if entB == nil {
		t.Fatal("produced PR entity was not create-minimal'd")
	}
	if entB.SnapshotJSON != "" {
		t.Errorf("expected a snapshot-less stub, got snapshot %q", entB.SnapshotJSON)
	}
	if entB.URL != "https://github.com/o/r/pull/4242" {
		t.Errorf("stub URL = %q, want the artifact URL", entB.URL)
	}
	if entB.Kind != "pr" {
		t.Errorf("stub kind = %q, want pr", entB.Kind)
	}
	if role := roleFor(t, database, "r-prod", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("produced role = %q, want %q", role, domain.MemoryRoleProduced)
	}
	if !memoryConversationIDs(t, database, entB.ID)["r-prod"] {
		t.Error("produced entity does not reach the conversation's memory through the join")
	}
}

// TestAttach_RepoLevelTargetsSkipped: artifacts that map to no entity — a git
// branch push (unmapped provider) and a github repo-level target with no '#N'
// — mint no entity and write no produced row. Only the primary row lands.
func TestAttach_RepoLevelTargetsSkipped(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-skip")
	ctx := context.Background()
	entA := primaryEntity(t, database, "r-skip")

	if _, err := sqlitestore.New(database).TaskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-skip", "", "narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// Branch push → provider "git", target "o/r": unmapped provider.
	branch, ok := domain.NewBranchArtifact("o/r", "refs/heads/feature", "sha123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	seedArtifact(t, database, "r-skip", branch)
	// A github artifact with a bare repo-level target (no '#N') → ParsePRTarget
	// ok=false.
	seedArtifact(t, database, "r-skip", domain.Artifact{
		Provider: domain.ArtifactProviderGitHub,
		Kind:     domain.ArtifactKindPullRequest,
		Target:   "o/r",
		State:    domain.ArtifactStatePROpen,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, "o/r", ""),
	})

	attachAll(t, database, runmode.LocalDefaultOrgID, "r-skip", entA.ID)

	if ent := entityBySource(t, database, domain.ArtifactProviderGit, "o/r"); ent != nil {
		t.Errorf("branch push should mint no entity, got %+v", ent)
	}
	if ent := entityBySource(t, database, domain.ArtifactProviderGitHub, "o/r"); ent != nil {
		t.Errorf("repo-level github target should mint no entity, got %+v", ent)
	}
	// The only join row is the primary — the skipped targets add nothing.
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ?`, "r-skip").Scan(&n); err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if n != 1 {
		t.Errorf("join rows = %d, want 1 (primary only)", n)
	}
}

// TestAttach_ListFailureLeavesPrimaryIntact: when the artifact listing fails,
// the produced pass is skipped but the memory upsert and the primary row
// survive — the attach is best-effort and never aborts its caller.
func TestAttach_ListFailureLeavesPrimaryIntact(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-fail")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	entA := primaryEntity(t, database, "r-fail")

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-fail", "", "narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	Attach(ctx, stores.TaskMemory, failingArtifactStore{ArtifactStore: stores.Artifacts}, stores.Entities,
		runmode.LocalDefaultOrgID, "r-fail", entA.ID)

	if role := roleFor(t, database, "r-fail", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("primary role = %q, want %q despite the listing failure", role, domain.MemoryRolePrimary)
	}
	if !memoryConversationIDs(t, database, entA.ID)["r-fail"] {
		t.Error("memory upsert lost when artifact listing failed")
	}
}

// failingArtifactStore fails ListByConversationSystem (the only method the produced pass
// calls); every other method delegates to the embedded real store.
type failingArtifactStore struct {
	db.ArtifactStore
}

func (failingArtifactStore) ListByConversationSystem(context.Context, string, string) ([]domain.Artifact, error) {
	return nil, errors.New("artifact listing failed")
}

// TestAttach_AbsentStoresSkipProducedPass: a caller holding no artifact or
// entity store cannot resolve produced entities, and says so by passing nil.
// The primary row still lands — it needs neither store — so the conversation's
// memory stays reachable from its task's entity.
func TestAttach_AbsentStoresSkipProducedPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		absent string
	}{
		{name: "nil artifacts", absent: "artifacts"},
		{name: "nil entities", absent: "entities"},
		{name: "both nil", absent: "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			seedConversation(t, database, "r-nil")
			ctx := context.Background()
			stores := sqlitestore.New(database)
			entA := primaryEntity(t, database, "r-nil")

			if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-nil", "", "narrative", domain.MemorySourceAgent); err != nil {
				t.Fatalf("upsert memory: %v", err)
			}
			// An artifact that WOULD mint a produced entity had both stores been present.
			seedProducedPR(t, database, "r-nil", "o/r", 9, "https://github.com/o/r/pull/9")

			artifacts, entities := stores.Artifacts, stores.Entities
			if tc.absent == "artifacts" || tc.absent == "both" {
				artifacts = nil
			}
			if tc.absent == "entities" || tc.absent == "both" {
				entities = nil
			}
			Attach(ctx, stores.TaskMemory, artifacts, entities, runmode.LocalDefaultOrgID, "r-nil", entA.ID)

			if role := roleFor(t, database, "r-nil", entA.ID); role != domain.MemoryRolePrimary {
				t.Errorf("primary role = %q, want %q", role, domain.MemoryRolePrimary)
			}
			if ent := entityBySource(t, database, "github", "o/r#9"); ent != nil {
				t.Errorf("produced pass ran without its stores: minted %+v", ent)
			}
			var n int
			if err := database.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ?`, "r-nil").Scan(&n); err != nil {
				t.Fatalf("count join rows: %v", err)
			}
			if n != 1 {
				t.Errorf("join rows = %d, want 1 (primary only)", n)
			}
		})
	}
}

// TestAttach_NilTaskMemoryWritesNothing: with no memory store there is nowhere
// to write at all, and the produced pass must not run either.
func TestAttach_NilTaskMemoryWritesNothing(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-nomem")
	stores := sqlitestore.New(database)
	entA := primaryEntity(t, database, "r-nomem")
	seedProducedPR(t, database, "r-nomem", "o/r", 11, "https://github.com/o/r/pull/11")

	Attach(context.Background(), nil, stores.Artifacts, stores.Entities, runmode.LocalDefaultOrgID, "r-nomem", entA.ID)

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM conversation_memory_entities WHERE conversation_id = ?`, "r-nomem").Scan(&n); err != nil {
		t.Fatalf("count join rows: %v", err)
	}
	if n != 0 {
		t.Errorf("join rows = %d, want 0", n)
	}
	if ent := entityBySource(t, database, "github", "o/r#11"); ent != nil {
		t.Errorf("produced pass ran with no memory store to write to: minted %+v", ent)
	}
}

// TestAttach_Precedence: a pre-existing touched row upgrades to produced on the
// produced PR, and the primary entity wins over both a prior touched row and a
// produced artifact pointing at it.
func TestAttach_Precedence(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-prec")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	// Primary P is itself a PR that the agent also produced and had already
	// touched mid-run — it must still end primary.
	entP := makeEntity(t, database, "github", "o/r#100", "pr")
	entB := makeEntity(t, database, "github", "o/r#200", "pr")

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, org, "r-prec", "", "narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// Mid-run touches recorded before termination.
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-prec", entP.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("pre-touch P: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-prec", entB.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("pre-touch B: %v", err)
	}
	// Both entities appear as produced artifacts.
	seedProducedPR(t, database, "r-prec", "o/r", 100, "https://github.com/o/r/pull/100")
	seedProducedPR(t, database, "r-prec", "o/r", 200, "https://github.com/o/r/pull/200")

	attachAll(t, database, org, "r-prec", entP.ID)

	if role := roleFor(t, database, "r-prec", entP.ID); role != domain.MemoryRolePrimary {
		t.Errorf("P role = %q, want %q (primary wins over touched+produced)", role, domain.MemoryRolePrimary)
	}
	if role := roleFor(t, database, "r-prec", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("B role = %q, want %q (touched upgrades to produced)", role, domain.MemoryRoleProduced)
	}
}

// TestAttach_MotivatingCase is the read-path acceptance for the motivating
// case: a Slack-thread-triggered conversation (primary A) that opens a PR (produced B)
// and touches an issue (C) leaves its narrative reachable from all three, with
// roles {A: primary, B: produced, C: touched}.
func TestAttach_MotivatingCase(t *testing.T) {
	database := newTestDB(t)
	seedConversation(t, database, "r-507")
	ctx := context.Background()
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	entA := makeEntity(t, database, "slack", domain.SlackSourceID("C0125", "1700000000.000100"), "thread")
	entC := makeEntity(t, database, "jira", "SKY-9", "issue")

	if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, org, "r-507", "", "the run narrative", domain.MemorySourceAgent); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// C touched mid-run; B produced via an artifact.
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, org, "r-507", entC.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("touch C: %v", err)
	}
	seedProducedPR(t, database, "r-507", "o/r", 77, "https://github.com/o/r/pull/77")

	attachAll(t, database, org, "r-507", entA.ID)

	entB := entityBySource(t, database, "github", "o/r#77")
	if entB == nil {
		t.Fatal("produced PR entity B was not created")
	}

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
