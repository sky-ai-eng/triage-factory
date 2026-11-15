package delegate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestMaterializeEntityMemories_Postgres_CarriesTheTaskAcrossTeams pins the
// handoff a cross-team reassignment leaves the new holder: the task moves to
// the new team, the old team's conversation on it stays where it was, and the
// new holder's conversation on the new team still finds that conversation's
// memory in this-task/ and in the set its opening rows are minted from. The
// old team's memory on the entity's other tasks stays with the old team, so
// history/ holds nothing of it.
func TestMaterializeEntityMemories_Postgres_CarriesTheTaskAcrossTeams(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, oldTeam := pgtest.SeedOrgWithUser(t, h, "materialize-carry")
	newTeam := pgtest.SeedTeam(t, h, orgID, "materialize-carry-b")
	entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", "https://github.com", "octo/repo#1449", "", "pr", "Shared PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	seedTask := func(teamID, dedupKey string) string {
		t.Helper()
		eventID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
		`, eventID, orgID, entity.ID); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		taskID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, priority_score, created_at)
			VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:opened', $6, $7, 'done', 'pending', 0.5, now())
		`, taskID, orgID, userID, teamID, entity.ID, dedupKey, eventID); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return taskID
	}
	seedMemoryOn := func(teamID, taskID, content string) string {
		t.Helper()
		conversationID := uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO conversations (id, org_id, team_id, creator_user_id, visibility, task_id, trigger_type, origin, status)
			VALUES ($1, $2, $3, $4, 'team', $5, 'manual', 'interactive', 'open')
		`, conversationID, orgID, teamID, userID, taskID); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		if _, err := stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, conversationID, "", content, domain.MemorySourceAgent); err != nil {
			t.Fatalf("UpsertAgentMemorySystem: %v", err)
		}
		if err := stores.TaskMemory.RecordEntityTouchSystem(ctx, orgID, conversationID, entity.ID, domain.MemoryRolePrimary); err != nil {
			t.Fatalf("RecordEntityTouchSystem: %v", err)
		}
		return conversationID
	}

	reassigned := seedTask(newTeam, "reassigned")
	handoff := seedMemoryOn(oldTeam, reassigned, "the old team's handoff")
	seedMemoryOn(oldTeam, seedTask(oldTeam, "old-other"), "the old team's other task")

	cwd := t.TempDir()
	thisTask := materializeEntityMemories(stores.TaskMemory, orgID, newTeam, localMemoryRoot(cwd), entity.ID, reassigned)

	root := localMemoryRoot(cwd)
	assertDirNames(t, filepath.Join(root, "this-task"), []string{"01.md"})
	assertMemoryFile(t, filepath.Join(root, "this-task", "01.md"), "the old team's handoff")
	assertDirNames(t, filepath.Join(root, "history"), nil)
	if len(thisTask) != 1 || thisTask[0].ConversationID != handoff {
		t.Errorf("returned this-task set = %+v, want the old team's conversation on the task", thisTask)
	}
}
