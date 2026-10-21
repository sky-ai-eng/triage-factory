package tracker

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// GitHub renamed octo/old to octo/new and TF has applied it, so the pull
// request's entity is octo/new#5. A run whose checkout predates the rename
// reads the pull request through a verb that returns no node id, and the
// touch creates a stub under octo/old#5. The next cycle resolves the stub to
// the entity's node id and folds it in: the run's memory link moves onto the
// entity, the stub is gone, nothing is emitted for it, and later cycles do not
// read it again.
func TestRefreshGitHub_AStubUnderARenamedRepositorysOldNameFoldsIntoItsEntity(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	fx := newGitHubScopeFixture(t)
	h, client := newFakeGitHubHost(t)
	trackRepos(t, fx.stores, org, []string{"octo/new"})

	h.listPR("octo/new", 5, "PR_5", "Widget")
	fx.cycle(t, ghOldHost, client, []string{"octo/new"})
	entity := fx.entity(t, ghOldHost, "octo/new#5")
	if entity == nil || entity.ExternalID != "PR_5" {
		t.Fatalf("octo/new#5 = %+v, want an entity carrying PR_5", entity)
	}

	conversation := seedTouchConversation(t, fx.database, entity.ID)
	stub := fx.touchPR(t, conversation, "octo/old#5")
	h.basicPR("octo/old#5", "PR_5")

	if evts := fx.cycle(t, ghOldHost, client, []string{"octo/new"}); len(evts) != 0 {
		t.Errorf("the cycle that folded the stub emitted %+v, want nothing", evts)
	}
	if got, err := fx.stores.Entities.GetSystem(ctx, org, stub.ID); err != nil || got != nil {
		t.Errorf("stub after the cycle = %+v (err %v), want it folded away", got, err)
	}
	if got := fx.entity(t, ghOldHost, "octo/old#5"); got != nil {
		t.Errorf("octo/old#5 = %+v, want no entity under the old name", got)
	}
	after, err := fx.stores.Entities.GetSystem(ctx, org, entity.ID)
	if err != nil || after == nil {
		t.Fatalf("reread the entity: %+v, %v", after, err)
	}
	if after.SourceID != "octo/new#5" || after.ExternalID != "PR_5" || after.SnapshotJSON != entity.SnapshotJSON {
		t.Errorf("entity after the fold = %+v, want its own key, id and snapshot", after)
	}
	if links := memoryLinks(t, fx.database, conversation); !slices.Equal(links, []string{entity.ID}) {
		t.Errorf("the run's memory links name %v, want the entity %s alone", links, entity.ID)
	}
	if active, err := fx.stores.Entities.ListActiveSystem(ctx, org, "github"); err != nil || len(active) != 1 {
		t.Errorf("active github entities = %d (err %v), want 1", len(active), err)
	}

	fx.cycle(t, ghOldHost, client, []string{"octo/new"})
	reads := 0
	for _, p := range h.askedPaths() {
		if p == "/repos/octo/old/pulls/5" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the stub's pull request was read %d times, want once: the folded stub is not polled", reads)
	}
}

// GitHub renamed octo/old to octo/new and TF has not applied it yet, so the
// pull request's entity is still octo/old#5. A touch through the new name
// creates a stub under octo/new#5, which would refuse the repository rename
// its key rewrite. The cycle folds the stub into octo/old#5, and the rename
// then applies and moves the entity onto the new name.
func TestRefreshGitHub_AStubUnderARenamedRepositorysNewNameFoldsAndTheRenameApplies(t *testing.T) {
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	fx := newGitHubScopeFixture(t)
	h, client := newFakeGitHubHost(t)
	trackRepos(t, fx.stores, org, []string{"octo/old"})
	if n, err := fx.stores.Repos.FillMissingExternalIDsSystem(ctx, org, []domain.RepoRef{
		{Source: domain.RepoSourceGitHub, Host: ghOldHost, Owner: "octo", Repo: "old", ExternalID: trackedRepositoryID},
	}); err != nil || n != 1 {
		t.Fatalf("record octo/old's repository id: n=%d err=%v", n, err)
	}

	h.listPR("octo/old", 5, "PR_5", "Widget")
	fx.cycle(t, ghOldHost, client, []string{"octo/old"})
	entity := fx.entity(t, ghOldHost, "octo/old#5")
	if entity == nil || entity.ExternalID != "PR_5" {
		t.Fatalf("octo/old#5 = %+v, want an entity carrying PR_5", entity)
	}

	conversation := seedTouchConversation(t, fx.database, entity.ID)
	stub := fx.touchPR(t, conversation, "octo/new#5")
	h.basicPR("octo/new#5", "PR_5")

	if evts := fx.cycle(t, ghOldHost, client, []string{"octo/old"}); len(evts) != 0 {
		t.Errorf("the cycle that folded the stub emitted %+v, want nothing", evts)
	}
	if got, err := fx.stores.Entities.GetSystem(ctx, org, stub.ID); err != nil || got != nil {
		t.Errorf("stub after the cycle = %+v (err %v), want it folded away", got, err)
	}

	out, err := fx.stores.Repos.RenameSystem(ctx, org, domain.RepoRef{
		Source: domain.RepoSourceGitHub, Host: ghOldHost, Owner: "octo", Repo: "new", ExternalID: trackedRepositoryID,
	})
	if err != nil || !out.Renamed {
		t.Fatalf("repository rename = %+v, %v; want it applied", out, err)
	}
	after := fx.entity(t, ghOldHost, "octo/new#5")
	if after == nil || after.ID != entity.ID || after.ExternalID != "PR_5" {
		t.Fatalf("octo/new#5 = %+v, want entity %s renamed onto it, still carrying PR_5", after, entity.ID)
	}
	if links := memoryLinks(t, fx.database, conversation); !slices.Equal(links, []string{entity.ID}) {
		t.Errorf("the run's memory links name %v, want the entity %s alone", links, entity.ID)
	}
	if active, err := fx.stores.Entities.ListActiveSystem(ctx, org, "github"); err != nil || len(active) != 1 {
		t.Errorf("active github entities = %d (err %v), want 1", len(active), err)
	}
}

// touchPR does what an agent's read touch on sourceID does when the read
// returns no node id: find or create the entity by its key alone, and link
// the conversation to it.
func (fx *githubScopeFixture) touchPR(t *testing.T, conversationID, sourceID string) *domain.Entity {
	t.Helper()
	ctx := context.Background()
	e, _, err := fx.stores.Entities.FindOrCreateSystem(ctx, runmode.LocalDefaultOrgID, "github", ghOldHost, sourceID, "", "pr", "", "")
	if err != nil {
		t.Fatalf("touch %s: %v", sourceID, err)
	}
	if err := fx.stores.TaskMemory.RecordEntityTouchSystem(ctx, runmode.LocalDefaultOrgID, conversationID, e.ID, domain.MemoryRoleTouched); err != nil {
		t.Fatalf("record the touch on %s: %v", sourceID, err)
	}
	return e
}

// seedTouchConversation inserts a conversation on a task of entityID: the run
// whose touches a test records.
func seedTouchConversation(t *testing.T, database *sql.DB, entityID string) string {
	t.Helper()
	eventID, taskID, runID, conversationID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	blueprintID := "bp_" + taskID
	stepPlan, err := domain.MarshalStepPlan([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: "p_touch", PromptName: "touch", PromptBody: "x", Source: "user"},
	})
	if err != nil {
		t.Fatalf("marshal step plan: %v", err)
	}
	for _, s := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`,
			[]any{eventID, entityID, domain.EventGitHubPROpened}},
		{`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_touch', 'Touch', 'body', ?, ?)`,
			[]any{runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID}},
		{`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES (?, ?, ?, ?, 'done')`,
			[]any{taskID, entityID, domain.EventGitHubPROpened, eventID}},
		{`INSERT INTO blueprints (id, name, source, creator_user_id, team_id) VALUES (?, 'bp', 'user', ?, ?)`,
			[]any{blueprintID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID}},
		{`INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		  VALUES (?, ?, ?, 'manual', 'running', ?, ?)`,
			[]any{runID, blueprintID, taskID, "/tmp/wt-" + runID, stepPlan}},
		{`INSERT INTO conversations (id, task_id, prompt_id, status, blueprint_run_id, blueprint_step_index, completed_at)
		  VALUES (?, ?, 'p_touch', 'open', ?, 0, CURRENT_TIMESTAMP)`,
			[]any{conversationID, taskID, runID}},
	} {
		if _, err := database.Exec(s.query, s.args...); err != nil {
			t.Fatalf("seed the touching conversation: %v\n%s", err, s.query)
		}
	}
	return conversationID
}

// memoryLinks returns the entities a conversation's memory links name.
func memoryLinks(t *testing.T, database *sql.DB, conversationID string) []string {
	t.Helper()
	rows, err := database.Query(`SELECT entity_id FROM conversation_memory_entities WHERE conversation_id = ? ORDER BY entity_id`, conversationID)
	if err != nil {
		t.Fatalf("read memory links: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan memory link: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read memory links: %v", err)
	}
	return out
}
