package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// GitHub identities in routing: an event that names its actor by GitHub's id
// resolves the actor by that id, so a user or team renamed since the TF
// binding or mapping was saved still routes, and a login GitHub has handed to
// another account does not route to the first account's user.

// bindGitHubID re-binds userID's identity on the review host to login with
// GitHub's id githubUserID.
func bindGitHubID(t *testing.T, database *sql.DB, userID, login, githubUserID string) {
	t.Helper()
	if err := sqlitestore.New(database).Users.UpsertGitHubIdentity(context.Background(), userID, reviewTestHost, login, githubUserID, "", "pat"); err != nil {
		t.Fatalf("bind identity %s: %v", login, err)
	}
}

func activeTasks(t *testing.T, database *sql.DB, entityID string) []domain.Task {
	t.Helper()
	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	return active
}

func emitGitHubEvent(router *Router, entityID, eventType, dedupKey string, meta any) {
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(context.Background(), domain.Event{
		EventType:    eventType,
		EntityID:     &entityID,
		DedupKey:     dedupKey,
		MetadataJSON: string(metaJSON),
		OccurredAt:   time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
}

func TestReviewRequested_RoutesTheRequestedUserByGitHubID(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamAlice := seedTeam(t, database, "alice-team")
	alice := seedUserOnTeam(t, database, teamAlice, "alice")
	bindGitHubID(t, database, alice, "alice", "5")
	// Another account was given alice's old login and is bound to bob.
	teamBob := seedTeam(t, database, "bob-team")
	bob := seedUserOnTeam(t, database, teamBob, "bob")
	bindGitHubID(t, database, bob, "alice-renamed", "9")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#byid")
	emitReviewRequested(reviewRouter(database), entityID, events.ReviewerDedupKeyUserID("5"), events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 3, RequestedLogin: "alice-renamed", RequestedUserID: "5",
	})

	active := activeTasks(t, database, entityID)
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamAlice {
		t.Errorf("visibility = %v, want [%s] — the account the id names, not the one holding the login", vis, teamAlice)
	}
}

func TestReviewRequested_RoutesTheRequestedTeamByGitHubID(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamBackend := seedTeam(t, database, "backend-tf")
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), teamBackend, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("map github team: %v", err)
	}
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	// GitHub renamed the organization and the team since the mapping was saved.
	entityID := reviewEntity(t, database, "owner/repo#teamid")
	emitReviewRequested(reviewRouter(database), entityID, events.ReviewerDedupKeyTeamID("11"), events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 3, RequestedTeam: "acme-corp/server", RequestedTeamID: "11",
	})

	active := activeTasks(t, database, entityID)
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d", len(active))
	}
	if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != teamBackend {
		t.Errorf("visibility = %v, want [%s] (the team mapped under GitHub's id)", vis, teamBackend)
	}
}

func TestAuthorCentric_AuthorResolvedByGitHubID(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	teamA := seedTeam(t, database, "team-a")
	aidan := seedUserOnTeam(t, database, teamA, "aidan")
	bindGitHubID(t, database, aidan, "aidan", "583231")
	seedSystemCIRule(t, database, teamA)

	// The author renamed themselves after binding.
	entityID := reviewEntity(t, database, "owner/repo#authorid")
	emitGitHubEvent(reviewRouter(database), entityID, domain.EventGitHubPRCICheckFailed, "build", events.GitHubPRCICheckFailedMetadata{
		Author: "aidan-renamed", AuthorID: "583231", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123",
	})

	active := activeTasks(t, database, entityID)
	if len(active) != 1 || teamIDValue(&active[0]) != teamA {
		t.Fatalf("tasks = %+v, want one owned by %s", active, teamA)
	}
}

// The transition: a review_requested task created before ids were recorded is
// keyed by the reviewer's login. The poll after ids were recorded reads the
// removal with the reviewer's id, keys it by the id, and names the handle
// beside it; that closes the old task, and only that one.
func TestReviewRequestRemoved_ByGitHubIDClosesATaskKeyedByLogin(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	seedUserOnTeam(t, database, seedTeam(t, database, "alice-team"), "alice")
	seedUserOnTeam(t, database, seedTeam(t, database, "bob-team"), "bob")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#transition")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, "user:alice", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "alice"})
	emitReviewRequested(router, entityID, "user:bob", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "bob"})

	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewRequestRemoved, events.ReviewerDedupKeyUserID("5"),
		events.GitHubPRReviewRequestRemovedMetadata{
			Author: "carol", Repo: "owner/repo", PRNumber: 6, RequestedLogin: "alice", RequestedUserID: "5",
		})

	active := activeTasks(t, database, entityID)
	if len(active) != 1 || active[0].DedupKey != "user:bob" {
		t.Fatalf("surviving tasks = %+v, want bob's alone", active)
	}
}

// A submitted review closes its reviewer's request whether the request's task
// is keyed by the reviewer's GitHub id or, created before ids were, by login.
func TestReviewByReviewer_ClosesTheirTaskKeyedEitherWay(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)

	alice := seedUserOnTeam(t, database, seedTeam(t, database, "alice-team"), "alice")
	bindGitHubID(t, database, alice, "alice", "5")
	seedUserOnTeam(t, database, seedTeam(t, database, "bob-team"), "bob")
	seedUserOnTeam(t, database, seedTeam(t, database, "dave-team"), "dave")
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#either")
	router := reviewRouter(database)
	emitReviewRequested(router, entityID, events.ReviewerDedupKeyUserID("5"), events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "alice", RequestedUserID: "5"})
	emitReviewRequested(router, entityID, "user:bob", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "bob"})
	emitReviewRequested(router, entityID, "user:dave", events.GitHubPRReviewRequestedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "dave"})

	// Alice reviews under a new login; bob under his, now with his id.
	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewApproved, "", events.GitHubPRReviewApprovedMetadata{
		Author: "carol", Reviewer: "alice-renamed", ReviewerID: "5", Repo: "owner/repo", PRNumber: 7})
	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewCommented, "", events.GitHubPRReviewCommentedMetadata{
		Author: "carol", Reviewer: "bob", ReviewerID: "6", Repo: "owner/repo", PRNumber: 7})

	active := activeTasks(t, database, entityID)
	if len(active) != 1 || active[0].DedupKey != "user:dave" {
		t.Fatalf("surviving tasks = %+v, want dave's alone", active)
	}
}
