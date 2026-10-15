package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedMentionRule seeds a match-all mentioned rule owned by teamID, so task
// creation is gated open without deciding visibility.
func seedMentionRule(t *testing.T, database *sql.DB, teamID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source, name, default_priority, sort_order,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'rule', ?, NULL, 1, 'user', 'mention rule', 0.7, 100, ?, ?)
	`, uuid.New().String(), runmode.LocalDefaultOrgID, teamID, runmode.LocalDefaultUserID,
		domain.EventGitHubPRMentioned, time.Now(), time.Now()); err != nil {
		t.Fatalf("seed mention rule: %v", err)
	}
}

func emitMentioned(router *Router, entityID, dedupKey string, meta events.GitHubPRMentionedMetadata) {
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventGitHubPRMentioned,
		EntityID:     &entityID,
		DedupKey:     dedupKey,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

// TestMentioned_RoutesToMentionedUserAndBumps: a mention task is visible to
// the mentioned user's team, not the PR author's or the rule's, and a second
// mention of the same user while it is open bumps it rather than opening
// another.
func TestMentioned_RoutesToMentionedUserAndBumps(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamAuthor := seedTeam(t, database, "author-team")
	teamBob := seedTeam(t, database, "bob-team")
	seedUserOnTeam(t, database, teamAuthor, "carol")
	seedUserOnTeam(t, database, teamBob, "bob")
	seedMentionRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#mention")
	router := reviewRouter(database)
	meta := events.GitHubPRMentionedMetadata{
		Author: "carol", Commenter: "dave", CommentID: 11, MentionedLogin: "bob",
		MentionSource: events.MentionSourceComment, Repo: "owner/repo", PRNumber: 5,
	}
	emitMentioned(router, entityID, "user:bob", meta)
	meta.CommentID = 12
	emitMentioned(router, entityID, "user:bob", meta)

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	task := active[0]
	if task.EventType != domain.EventGitHubPRMentioned || task.DedupKey != "user:bob" {
		t.Errorf("task = %s keyed %q, want the mention keyed user:bob", task.EventType, task.DedupKey)
	}
	if task.TeamID != nil {
		t.Errorf("owner team_id = %v, want nil until someone claims it", *task.TeamID)
	}
	if vis := visTeamsOf(t, database, task.ID); len(vis) != 1 || vis[0] != teamBob {
		t.Errorf("visibility = %v, want [%s] (the mentioned user's team only)", vis, teamBob)
	}
	var bumped int
	if err := database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'bumped'`, task.ID).Scan(&bumped); err != nil {
		t.Fatalf("count bumps: %v", err)
	}
	if bumped != 1 {
		t.Errorf("bumped task_events = %d, want 1 for the second mention", bumped)
	}
}

// TestMentioned_TeamRoutesToMappedTFTeams: a team mention routes to the TF
// team(s) the github-team mapping returns.
func TestMentioned_TeamRoutesToMappedTFTeams(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamBackend := seedTeam(t, database, "backend-tf")
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), teamBackend, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("map github team: %v", err)
	}
	seedMentionRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#teammention")
	emitMentioned(reviewRouter(database), entityID, "team:acme/backend", events.GitHubPRMentionedMetadata{
		Author: "carol", MentionedTeam: "acme/backend", MentionSource: events.MentionSourceBody, Repo: "owner/repo", PRNumber: 6,
	})

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 task, got %d (err=%v)", len(active), err)
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamBackend {
		t.Errorf("visibility = %v, want [%s] (the mapped TF team)", vis, teamBackend)
	}
}

// TestMentioned_RoutedAsRequestedParty pins the ownership declaration the two
// tests above rest on, and that a merge or close still retires a mention task.
func TestMentioned_RoutedAsRequestedParty(t *testing.T) {
	if got := ownershipModelForEvent(domain.EventGitHubPRMentioned); got != events.OwnershipRequestedParty {
		t.Errorf("ownership = %v, want OwnershipRequestedParty", got)
	}
	if isAuthorCentricGitHubEvent(domain.EventGitHubPRMentioned) || EventSupportsWatch(domain.EventGitHubPRMentioned) {
		t.Error("mentioned routes by the PR's owner, want by the mentioned identity")
	}
	found := false
	for _, et := range githubPRTerminalCloseTypes() {
		found = found || et == domain.EventGitHubPRMentioned
	}
	if !found {
		t.Error("a merge or close does not retire mention tasks")
	}
}
