package delegate

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
)

// --- helpers shared by the attach-pass tests ---

func attachEntityBySource(t *testing.T, database *sql.DB, source, sourceID string) *domain.Entity {
	t.Helper()
	ent, err := sqlitestore.New(database).Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, source, sourceID)
	if err != nil {
		t.Fatalf("GetBySource(%s,%s): %v", source, sourceID, err)
	}
	return ent
}

func attachMakeEntity(t *testing.T, database *sql.DB, source, sourceID, kind string) *domain.Entity {
	t.Helper()
	ent, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, source, sourceID, kind, "", "")
	if err != nil {
		t.Fatalf("FindOrCreate(%s,%s): %v", source, sourceID, err)
	}
	return ent
}

// attachRoleFor reads the conversation_memory_entities role for one (run, entity) pair,
// or "" when no join row exists.
func attachRoleFor(t *testing.T, database *sql.DB, conversationID, entityID string) string {
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
		t.Fatalf("read role (run=%s entity=%s): %v", conversationID, entityID, err)
	}
	return role
}

// attachMemoryConversationIDs returns the set of conversation ids whose memory the entity-scoped
// read surfaces — the through-the-join reachability the attach pass creates.
func attachMemoryConversationIDs(t *testing.T, s *Spawner, entityID string) map[string]bool {
	t.Helper()
	mems, err := s.taskMemory.GetMemoriesForEntitySystem(context.Background(), runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem(%s): %v", entityID, err)
	}
	out := make(map[string]bool, len(mems))
	for _, m := range mems {
		out[m.ConversationID] = true
	}
	return out
}

// seedProducedPR upserts a PR artifact attributed to conversationID so the produced
// attach derives an entity from it.
func seedProducedPR(t *testing.T, s *Spawner, conversationID, repoPath string, number int, url string) {
	t.Helper()
	a := domain.NewPullRequestArtifact(repoPath, number, "node", "head", "main", url, "produced PR", "", false)
	seedResolvedArtifact(t, s, conversationID, a)
}

// TestAttachConversationMemoryEntities_PrimaryAlwaysWritten: the primary join row is
// written unconditionally at termination, exactly like the conversation_memory upsert —
// even when the agent wrote no memory file (agent_content NULL).
func TestAttachConversationMemoryEntities_PrimaryAlwaysWritten(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-prim", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	entA := attachEntityBySource(t, database, "github", "owner/repo#r-prim")

	// Empty content → agent_content NULL, matching a run whose agent never
	// wrote its memory file. The upsert still lands a row.
	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-prim", entA.ID, "", ""); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	s.attachConversationMemoryEntities(ctx, runmode.LocalDefaultOrgID, "r-prim", entA.ID)

	if role := attachRoleFor(t, database, "r-prim", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("primary role = %q, want %q (attach is unconditional)", role, domain.MemoryRolePrimary)
	}
	if !attachMemoryConversationIDs(t, s, entA.ID)["r-prim"] {
		t.Error("primary entity does not reach the run's memory through the join")
	}
}

// TestAttachConversationMemoryEntities_ProducedStubForUnpolledPR: a produced-PR artifact
// whose entity hasn't been polled yet is create-minimal'd as a snapshot-less
// stub (linked out by the artifact URL) and attached with role=produced.
func TestAttachConversationMemoryEntities_ProducedStubForUnpolledPR(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-prod", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	entA := attachEntityBySource(t, database, "github", "owner/repo#r-prod")

	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-prod", entA.ID, "", "narrative"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// A PR the run just opened — no entity exists for it yet.
	seedProducedPR(t, s, "r-prod", "o/r", 4242, "https://github.com/o/r/pull/4242")

	s.attachConversationMemoryEntities(ctx, runmode.LocalDefaultOrgID, "r-prod", entA.ID)

	entB := attachEntityBySource(t, database, "github", "o/r#4242")
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
	if role := attachRoleFor(t, database, "r-prod", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("produced role = %q, want %q", role, domain.MemoryRoleProduced)
	}
	if !attachMemoryConversationIDs(t, s, entB.ID)["r-prod"] {
		t.Error("produced entity does not reach the run's memory through the join")
	}
}

// TestAttachConversationMemoryEntities_RepoLevelTargetsSkipped: artifacts that map to no
// entity — a git branch push (unmapped provider) and a github repo-level target
// with no '#N' — mint no entity and write no produced row. Only the primary
// row lands.
func TestAttachConversationMemoryEntities_RepoLevelTargetsSkipped(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-skip", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	entA := attachEntityBySource(t, database, "github", "owner/repo#r-skip")

	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-skip", entA.ID, "", "narrative"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// Branch push → provider "git", target "o/r": unmapped provider.
	branch, ok := domain.NewBranchArtifact("o/r", "refs/heads/feature", "sha123", true)
	if !ok {
		t.Fatal("NewBranchArtifact returned ok=false")
	}
	seedResolvedArtifact(t, s, "r-skip", branch)
	// A github artifact with a bare repo-level target (no '#N') → ParsePRTarget
	// ok=false.
	repoLevel := domain.Artifact{
		Provider: domain.ArtifactProviderGitHub,
		Kind:     domain.ArtifactKindPullRequest,
		Target:   "o/r",
		State:    domain.ArtifactStatePROpen,
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindPullRequest, "o/r", ""),
	}
	seedResolvedArtifact(t, s, "r-skip", repoLevel)

	s.attachConversationMemoryEntities(ctx, runmode.LocalDefaultOrgID, "r-skip", entA.ID)

	if ent := attachEntityBySource(t, database, domain.ArtifactProviderGit, "o/r"); ent != nil {
		t.Errorf("branch push should mint no entity, got %+v", ent)
	}
	if ent := attachEntityBySource(t, database, domain.ArtifactProviderGitHub, "o/r"); ent != nil {
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

// TestAttachConversationMemoryEntities_ListFailureLeavesPrimaryIntact: when the artifact
// listing fails, the produced pass is skipped but the memory upsert and the
// primary row survive — the attach is best-effort and never aborts completion.
func TestAttachConversationMemoryEntities_ListFailureLeavesPrimaryIntact(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fail", "sess", "/tmp/wt")
	stores := testSpawnerStores(database)
	stores.Artifacts = failingArtifactStore{ArtifactStore: stores.Artifacts}
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	entA := attachEntityBySource(t, database, "github", "owner/repo#r-fail")

	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, runmode.LocalDefaultOrgID, "r-fail", entA.ID, "", "narrative"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}

	s.attachConversationMemoryEntities(ctx, runmode.LocalDefaultOrgID, "r-fail", entA.ID)

	if role := attachRoleFor(t, database, "r-fail", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("primary role = %q, want %q despite the listing failure", role, domain.MemoryRolePrimary)
	}
	if !attachMemoryConversationIDs(t, s, entA.ID)["r-fail"] {
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

// TestAttachConversationMemoryEntities_Precedence: a pre-existing touched row upgrades to
// produced on the produced PR, and the primary entity wins over both a prior
// touched row and a produced artifact pointing at it.
func TestAttachConversationMemoryEntities_Precedence(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-prec", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Primary P is itself a PR that the run also produced and had already
	// touched mid-run — it must still end primary.
	entP := attachMakeEntity(t, database, "github", "o/r#100", "pr")
	entB := attachMakeEntity(t, database, "github", "o/r#200", "pr")

	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, org, "r-prec", entP.ID, "", "narrative"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// Mid-run touches recorded before termination.
	if err := s.taskMemory.RecordEntityTouchSystem(ctx, org, "r-prec", entP.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("pre-touch P: %v", err)
	}
	if err := s.taskMemory.RecordEntityTouchSystem(ctx, org, "r-prec", entB.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("pre-touch B: %v", err)
	}
	// Both entities appear as produced artifacts.
	seedProducedPR(t, s, "r-prec", "o/r", 100, "https://github.com/o/r/pull/100")
	seedProducedPR(t, s, "r-prec", "o/r", 200, "https://github.com/o/r/pull/200")

	s.attachConversationMemoryEntities(ctx, org, "r-prec", entP.ID)

	if role := attachRoleFor(t, database, "r-prec", entP.ID); role != domain.MemoryRolePrimary {
		t.Errorf("P role = %q, want %q (primary wins over touched+produced)", role, domain.MemoryRolePrimary)
	}
	if role := attachRoleFor(t, database, "r-prec", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("B role = %q, want %q (touched upgrades to produced)", role, domain.MemoryRoleProduced)
	}
}

// TestAttachConversationMemoryEntities_MotivatingCase is the read-path acceptance for
// the motivating case: a Slack-thread-triggered run (primary A) that opens a
// PR (produced B) and touches an issue (C) leaves its narrative reachable from
// all three, with roles {A: primary, B: produced, C: touched}.
func TestAttachConversationMemoryEntities_MotivatingCase(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-507", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	entA := attachMakeEntity(t, database, "slack", domain.SlackSourceID("C0125", "1700000000.000100"), "thread")
	entC := attachMakeEntity(t, database, "jira", "SKY-9", "issue")

	if err := s.taskMemory.UpsertAgentMemorySystem(ctx, org, "r-507", entA.ID, "", "the run narrative"); err != nil {
		t.Fatalf("upsert memory: %v", err)
	}
	// C touched mid-run; B produced via an artifact.
	if err := s.taskMemory.RecordEntityTouchSystem(ctx, org, "r-507", entC.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("touch C: %v", err)
	}
	seedProducedPR(t, s, "r-507", "o/r", 77, "https://github.com/o/r/pull/77")

	s.attachConversationMemoryEntities(ctx, org, "r-507", entA.ID)

	entB := attachEntityBySource(t, database, "github", "o/r#77")
	if entB == nil {
		t.Fatal("produced PR entity B was not created")
	}

	for name, id := range map[string]string{"A": entA.ID, "B": entB.ID, "C": entC.ID} {
		mems, err := s.taskMemory.GetMemoriesForEntitySystem(ctx, org, id, runmode.LocalDefaultTeamID)
		if err != nil {
			t.Fatalf("GetMemoriesForEntitySystem(%s): %v", name, err)
		}
		if len(mems) != 1 || mems[0].ConversationID != "r-507" {
			t.Fatalf("entity %s: got %d memories %+v, want the run's one", name, len(mems), mems)
		}
		if mems[0].Content != "the run narrative" {
			t.Errorf("entity %s content = %q, want the run narrative", name, mems[0].Content)
		}
	}

	if role := attachRoleFor(t, database, "r-507", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("A role = %q, want primary", role)
	}
	if role := attachRoleFor(t, database, "r-507", entB.ID); role != domain.MemoryRoleProduced {
		t.Errorf("B role = %q, want produced", role)
	}
	if role := attachRoleFor(t, database, "r-507", entC.ID); role != domain.MemoryRoleTouched {
		t.Errorf("C role = %q, want touched", role)
	}
}

// TestAttachConversationMemoryEntities_MultiStepPrimaryPerStep: two step
// conversations on one task
// each write their own primary join row for the shared entity, so the entity's
// memory read surfaces both conversations.
func TestAttachConversationMemoryEntities_MultiStepPrimaryPerStep(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-step1", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	entA := attachEntityBySource(t, database, "github", "owner/repo#r-step1")

	// A second step-run on the same task (same entity).
	conv1, err := s.conversations.GetSystem(ctx, org, "r-step1")
	if err != nil || conv1 == nil {
		t.Fatalf("GetSystem(r-step1): err=%v conversation=%v", err, conv1)
	}
	brID := seedConversationBlueprint(t, database, "r-step2", conv1.TaskID)
	stepIdx := 1
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: "r-step2", TaskID: conv1.TaskID, PromptID: "test-prompt",
		Status: "running", Model: "claude-sonnet-4-6",
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})

	for _, conversationID := range []string{"r-step1", "r-step2"} {
		if err := s.taskMemory.UpsertAgentMemorySystem(ctx, org, conversationID, entA.ID, "", conversationID+" narrative"); err != nil {
			t.Fatalf("upsert memory %s: %v", conversationID, err)
		}
		s.attachConversationMemoryEntities(ctx, org, conversationID, entA.ID)
	}

	if role := attachRoleFor(t, database, "r-step1", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("step1 role = %q, want primary", role)
	}
	if role := attachRoleFor(t, database, "r-step2", entA.ID); role != domain.MemoryRolePrimary {
		t.Errorf("step2 role = %q, want primary", role)
	}
	conversationIDs := attachMemoryConversationIDs(t, s, entA.ID)
	if !conversationIDs["r-step1"] || !conversationIDs["r-step2"] {
		t.Errorf("entity should reach both step conversations, got %v", conversationIDs)
	}
}
