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

// An event that names its actor by GitHub's id routes by that id, so a user or
// team renamed since the binding or mapping was saved still routes, and a
// login GitHub has handed to another account does not route to the first
// account's user.

// idRoutingFixture is a database with a match-all review_requested rule and
// the router over it.
func idRoutingFixture(t *testing.T) (*sql.DB, *Router) {
	t.Helper()
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedReviewRule(t, database, runmode.LocalDefaultTeamID)
	return database, reviewRouter(database)
}

// bindGitHubID re-binds userID's identity on the review host to login with
// GitHub's id githubUserID.
func bindGitHubID(t *testing.T, database *sql.DB, userID, login, githubUserID string) {
	t.Helper()
	if err := sqlitestore.New(database).Users.UpsertGitHubIdentity(context.Background(), userID, reviewTestHost, login, githubUserID, "", "pat"); err != nil {
		t.Fatalf("bind identity %s: %v", login, err)
	}
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

func TestReviewRequested_RoutesTheReviewerByGitHubID(t *testing.T) {
	database, router := idRoutingFixture(t)
	teamAlice := seedTeam(t, database, "alice-team")
	bindGitHubID(t, database, seedUserOnTeam(t, database, teamAlice, "alice"), "alice", "5")
	// Bob's binding holds the login the request carries, under another account's id.
	bindGitHubID(t, database, seedUserOnTeam(t, database, seedTeam(t, database, "bob-team"), "bob"), "alice-renamed", "9")
	teamBackend := seedTeam(t, database, "backend-tf")
	if err := sqlitestore.New(database).TeamGitHubGroups.SetForTeam(context.Background(), teamBackend, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		{GitHubTeamID: "11", GitHubOrgID: "9", OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("map github team: %v", err)
	}

	for _, c := range []struct {
		name, key string
		meta      events.GitHubPRReviewRequestedMetadata
		want      string
	}{
		{"user", events.ReviewerDedupKeyUserID("5"), events.GitHubPRReviewRequestedMetadata{RequestedLogin: "alice-renamed", RequestedUserID: "5"}, teamAlice},
		// GitHub renamed the organization and the team since the mapping was saved.
		{"team", events.ReviewerDedupKeyTeamID("11"), events.GitHubPRReviewRequestedMetadata{RequestedTeam: "acme-corp/server", RequestedTeamID: "11"}, teamBackend},
	} {
		entityID := reviewEntity(t, database, "owner/repo#"+c.name)
		c.meta.Author, c.meta.Repo, c.meta.PRNumber = "carol", "owner/repo", 3
		emitReviewRequested(router, entityID, c.key, c.meta)

		active := activeTasksOfType(t, database, entityID, domain.EventGitHubPRReviewRequested)
		if len(active) != 1 {
			t.Fatalf("%s: expected 1 task, got %d", c.name, len(active))
		}
		if vis := visTeamsOf(t, database, active[0].ID); len(vis) != 1 || vis[0] != c.want {
			t.Errorf("%s: visibility = %v, want [%s]", c.name, vis, c.want)
		}
	}
}

func TestAuthorCentric_AuthorResolvedByGitHubID(t *testing.T) {
	database, router := idRoutingFixture(t)
	teamA := seedTeam(t, database, "team-a")
	bindGitHubID(t, database, seedUserOnTeam(t, database, teamA, "aidan"), "aidan", "583231")
	seedSystemCIRule(t, database, teamA)

	entityID := reviewEntity(t, database, "owner/repo#authorid")
	emitGitHubEvent(router, entityID, domain.EventGitHubPRCICheckFailed, "build", events.GitHubPRCICheckFailedMetadata{
		Author: "aidan-renamed", AuthorID: "583231", CheckName: "build", Repo: "owner/repo", HeadSHA: "abc123",
	})

	active := activeTasksOfType(t, database, entityID, domain.EventGitHubPRCICheckFailed)
	if len(active) != 1 || teamIDValue(&active[0]) != teamA {
		t.Fatalf("tasks = %+v, want one owned by %s", active, teamA)
	}
}

// A removal or a submitted review read with the reviewer's GitHub id closes
// their request's task whether it is keyed by that id or, created before ids
// were recorded, by login.
func TestReviewRequestClosers_MatchATaskKeyedEitherWay(t *testing.T) {
	database, router := idRoutingFixture(t)
	bindGitHubID(t, database, seedUserOnTeam(t, database, seedTeam(t, database, "alice-team"), "alice"), "alice", "5")
	for _, login := range []string{"bob", "dave", "erin"} {
		seedUserOnTeam(t, database, seedTeam(t, database, login+"-team"), login)
	}
	entityID := reviewEntity(t, database, "owner/repo#either")
	request := func(key, login, id string) {
		emitReviewRequested(router, entityID, key, events.GitHubPRReviewRequestedMetadata{
			Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: login, RequestedUserID: id})
	}
	request(events.ReviewerDedupKeyUserID("5"), "alice", "5")
	request("user:bob", "bob", "")
	request("user:dave", "dave", "")
	request("user:erin", "erin", "")
	if n := len(activeTasksOfType(t, database, entityID, domain.EventGitHubPRReviewRequested)); n != 4 {
		t.Fatalf("requests opened %d tasks, want 4", n)
	}

	// Alice reviews under a new login, bob under his own now with his id, and
	// erin's request is removed by her id.
	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewApproved, "", events.GitHubPRReviewApprovedMetadata{
		Author: "carol", Reviewer: "alice-renamed", ReviewerID: "5", Repo: "owner/repo", PRNumber: 7})
	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewCommented, "", events.GitHubPRReviewCommentedMetadata{
		Author: "carol", Reviewer: "bob", ReviewerID: "6", Repo: "owner/repo", PRNumber: 7})
	emitGitHubEvent(router, entityID, domain.EventGitHubPRReviewRequestRemoved, events.ReviewerDedupKeyUserID("8"), events.GitHubPRReviewRequestRemovedMetadata{
		Author: "carol", Repo: "owner/repo", PRNumber: 7, RequestedLogin: "erin", RequestedUserID: "8"})

	active := activeTasksOfType(t, database, entityID, domain.EventGitHubPRReviewRequested)
	if len(active) != 1 || active[0].DedupKey != "user:dave" {
		t.Fatalf("surviving tasks = %+v, want dave's alone", active)
	}
}
