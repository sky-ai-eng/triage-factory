package agenthost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// seedPgConversationWithMemory inserts a completed conversation (visibility
// as given) plus one
// authored conversation_memory row and its primary conversation_memory_entities join on entityID.
// origin='interactive' sidesteps conversations_origin_requires_parents
// so no task / prompt / blueprint parentage is needed — MemoryLoad
// reads only conversations.visibility + conversations.team_id off the
// conversation, never its lineage.
func seedPgConversationWithMemory(t *testing.T, h *pgtest.Harness, stores db.Stores, orgID, teamID, creatorID, entityID, visibility, narrative string) string {
	t.Helper()
	conversationID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, visibility, trigger_type, origin, status)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'interactive', 'completed')
	`, conversationID, orgID, teamID, creatorID, visibility); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	ctx := context.Background()
	if err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, conversationID, entityID, "", narrative); err != nil {
		t.Fatalf("UpsertAgentMemorySystem: %v", err)
	}
	if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, entityID, domain.MemoryRolePrimary); err != nil {
		t.Fatalf("RecordEntityTouchSystem: %v", err)
	}
	return conversationID
}

// contentsOf collects each entry's composed Content for set assertions.
func contentsOf(mems []MemoryLoadEntry) map[string]bool {
	out := map[string]bool{}
	for _, m := range mems {
		out[m.Content] = true
	}
	return out
}

// TestLocalClient_MemoryLoad_Postgres_TeamScoped pins that MemoryLoad threads the
// reading conversation's owning team into the memory read (admin-pool,
// BYPASSRLS): on one SHARED org-wide entity, a team-1 conversation sees its
// own team-visible memory plus any
// org-visible memory — never team-2's team-visible memory — and Count matches
// that same filter. A regression that passed "" instead of info.TeamID would
// leak team-2's narrative here. The team-scoping semantics themselves are the
// store's; this proves MemoryLoad wires the reading team into them.
func TestLocalClient_MemoryLoad_Postgres_TeamScoped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, h, "mem-load-scope")
	team2 := pgtest.SeedTeam(t, h, orgID, "mem-load-scope-b")

	// One entity, shared by both teams (entities are org-wide).
	ent, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "octo/repo#7", "pr", "Shared PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	entityID := ent.ID

	// team-1's own team-visible memory; team-2's team-visible memory (must be
	// excluded from team-1's read); an org-visible conversation owned by team-2
	// (included
	// via the visibility='org' arm, NOT the team arm).
	seedPgConversationWithMemory(t, h, stores, orgID, team1, owner, entityID, "team", "team1 narrative")
	seedPgConversationWithMemory(t, h, stores, orgID, team2, owner, entityID, "team", "team2 narrative")
	seedPgConversationWithMemory(t, h, stores, orgID, team2, owner, entityID, "org", "org-wide narrative")

	// A team-1 reading conversation for the touch FK.
	readerConv := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, visibility, trigger_type, origin, status)
		VALUES ($1, $2, $3, $4, 'team', 'manual', 'interactive', 'running')
	`, readerConv, orgID, team1, owner); err != nil {
		t.Fatalf("seed reader conversation: %v", err)
	}

	client := NewLocal(stores, ConversationInfo{OrgID: orgID, UserID: owner, TeamID: team1, ConversationID: readerConv, IsEventTriggered: true})
	res, err := client.MemoryLoad(ctx, "github", "octo/repo#7", 20)
	if err != nil {
		t.Fatalf("MemoryLoad: %v", err)
	}

	if res.Count != 2 {
		t.Errorf("Count = %d, want 2 (team1 + org-visible; team2's team-visible excluded)", res.Count)
	}
	got := contentsOf(res.Memories)
	if len(res.Memories) != 2 {
		t.Fatalf("len(Memories) = %d, want 2: %+v", len(res.Memories), res.Memories)
	}
	if !got["team1 narrative"] || !got["org-wide narrative"] {
		t.Errorf("Memories = %v, want team1 + org-wide narratives", got)
	}
	if got["team2 narrative"] {
		t.Errorf("team2's team-visible narrative leaked into team1's read: %v", got)
	}
	// The bounded read returns oldest-first (DESC LIMIT reversed to ASC): team1's
	// conversation was seeded before the org-visible one, so its narrative
	// comes first.
	if res.Memories[0].Content != "team1 narrative" || res.Memories[1].Content != "org-wide narrative" {
		t.Errorf("Memories order = [%q, %q], want ASC [team1, org-wide]", res.Memories[0].Content, res.Memories[1].Content)
	}

	// A tighter limit caps the set IN the query (not sliced after a full fetch)
	// while Count stays the pre-limit total — team1's single most-recent visible
	// memory is the org-visible one (seeded last).
	resLim, err := client.MemoryLoad(ctx, "github", "octo/repo#7", 1)
	if err != nil {
		t.Fatalf("MemoryLoad(limit 1): %v", err)
	}
	if resLim.Count != 2 {
		t.Errorf("Count under limit 1 = %d, want 2 (pre-limit total)", resLim.Count)
	}
	if len(resLim.Memories) != 1 || resLim.Memories[0].Content != "org-wide narrative" {
		t.Errorf("limit 1 = %+v, want just the most recent visible memory (org-wide narrative)", resLim.Memories)
	}

	// Loading recorded the touch for the reading conversation.
	var role string
	if err := h.AdminDB.QueryRow(
		`SELECT role FROM conversation_memory_entities WHERE conversation_id = $1 AND entity_id = $2`, readerConv, entityID,
	).Scan(&role); err != nil {
		t.Fatalf("read reader touch row: %v", err)
	}
	if role != domain.MemoryRoleTouched {
		t.Errorf("reader touch role = %q, want %q", role, domain.MemoryRoleTouched)
	}

	// Symmetric: a team-2 read sees team2 + org-visible, never team1's.
	client2 := NewLocal(stores, ConversationInfo{OrgID: orgID, UserID: owner, TeamID: team2, ConversationID: readerConv, IsEventTriggered: true})
	res2, err := client2.MemoryLoad(ctx, "github", "octo/repo#7", 20)
	if err != nil {
		t.Fatalf("MemoryLoad team2: %v", err)
	}
	got2 := contentsOf(res2.Memories)
	if res2.Count != 2 || got2["team1 narrative"] || !got2["team2 narrative"] || !got2["org-wide narrative"] {
		t.Errorf("team2 read = %v (count %d), want {team2, org-wide}, count 2", got2, res2.Count)
	}
}

// TestLocalClient_MemoryLoad_Postgres_Miss pins the miss path over Postgres: an
// unknown natural key returns an empty result and mints no entity (the lookup is
// GetBySourceSystem, never FindOrCreate).
func TestLocalClient_MemoryLoad_Postgres_Miss(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, owner, team1 := pgtest.SeedOrgWithUser(t, h, "mem-load-miss")
	readerConv := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO conversations (id, org_id, team_id, creator_user_id, visibility, trigger_type, origin, status)
		VALUES ($1, $2, $3, $4, 'team', 'manual', 'interactive', 'running')
	`, readerConv, orgID, team1, owner); err != nil {
		t.Fatalf("seed reader conversation: %v", err)
	}

	client := NewLocal(stores, ConversationInfo{OrgID: orgID, UserID: owner, TeamID: team1, ConversationID: readerConv, IsEventTriggered: true})
	res, err := client.MemoryLoad(ctx, "jira", "NOPE-1", 20)
	if err != nil {
		t.Fatalf("MemoryLoad(miss): %v", err)
	}
	if res.EntityID != "" || res.Count != 0 || len(res.Memories) != 0 {
		t.Errorf("miss result = %+v, want empty EntityID / Count 0 / no memories", res)
	}
	// No entity minted.
	got, err := stores.Entities.GetBySourceSystem(ctx, orgID, "jira", "NOPE-1")
	if err != nil {
		t.Fatalf("GetBySourceSystem: %v", err)
	}
	if got != nil {
		t.Errorf("a miss must mint no entity, got %+v", got)
	}
	// No touch row for the reading conversation.
	var n int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM conversation_memory_entities WHERE conversation_id = $1`, readerConv).Scan(&n); err != nil {
		t.Fatalf("count touch rows: %v", err)
	}
	if n != 0 {
		t.Errorf("miss recorded %d touch row(s), want 0", n)
	}
}
